// Command ghcall runs one incremental check across every configured filter
// and prints matching PRs as JSON.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"ghcall/internal/agent"
	"ghcall/internal/cache"
	"ghcall/internal/config"
	"ghcall/internal/github"
	"ghcall/internal/gitlab"
	"ghcall/internal/output"
	"ghcall/internal/pipeline"
	"ghcall/internal/vcs"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to ghcall config file")
	dryRun := flag.Bool("dry-run", false, "print planned per-filter repo/field checks without calling GitHub")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("ghcall: %v", err)
	}

	if *dryRun {
		if err := printDryRun(cfg); err != nil {
			log.Fatalf("ghcall: %v", err)
		}
		return
	}

	providers, token, err := buildProviders(cfg)
	if err != nil {
		log.Fatalf("ghcall: %v", err)
	}

	c, err := cache.Open(cfg.Cache)
	if err != nil {
		log.Fatalf("ghcall: %v", err)
	}
	defer c.Close()

	results, err := pipeline.New(cfg, providers, c).Run()
	if err != nil {
		log.Fatalf("ghcall: %v", err)
	}

	if err := output.WriteJSON(os.Stdout, results); err != nil {
		log.Fatalf("ghcall: %v", err)
	}

	if cfg.Agent.Enabled() {
		if skipped := agent.Skipped(results); len(skipped) > 0 {
			// Not a failure — the agent container can only fix a GitLab MR
			// from inside its own pipeline, so these need a change to that
			// image, not to ghcall. The count is the signal for when that
			// work becomes worth doing.
			log.Printf("ghcall: %d failing-CI result(s) skipped: the autofix agent is GitHub-only", len(skipped))
			for _, r := range skipped {
				log.Printf("ghcall:   skipped %s %s#%d (%s)", r.Provider, r.Repo, r.PR.Number, r.PR.CIState)
			}
		}
		runResults := agent.Run(context.Background(), results, cfg.Agent, buildAgentEnv(cfg, token))
		for _, rr := range runResults {
			switch {
			case rr.Err != nil:
				log.Printf("ghcall: agent run failed for %s#%d: %v\n%s", rr.Repo, rr.Number, rr.Err, rr.Output)
			case cfg.Agent.Launcher == config.LauncherKubernetes:
				// The Job runs on after ghcall exits, so the Job's name (or
				// the note that an identical one was already in flight) is
				// the whole outcome there is to report.
				log.Printf("ghcall: agent job for %s#%d: %s", rr.Repo, rr.Number, rr.Output)
			default:
				log.Printf("ghcall: agent run finished for %s#%d", rr.Repo, rr.Number)
			}
		}
	}
}

// buildProviders constructs a client per provider the config actually
// references, and returns the GitHub token alongside (the agent forwards it
// into every run). A GitLab-only config never asks for a GitHub token, and
// vice versa.
func buildProviders(cfg *config.Config) (map[string]vcs.Provider, string, error) {
	providers := map[string]vcs.Provider{}
	var githubToken string

	for _, name := range cfg.Providers() {
		switch name {
		case vcs.GitHub:
			tok, err := cfg.Token()
			if err != nil {
				return nil, "", err
			}
			githubToken = tok
			providers[name] = github.NewProvider(tok, cfg.GitHub.BaseURL, cfg.GitHub.GraphQLURL)
		case vcs.GitLab:
			tok, err := cfg.GitLabToken()
			if err != nil {
				return nil, "", err
			}
			providers[name] = gitlab.New(cfg.GitLab.BaseURL, tok)
		default:
			return nil, "", fmt.Errorf("unknown provider %q", name)
		}
	}
	return providers, githubToken, nil
}

// buildAgentEnv resolves the env vars forwarded into every agent container:
// the GitHub token ghcall itself uses, plus whatever cfg.Agent.EnvPassthrough
// names (e.g. Bedrock/LiteLLM credentials) are set in ghcall's own process
// environment. Unset passthrough names are silently skipped.
func buildAgentEnv(cfg *config.Config, token string) []string {
	env := []string{"GITHUB_TOKEN=" + token}
	for _, name := range cfg.Agent.EnvPassthrough {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// printDryRun shows what a real run would check, per filter, without
// spending any GitHub API quota — for safely validating config changes
// against large repo lists.
func printDryRun(cfg *config.Config) error {
	type planned struct {
		Filter   string   `json:"filter"`
		Provider string   `json:"provider"`
		Repos    []string `json:"repos"`
		State    string   `json:"state"`
		Fields   []string `json:"fields"`
	}

	plan := make([]planned, 0, len(cfg.Filters))
	for _, f := range cfg.Filters {
		// Parse the refs even though the dry run discards them: nested-path
		// and provider mistakes are exactly what this mode is for catching.
		if _, err := f.RepoRefs(); err != nil {
			return err
		}
		repos := append([]string(nil), f.Repos...)
		sort.Strings(repos)
		plan = append(plan, planned{
			Filter:   f.Name,
			Provider: f.ProviderOrDefault(),
			Repos:    repos,
			State:    f.State,
			Fields:   f.Fields,
		})
	}

	fmt.Fprintln(os.Stderr, "ghcall: dry run — no API calls will be made")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(plan)
}
