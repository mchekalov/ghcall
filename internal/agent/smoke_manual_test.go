//go:build manual

package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ghcall/internal/config"
	"ghcall/internal/pipeline"
	"ghcall/internal/vcs"
)

// TestSmokeRealDocker exercises Run against the real docker binary (no
// execCommandContext override) to prove the wiring genuinely shells out and
// works end-to-end, not just against the faked exec in trigger_test.go.
// Requires a local docker daemon and the alpine:latest image; run manually
// with: go test ./internal/agent/... -tags manual -run TestSmokeRealDocker -v
func TestSmokeRealDocker(t *testing.T) {
	var results []pipeline.FilterResult
	for i := 1; i <= 3; i++ {
		results = append(results, pipeline.FilterResult{
			Provider: vcs.GitHub,
			Repo:     fmt.Sprintf("smoke/repo%d", i),
			PR:       pipeline.PullRequestResult{Number: i, CIState: "FAILURE"},
		})
	}
	cfg := config.AgentConfig{
		Image:             "alpine:latest",
		MaxConcurrentRuns: 2,
		PerRunTimeoutSec:  10,
	}

	start := time.Now()
	out := Run(context.Background(), results, cfg, []string{"SMOKE_ENV=1"})
	t.Logf("Run took %s", time.Since(start))

	if len(out) != 3 {
		t.Fatalf("got %d results, want 3", len(out))
	}
	for _, r := range out {
		t.Logf("repo=%s pr=%d err=%v output=%q", r.Repo, r.Number, r.Err, r.Output)
	}
}
