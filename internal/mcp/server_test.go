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
	"github.com/sean1588/herdr-orchestrator/internal/ui"
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

func TestServeRejectsNonPost(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(testServer().handleHTTP))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
}

// TestServeUI hits the mux Serve runs. With a UI handler, GET /ui/ is the
// dashboard; without one it stays 405. POST /mcp is the same either way.
func TestServeUI(t *testing.T) {
	const listTasks = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`
	tests := []struct {
		name     string
		ui       string // "on" wires the dashboard, "nil" passes a nil handler, else off
		method   string
		path     string
		body     string
		wantCode int
		wantType string
		wantCSP  string
		wantBody string
	}{
		{
			name: "dashboard", ui: "on", method: http.MethodGet, path: "/ui/",
			wantCode: http.StatusOK, wantType: "text/html",
			wantCSP: "default-src 'self'", wantBody: "list_tasks",
		},
		{
			name: "dashboard no slash", ui: "on", method: http.MethodGet, path: "/ui",
			wantCode: http.StatusOK, wantType: "text/html",
			wantCSP: "default-src 'self'", wantBody: "list_tasks",
		},
		{
			name: "no ui", method: http.MethodGet, path: "/ui/",
			wantCode: http.StatusMethodNotAllowed,
		},
		{
			name: "nil ui", ui: "nil", method: http.MethodGet, path: "/ui/",
			wantCode: http.StatusMethodNotAllowed,
		},
		{
			name: "mcp with ui", ui: "on", method: http.MethodPost, path: "/mcp",
			body: listTasks, wantCode: http.StatusOK, wantBody: `"result"`,
		},
		{
			name: "mcp without ui", method: http.MethodPost, path: "/mcp",
			body: listTasks, wantCode: http.StatusOK, wantBody: `"result"`,
		},
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer()
			switch tt.ui {
			case "on":
				s.WithUI(ui.Handler())
			case "nil":
				s.WithUI(nil)
			}
			ts := httptest.NewServer(s.mux())
			t.Cleanup(ts.Close)

			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}
			req, err := http.NewRequest(tt.method, ts.URL+tt.path, body)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantCode)
			}
			if tt.wantType != "" && !strings.HasPrefix(resp.Header.Get("Content-Type"), tt.wantType) {
				t.Fatalf("Content-Type = %q, want prefix %q", resp.Header.Get("Content-Type"), tt.wantType)
			}
			if tt.wantCSP != "" && !strings.Contains(resp.Header.Get("Content-Security-Policy"), tt.wantCSP) {
				t.Fatalf("Content-Security-Policy = %q, want substring %q", resp.Header.Get("Content-Security-Policy"), tt.wantCSP)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantBody != "" && !strings.Contains(string(got), tt.wantBody) {
				t.Fatalf("body missing %q: %s", tt.wantBody, got)
			}
		})
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
