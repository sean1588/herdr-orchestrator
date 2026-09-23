package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
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

func TestIssueSourceListsOnlyTheFrontier(t *testing.T) {
	// blocker renders one blockedBy node in the verified gh shape.
	blocker := func(n int, state string) string {
		return fmt.Sprintf(`{"id":"I_kwFIXTURE%d","number":%d,"state":%q,"title":"Synthetic","url":"https://github.com/example/repo/issues/%d"}`, n, n, state, n)
	}
	issue := func(n int, blockers ...string) string {
		return fmt.Sprintf(`{"number":%d,"blockedBy":{"nodes":[%s],"totalCount":%d}}`, n, strings.Join(blockers, ","), len(blockers))
	}
	type held struct {
		Issue    int   `json:"issue"`
		Blockers []int `json:"blockers"`
	}
	for _, tc := range []struct {
		name     string
		issues   []string
		wantKeys []string
		wantHeld []held
	}{
		{"no blockers", []string{issue(1)}, []string{"1"}, nil},
		{"all blockers closed", []string{issue(2, blocker(1, "CLOSED"), blocker(3, "CLOSED"))}, []string{"2"}, nil},
		{"open blocker holds", []string{issue(72, blocker(71, "OPEN"))}, []string{}, []held{{72, []int{71}}}},
		{"mixed holds on the open one", []string{issue(9, blocker(7, "CLOSED"), blocker(8, "OPEN"))}, []string{}, []held{{9, []int{8}}}},
		{
			"frontier of a chain",
			[]string{issue(10), issue(11, blocker(10, "OPEN")), issue(12, blocker(10, "OPEN"), blocker(11, "OPEN"))},
			[]string{"10"},
			[]held{{11, []int{10}}, {12, []int{10, 11}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := "[" + strings.Join(tc.issues, ",") + "]"
			f := &proc.Fake{Responder: func(proc.Call) ([]byte, error) { return []byte(out), nil }}
			var buf bytes.Buffer
			s := IssueSource{Client: New(f), Log: slog.New(slog.NewJSONHandler(&buf, nil))}
			keys, err := s.List(context.Background(), source.Selector{"label": "ready"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(keys, tc.wantKeys) {
				t.Fatalf("keys = %v, want %v", keys, tc.wantKeys)
			}
			var got []held
			dec := json.NewDecoder(&buf)
			for dec.More() {
				var rec struct {
					Level string `json:"level"`
					Msg   string `json:"msg"`
					held
				}
				if err := dec.Decode(&rec); err != nil {
					t.Fatal(err)
				}
				if rec.Level != "INFO" || rec.Msg != "issue waiting on open blockers" {
					t.Fatalf("unexpected record %+v", rec)
				}
				got = append(got, rec.held)
			}
			if !reflect.DeepEqual(got, tc.wantHeld) {
				t.Fatalf("held records = %+v, want %+v", got, tc.wantHeld)
			}

			// A nil logger filters identically and stays silent.
			s.Log = nil
			if keys, err := s.List(context.Background(), source.Selector{"label": "ready"}); err != nil || !reflect.DeepEqual(keys, tc.wantKeys) {
				t.Fatalf("nil logger: keys = %v %v, want %v", keys, err, tc.wantKeys)
			}
		})
	}
}
