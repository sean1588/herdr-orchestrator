package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The page is one file. Links it renders to GitHub are built in script, so the
// markup itself must not name an external stylesheet, script, or import.
func TestEmbeddedPageLoadsNothing(t *testing.T) {
	text := string(indexHTML)
	if !strings.Contains(text, "list_tasks") {
		t.Fatal("embedded index.html is not the dashboard")
	}
	forbidden := []string{
		`src="http`,
		`src='http`,
		`href="http`,
		`href='http`,
		`@import`,
	}
	for _, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Errorf("index.html contains %q", needle)
		}
	}
}

func TestHandler(t *testing.T) {
	h := Handler()
	tests := []struct {
		name     string
		method   string
		path     string
		wantCode int
		wantType string
		wantCSP  string
		wantBody string
	}{
		{
			name: "slash", method: http.MethodGet, path: "/ui/",
			wantCode: http.StatusOK, wantType: "text/html",
			wantCSP: "default-src 'self'", wantBody: "list_tasks",
		},
		{
			name: "no slash", method: http.MethodGet, path: "/ui",
			wantCode: http.StatusOK, wantType: "text/html",
			wantCSP: "default-src 'self'", wantBody: "list_tasks",
		},
		{name: "post", method: http.MethodPost, path: "/ui/", wantCode: http.StatusMethodNotAllowed},
		{name: "other path", method: http.MethodGet, path: "/ui/extra", wantCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantType != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.wantType) {
				t.Fatalf("Content-Type = %q, want prefix %q", rec.Header().Get("Content-Type"), tt.wantType)
			}
			if tt.wantCSP != "" && !strings.Contains(rec.Header().Get("Content-Security-Policy"), tt.wantCSP) {
				t.Fatalf("Content-Security-Policy = %q, want substring %q", rec.Header().Get("Content-Security-Policy"), tt.wantCSP)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body missing %q", tt.wantBody)
			}
		})
	}
}
