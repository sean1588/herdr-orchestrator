package github

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
	"github.com/sean1588/herdr-orchestrator/internal/source"
)

func TestIssueSourceAdapter(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		switch c.Args[1] {
		case "list":
			return []byte(`[{"number":12}]`), nil
		case "view":
			return []byte(`{"number":12,"title":"Title","body":"Body"}`), nil
		}
		return nil, nil
	}}
	s := IssueSource{Client: New(f), RepoDir: "/issues-repo"}
	ctx := context.Background()
	sel := source.Selector{"label": "ready"}
	keys, err := s.List(ctx, sel)
	if err != nil || !reflect.DeepEqual(keys, []string{"12"}) {
		t.Fatalf("list=%v %v", keys, err)
	}
	item, err := s.Get(ctx, "12")
	if err != nil || item.Key != "12" || item.Title != "Title" || item.Body != "Body" {
		t.Fatalf("get=%+v %v", item, err)
	}
	if err := s.Acknowledge(ctx, "12", sel); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, "12", "implemented"); err != nil {
		t.Fatal(err)
	}
	calls := f.Snapshot()
	for _, c := range calls {
		if c.Dir != "/issues-repo" || c.Name != "gh" || c.Args[0] != "issue" {
			t.Fatalf("misrouted call %+v", c)
		}
	}
	if !reflect.DeepEqual(calls[2].Args, []string{"issue", "edit", "12", "--remove-label", "ready"}) {
		t.Fatalf("ack call %+v", calls[2])
	}
}

func TestIssueSourceRejectsNonNumericKeysBeforeIO(t *testing.T) {
	for _, key := range []string{"uuid", "0", "-2", "01", "+1", "12\n"} {
		t.Run(key, func(t *testing.T) {
			f := &proc.Fake{}
			s := IssueSource{Client: New(f)}
			ctx := context.Background()
			if _, err := s.Get(ctx, key); err == nil {
				t.Fatal("Get accepted key")
			}
			if err := s.Complete(ctx, key, ""); err == nil {
				t.Fatal("Complete accepted key")
			}
			if err := s.Acknowledge(ctx, key, nil); err == nil {
				t.Fatal("Acknowledge accepted key")
			}
			if len(f.Snapshot()) != 0 {
				t.Fatal("invalid key reached gh")
			}
		})
	}
}

func TestIssueSourceSelectorAndErrors(t *testing.T) {
	sentinel := errors.New("offline")
	f := &proc.Fake{Responder: func(proc.Call) ([]byte, error) { return nil, sentinel }}
	s := IssueSource{Client: New(f)}
	ctx := context.Background()
	for _, selector := range []source.Selector{nil, {"label": 5}, {"label": ""}} {
		if _, err := s.List(ctx, selector); err == nil {
			t.Fatal("accepted invalid discovery selector")
		}
	}
	if len(f.Snapshot()) != 0 {
		t.Fatal("invalid discovery called gh")
	}
	if err := s.Acknowledge(ctx, "1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "1"); !errors.Is(err, sentinel) {
		t.Fatalf("error not preserved: %v", err)
	}
}
