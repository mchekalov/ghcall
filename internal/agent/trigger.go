// Package agent starts the shared AI CI-autofix container for PRs a
// pipeline run reports with a failing CI state. It only starts containers
// and records how they exited — it never interprets the fix result, since
// the container posts its own commit/comment/merge outcome straight to
// GitHub.
//
// How a run is started is the launcher's job: docker.go shells out to a
// local container runtime, kubernetes.go creates a Job per PR against the
// in-cluster API server. Run owns everything around that — which PRs are
// candidates, the bounded worker pool, the per-run timeout — and is
// identical for both.
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ghcall/internal/config"
	"ghcall/internal/pipeline"
)

// RunResult is the outcome of starting one agent run for one PR.
type RunResult struct {
	Repo   string
	Number int
	Err    error
	Output string
}

// launchFunc starts one agent run for one PR. What "started" means is
// launcher-specific: the docker launcher blocks until the container exits
// and returns its combined output, the kubernetes launcher returns as soon
// as the API server has accepted the Job and returns the Job's name.
type launchFunc func(ctx context.Context, target pipeline.FilterResult, env []string) (output string, err error)

// failingCIStates are the statusCheckRollup values that mark a PR as a
// candidate for the autofix agent. GitHub reports FAILURE for a check
// that ran and failed, and ERROR for one that could not run at all.
var failingCIStates = map[string]bool{
	"FAILURE": true,
	"ERROR":   true,
}

// Candidates filters results down to the PRs the agent should be started
// for: any result whose CI state indicates a failure.
func Candidates(results []pipeline.FilterResult) []pipeline.FilterResult {
	var out []pipeline.FilterResult
	for _, r := range results {
		if failingCIStates[r.PR.CIState] {
			out = append(out, r)
		}
	}
	return out
}

// launcherFor builds the launch function cfg selects.
func launcherFor(cfg config.AgentConfig) (launchFunc, error) {
	switch cfg.Launcher {
	case config.LauncherKubernetes:
		return newKubernetesLauncherFunc(cfg)
	case config.LauncherDocker, "":
		return newDockerLauncher(cfg), nil
	default:
		return nil, fmt.Errorf("unknown agent launcher %q (want %s|%s)",
			cfg.Launcher, config.LauncherDocker, config.LauncherKubernetes)
	}
}

// Run starts one agent run per failing-CI PR in results, bounded by
// cfg.MaxConcurrentRuns concurrent launches, and blocks until every launch
// has returned. It mirrors the bounded worker-pool shape
// pipeline.checkChanges already uses for phase 1 (a semaphore channel +
// sync.WaitGroup), just sized by cfg.MaxConcurrentRuns instead of
// Concurrency.MaxInFlight.
//
// env is a fully-resolved list of "NAME=VALUE" strings (the platform token
// plus whatever cfg.EnvPassthrough named) forwarded into every run. Run does
// not read the process environment itself, so it stays deterministic and
// testable.
//
// If cfg is not Enabled(), or results contains no failing-CI PRs, Run
// returns nil immediately without starting anything.
func Run(ctx context.Context, results []pipeline.FilterResult, cfg config.AgentConfig, env []string) []RunResult {
	if !cfg.Enabled() {
		return nil
	}
	targets := Candidates(results)
	if len(targets) == 0 {
		return nil
	}

	launch, err := launcherFor(cfg)
	if err != nil {
		// Nothing can be started at all; report it once per target so the
		// caller's per-PR logging still names what was missed.
		out := make([]RunResult, 0, len(targets))
		for _, r := range targets {
			out = append(out, RunResult{Repo: r.Repo, Number: r.PR.Number, Err: err})
		}
		return out
	}

	maxRuns := cfg.MaxConcurrentRuns
	if maxRuns <= 0 {
		maxRuns = 1
	}
	timeout := time.Duration(cfg.PerRunTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 25 * time.Minute
	}

	sem := make(chan struct{}, maxRuns)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var out []RunResult

	for _, r := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(r pipeline.FilterResult) {
			defer wg.Done()
			defer func() { <-sem }()

			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			output, err := launch(runCtx, r, env)

			mu.Lock()
			out = append(out, RunResult{Repo: r.Repo, Number: r.PR.Number, Err: err, Output: output})
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return out
}
