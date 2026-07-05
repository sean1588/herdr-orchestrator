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

// The nil-Client fallback must carry a request timeout: the daemon ctx has no
// deadline, so an unbounded client would let a hung endpoint pin the drive loop.
func TestWebhook_NilClient_IsBounded(t *testing.T) {
	if defaultClient.Timeout <= 0 {
		t.Fatalf("defaultClient.Timeout = %v; want > 0 (an unbounded nil-Client fallback can block the drive loop)", defaultClient.Timeout)
	}
}

// A nil-Client webhook against a hung endpoint, with a deadline-free context,
// must still return (an error) rather than block forever.
func TestWebhook_NilClient_TimesOutOnHang(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block // never respond until the test tears down
	}))
	defer srv.Close()
	defer close(block)

	// Shrink the shared fallback so the test doesn't wait the real timeout.
	orig := defaultClient
	defaultClient = &http.Client{Timeout: 100 * time.Millisecond}
	defer func() { defaultClient = orig }()

	wh := Webhook{URL: srv.URL} // nil Client => must use the bounded fallback
	done := make(chan error, 1)
	go func() { done <- wh.Notify(context.Background(), Event{Kind: "escalated"}) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Notify against a hung endpoint = nil, want a timeout error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Notify did not return: the nil-Client webhook is unbounded and would block the drive loop")
	}
}
