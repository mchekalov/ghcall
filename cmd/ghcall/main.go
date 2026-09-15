// Command ghcall runs one incremental check across every configured filter
// and prints matching PRs as JSON.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"ghcall/internal/cache"
	"ghcall/internal/config"
	"ghcall/internal/github"
	"ghcall/internal/output"
	"ghcall/internal/pipeline"
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

	token, err := cfg.Token()
	if err != nil {
		log.Fatalf("ghcall: %v", err)
	}

	c, err := cache.Open(cfg.Cache.Path)
	if err != nil {
		log.Fatalf("ghcall: %v", err)
	}
	defer c.Close()

	rest := github.NewRESTClient(token)
	gql := github.NewGraphQLClient(token)

	results, err := pipeline.New(cfg, rest, gql, c).Run()
	if err != nil {
		log.Fatalf("ghcall: %v", err)
	}

	if err := output.WriteJSON(os.Stdout, results); err != nil {
		log.Fatalf("ghcall: %v", err)
	}
}

// printDryRun shows what a real run would check, per filter, without
// spending any GitHub API quota — for safely validating config changes
// against large repo lists.
func printDryRun(cfg *config.Config) error {
	type planned struct {
		Filter string   `json:"filter"`
		Repos  []string `json:"repos"`
		State  string   `json:"state"`
		Fields []string `json:"fields"`
	}

	plan := make([]planned, 0, len(cfg.Filters))
	for _, f := range cfg.Filters {
		repos := append([]string(nil), f.Repos...)
		sort.Strings(repos)
		plan = append(plan, planned{Filter: f.Name, Repos: repos, State: f.State, Fields: f.Fields})
	}

	fmt.Fprintln(os.Stderr, "ghcall: dry run — no GitHub calls will be made")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(plan)
}
