package main

import (
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

const (
	goodFixture   = "../../internal/config/testdata/default-pipeline.yaml"
	brokenFixture = "../../internal/config/testdata/broken-pipeline.yaml"
)

func TestRun_Dispatch(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"unknown command", []string{"bogus"}, 2},
		{"help", []string{"--help"}, 0},
		{"validate good", []string{"validate", goodFixture}, 0},
		{"validate broken", []string{"validate", brokenFixture}, 1},
		{"validate missing arg", []string{"validate"}, 2},
		{"run missing flags", []string{"run"}, 2},
		{"run missing issue", []string{"run", "--config", goodFixture, "--repo", "."}, 2},
		{"recover missing flags", []string{"recover"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestRepoSlug(t *testing.T) {
	wf, _, err := config.Load(goodFixture)
	if err != nil {
		t.Fatal(err)
	}
	if got := repoSlug(wf); got != "sean1588/minicode" {
		t.Errorf("repoSlug = %q, want sean1588/minicode", got)
	}
}
