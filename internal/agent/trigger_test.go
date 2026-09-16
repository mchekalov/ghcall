package agent

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"ghcall/internal/config"
	"ghcall/internal/pipeline"
)

func TestCandidates(t *testing.T) {
	results := []pipeline.FilterResult{
		{Repo: "o/a", PR: pipeline.PullRequestResult{Number: 1, CIState: "SUCCESS"}},
		{Repo: "o/b", PR: pipeline.PullRequestResult{Number: 2, CIState: "FAILURE"}},
		{Repo: "o/c", PR: pipeline.PullRequestResult{Number: 3, CIState: "ERROR"}},
		{Repo: "o/d", PR: pipeline.PullRequestResult{Number: 4, CIState: "PENDING"}},
		{Repo: "o/e", PR: pipeline.PullRequestResult{Number: 5, CIState: ""}},
	}

	got := Candidates(results)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(got), got)
	}
	if got[0].PR.Number != 2 || got[1].PR.Number != 3 {
		t.Fatalf("unexpected candidates: %+v", got)
	}
}

func TestRun_DisabledOrNoCandidates(t *testing.T) {
	failing := []pipeline.FilterResult{{Repo: "o/a", PR: pipeline.PullRequestResult{Number: 1, CIState: "FAILURE"}}}

	if out := Run(context.Background(), failing, config.AgentConfig{}, nil); out != nil {
		t.Fatalf("disabled agent config (no Image) should return nil, got %+v", out)
	}

	enabled := config.AgentConfig{Image: "img", MaxConcurrentRuns: 1, PerRunTimeoutSec: 5}
	green := []pipeline.FilterResult{{Repo: "o/a", PR: pipeline.PullRequestResult{Number: 1, CIState: "SUCCESS"}}}
	if out := Run(context.Background(), green, enabled, nil); out != nil {
		t.Fatalf("no failing-CI PRs should return nil, got %+v", out)
	}
}

func TestRun_InvokesDockerPerCandidateAndCapturesResult(t *testing.T) {
	orig := execCommandContext
	defer func() { execCommandContext = orig }()

	var mu sync.Mutex
	var recordedArgs [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		call := append([]string{name}, args...)
		recordedArgs = append(recordedArgs, call)
		mu.Unlock()

		// Simulate PR #2's container failing; everything else succeeds.
		script := "exit 0"
		for _, a := range args {
			if a == "2" {
				script = "exit 1"
			}
		}
		return exec.CommandContext(ctx, "sh", "-c", script)
	}

	results := []pipeline.FilterResult{
		{Repo: "o/a", PR: pipeline.PullRequestResult{Number: 1, CIState: "FAILURE"}},
		{Repo: "o/b", PR: pipeline.PullRequestResult{Number: 2, CIState: "ERROR"}},
		{Repo: "o/c", PR: pipeline.PullRequestResult{Number: 3, CIState: "SUCCESS"}}, // not a candidate
	}
	cfg := config.AgentConfig{Image: "autofix:latest", MaxConcurrentRuns: 2, PerRunTimeoutSec: 5}
	env := []string{"GITHUB_TOKEN=tok", "AWS_BEARER_TOKEN_BEDROCK=bed"}

	out := Run(context.Background(), results, cfg, env)

	if len(out) != 2 {
		t.Fatalf("got %d results, want 2 (only failing-CI PRs start a container): %+v", len(out), out)
	}
	byPR := map[int]RunResult{}
	for _, r := range out {
		byPR[r.Number] = r
	}
	if r, ok := byPR[1]; !ok || r.Err != nil {
		t.Errorf("PR 1 should have succeeded, got %+v", r)
	}
	if r, ok := byPR[2]; !ok || r.Err == nil {
		t.Errorf("PR 2 should have failed (non-zero exit), got %+v", r)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recordedArgs) != 2 {
		t.Fatalf("expected 2 docker invocations, got %d: %v", len(recordedArgs), recordedArgs)
	}
	for _, args := range recordedArgs {
		if args[0] != "docker" {
			t.Errorf("expected default docker_bin %q, got %q (%v)", "docker", args[0], args)
		}
		assertContainsSeq(t, args, "-e", "GITHUB_TOKEN=tok")
		assertContainsSeq(t, args, "-e", "AWS_BEARER_TOKEN_BEDROCK=bed")
		assertContains(t, args, "autofix:latest")
		assertContainsSeq(t, args, "--platform", "github")
	}
}

