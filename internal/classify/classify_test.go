package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withKey(string) string { return "sk-test" }

func TestJev_Classify(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		want     Result
		wantErr  string
		getenv   func(string) string
		wantCall bool
	}{
		{
			name:   "choice and its probability",
			status: 200,
			body: `{"answers":{"activity":{"type":"choice","choice":"awaiting_permission",
				"probabilities":{"awaiting_permission":0.97,"working":0.02,"awaiting_answer":0.01},
				"confidence":0.96}},"usage":{"cost":0.00004}}`,
			want:     Result{Activity: AwaitingPermission, Confidence: 0.97},
			getenv:   withKey,
			wantCall: true,
		},
		{
			name:     "non-2xx is an error",
			status:   502,
			body:     `upstream down`,
			wantErr:  "status 502",
			getenv:   withKey,
			wantCall: true,
		},
		{
			name:     "missing activity answer is an error",
			status:   200,
			body:     `{"answers":{}}`,
			wantErr:  "no activity answer",
			getenv:   withKey,
			wantCall: true,
		},
		{
			name:     "label outside the declared set is an error",
			status:   200,
			body:     `{"answers":{"activity":{"choice":"sleeping","probabilities":{"sleeping":1}}}}`,
			wantErr:  "unknown activity",
			getenv:   withKey,
			wantCall: true,
		},
		{
			name:     "malformed JSON is an error",
			status:   200,
			body:     `{`,
			wantErr:  "decode response",
			getenv:   withKey,
			wantCall: true,
		},
		{
			name:     "no key: error, and no request is made",
			wantErr:  KeyEnv,
			getenv:   func(string) string { return "" },
			wantCall: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
					t.Errorf("Authorization = %q", got)
				}
				var req jevRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if req.Model != Model {
					t.Errorf("model = %q, want %q", req.Model, Model)
				}
				if req.State["pane_tail"] != "the tail" {
					t.Errorf("state.pane_tail = %q", req.State["pane_tail"])
				}
				if q := req.Questions["activity"]; q.Type != "choice" || len(q.Criteria) != 5 {
					t.Errorf("activity question = %+v, want a 5-way choice", q)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			got, err := Jev{URL: srv.URL, Getenv: tt.getenv}.Classify(context.Background(), "the tail")
			if called != tt.wantCall {
				t.Errorf("request made = %v, want %v", called, tt.wantCall)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResult_Confident(t *testing.T) {
	for _, tt := range []struct {
		p    float64
		want bool
	}{{0.99, true}, {Threshold, true}, {0.89, false}, {0, false}} {
		if got := (Result{Activity: Working, Confidence: tt.p}).Confident(); got != tt.want {
			t.Errorf("Confident(%v) = %v, want %v", tt.p, got, tt.want)
		}
	}
}

// The request timeout is what keeps a hung endpoint from pinning a drive worker
// (notify.Webhook's recorded defect). Assert it is set rather than waiting it out.
func TestJev_ClientIsBounded(t *testing.T) {
	if client.Timeout != RequestTimeout || RequestTimeout <= 0 {
		t.Fatalf("client timeout = %v, want %v (> 0)", client.Timeout, RequestTimeout)
	}
}
