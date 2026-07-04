package mcp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

func testServer() *Server {
	return New(fakeReader{tasks: map[string]store.Task{}}, &fakeController{},
		func(i int) string { return "issue-0" }, nil)
}

func TestServeToolsCall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(testServer().handleHTTP))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"result"`) {
		t.Fatalf("no result in response: %s", body)
	}
}

func TestServeParseError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(testServer().handleHTTP))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "-32700") {
		t.Fatalf("want parse error, got: %s", body)
	}
}

func TestServeNotification(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(testServer().handleHTTP))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("notification should return no body, got: %s", body)
	}
}

// Serve runs on a real listener and returns nil once its context is cancelled.
func TestServeLifecycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testServer().Serve(ctx, ln) }()

	// The endpoint answers while serving.
	url := "http://" + ln.Addr().String() + "/mcp"
	resp, err := http.Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("request while serving: %v", err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}