func TestRun_CustomDockerBin(t *testing.T) {
	orig := execCommandContext
	defer func() { execCommandContext = orig }()

	var gotName string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName = name
		return exec.CommandContext(ctx, "sh", "-c", "exit 0")
	}

	results := []pipeline.FilterResult{{Repo: "o/a", PR: pipeline.PullRequestResult{Number: 1, CIState: "FAILURE"}}}
	cfg := config.AgentConfig{Image: "img", DockerBin: "podman", MaxConcurrentRuns: 1, PerRunTimeoutSec: 5}

	Run(context.Background(), results, cfg, nil)

	if gotName != "podman" {
		t.Fatalf("expected configured docker_bin %q to be used, got %q", "podman", gotName)
	}
}

// TestRun_RespectsMaxConcurrentRuns forces every fake invocation to hold its
// "slot" for a short, fixed duration and records the high-water mark of
// concurrently-held slots, proving Run's worker pool never exceeds
// cfg.MaxConcurrentRuns regardless of how many candidates are queued.
func TestRun_RespectsMaxConcurrentRuns(t *testing.T) {
	orig := execCommandContext
	defer func() { execCommandContext = orig }()

	var mu sync.Mutex
	current, maxSeen := 0, 0
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		current++
		if current > maxSeen {
			maxSeen = current
		}
		mu.Unlock()

		time.Sleep(30 * time.Millisecond) // hold the slot long enough to force overlap

		mu.Lock()
		current--
		mu.Unlock()

		return exec.CommandContext(ctx, "sh", "-c", "exit 0")
	}

	var results []pipeline.FilterResult
	for i := 1; i <= 6; i++ {
		results = append(results, pipeline.FilterResult{
			Repo: fmt.Sprintf("o/r%d", i),
			PR:   pipeline.PullRequestResult{Number: i, CIState: "FAILURE"},
		})
	}
	cfg := config.AgentConfig{Image: "img", MaxConcurrentRuns: 2, PerRunTimeoutSec: 5}

	out := Run(context.Background(), results, cfg, nil)

	if len(out) != 6 {
		t.Fatalf("got %d results, want 6 (ghcall must wait for every started container)", len(out))
	}
	if maxSeen > 2 {
		t.Fatalf("observed %d concurrent docker invocations, want <= max_concurrent_runs (2)", maxSeen)
	}
	if maxSeen < 2 {
		t.Fatalf("observed only %d concurrent invocation, test is not exercising concurrency at all", maxSeen)
	}
}

func TestRun_PerRunTimeout(t *testing.T) {
	orig := execCommandContext
	defer func() { execCommandContext = orig }()

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Sleeps far longer than the configured per-run timeout; the
		// context passed in must cut it off well before that.
		return exec.CommandContext(ctx, "sleep", "5")
	}

	results := []pipeline.FilterResult{{Repo: "o/a", PR: pipeline.PullRequestResult{Number: 1, CIState: "FAILURE"}}}
	cfg := config.AgentConfig{Image: "img", MaxConcurrentRuns: 1, PerRunTimeoutSec: 1}

	start := time.Now()
	out := Run(context.Background(), results, cfg, nil)
	elapsed := time.Since(start)

	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if out[0].Err == nil {
		t.Fatalf("expected a timeout error, got nil (Output=%q)", out[0].Output)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("Run took %s, want well under the fake command's 5s sleep (timeout should have cut it off)", elapsed)
	}
}

func assertContains(t *testing.T, haystack []string, want string) {
	t.Helper()
	for _, s := range haystack {
		if s == want {
			return
		}
	}
	t.Errorf("expected %v to contain %q", haystack, want)
}

func assertContainsSeq(t *testing.T, haystack []string, want ...string) {
	t.Helper()
	for i := 0; i+len(want) <= len(haystack); i++ {
		match := true
		for j, w := range want {
			if haystack[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return
		}
	}
	t.Errorf("expected %v to contain the sequence %v", haystack, want)
}
