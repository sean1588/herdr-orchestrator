//go:build live

// The live fixture test calls the real model through OpenRouter, so it is off by
// default. Run it with:
//
//	OPENROUTER_API_KEY=... go test -tags live -run TestLive ./internal/classify/ -v
package classify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const liveURL = "https://openrouter.ai/api/v1/systemone"

// Every hand-written fixture must classify to the label in its file name
// (<label>-<n>.txt) at p >= Threshold.
func TestLive_FixturesClassifyToTheirLabel(t *testing.T) {
	if os.Getenv(KeyEnv) == "" {
		t.Fatalf("%s must be set for the live test", KeyEnv)
	}
	paths, err := filepath.Glob("testdata/panes/*.txt")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	seen := map[Activity]int{}
	total := 0.0
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".txt")
		want := Activity(name[:strings.LastIndex(name, "-")])
		seen[want]++
		t.Run(name, func(t *testing.T) {
			tail, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			got, cost, err := Jev{URL: liveURL}.classify(context.Background(), string(tail))
			if err != nil {
				t.Fatal(err)
			}
			total += cost
			t.Logf("%s: %s p=%.2f cost=$%.8f", name, got.Activity, got.Confidence, cost)
			if got.Activity != want || !got.Confident() {
				t.Errorf("got %s at p=%.2f, want %s at p >= %.2f", got.Activity, got.Confidence, want, Threshold)
			}
		})
	}
	for a := range criteria {
		if seen[a] < 2 {
			t.Errorf("label %s has %d fixtures, want at least 2", a, seen[a])
		}
	}
	t.Logf("total cost for %d calls: $%.8f", len(paths), total)
}
