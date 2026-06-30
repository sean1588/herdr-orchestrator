package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNop_Notify_ReturnsNil(t *testing.T) {
	if err := (Nop{}).Notify(context.Background(), Event{TaskID: "issue-1", Kind: "escalated"}); err != nil {
		t.Errorf("Nop.Notify = %v, want nil", err)
	}
}

func TestWebhook_Notify_PostsJSON(t *testing.T) {
	type received struct {
		ev          Event
		method      string
		contentType string
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("decode body: %v", err)
		}
		got <- received{ev: ev, method: r.Method, contentType: r.Header.Get("Content-Type")}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := Webhook{URL: srv.URL}
	want := Event{TaskID: "issue-7", Issue: 7, State: "escalated", Kind: "escalated", Detail: "needs_input"}
	if err := wh.Notify(context.Background(), want); err != nil {
		t.Fatalf("Notify = %v, want nil", err)
	}

	select {
	case r := <-got:
		if r.method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.method)
		}
		if r.contentType != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.contentType)
		}
		if r.ev.TaskID != want.TaskID {
			t.Errorf("body TaskID = %q, want %q", r.ev.TaskID, want.TaskID)
		}
		if r.ev.Kind != want.Kind {
			t.Errorf("body Kind = %q, want %q", r.ev.Kind, want.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook server never received a request")
	}
}

func TestWebhook_Notify_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	wh := Webhook{URL: srv.URL}
	if err := wh.Notify(context.Background(), Event{TaskID: "issue-9", Kind: "alert"}); err == nil {
		t.Error("Notify on a 500 response = nil, want error")
	}
}
