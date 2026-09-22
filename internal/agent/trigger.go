// Package agent starts the shared AI CI-autofix container for PRs a
// pipeline run reports with a failing CI state. It only starts containers
// and records how they exited — it never interprets the fix result, since
// the container posts its own commit/comment/merge outcome straight to
// GitHub.
package agent

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"ghcall/internal/config"
	"ghcall/internal/pipeline"
)

// execCommandContext is overridden in tests so Run can be exercised without
// a real docker binary or containers.
var execCommandContext = exec.CommandContext

// RunResult is the outcome of starting one agent container for one PR.
type RunResult struct {
	Repo   string
	Number int
	Err    error
	Output string
}

// failingCIStates are the statusCheckRollup values that mark a PR as a
// candidate for the autofix agent. GitHub reports FAILURE for a check
// that ran and failed, and ERROR for one that could not run at all.
var failingCIStates = map[string]bool{
	"FAILURE": true,
	"ERRROR":  true,
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

// Run starts one `docker run` per failing-CI PR in results, bounded by
// cfg.MaxConcurrentRuns concurrent containers, and blocks until every
// started container has exited (or hit its per-run timeout). It mirrors
// the bounded worker-pool shape pipeline.checkChanges already uses for
// phase 1 (a semaphore channel + sync.WaitGroup), just sized by
// cfg.MaxConcurrentRuns instead of Concurrency.MaxInFlight, and doing an
// exec.CommandContext per item instead of a REST call.
//
// env is a fully-resolved list of "NAME=VALUE" strings (the platform token
// plus whatever cfg.EnvPassthrough named) forwarded into every container
// via `docker run -e`. Run does not read the process environment itself,
// so it stays deterministic and testable.
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

	maxRuns := cfg.MaxConcurrentRuns
	if maxRuns <= 0 {
		maxRuns = 1
	}
	timeout := time.Duration(cfg.PerRunTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 25 * time.Minute
	}
	dockerBin := cfg.DockerBin
	if dockerBin == "" {
		dockerBin = "docker"
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

			args := make([]string, 0, 6+2*len(env))
			args = append(args, "run", "--rm")
			for _, kv := range env {
				args = append(args, "-e", kv)
			}
			args = append(args, cfg.Image,
				"--platform", "github",
				"--repo", r.Repo,
				"--pr", strconv.Itoa(r.PR.Number))

			cmd := execCommandContext(runCtx, dockerBin, args...)
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()

			mu.Lock()
			out = append(out, RunResult{Repo: r.Repo, Number: r.PR.Number, Err: err, Output: buf.String()})
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return out
}
