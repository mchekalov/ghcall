// Package pipeline orchestrates ghcall's three-phase fetch per run:
// cheap REST change-detection, batched GraphQL PR fetches for repos that
// changed, and a CI-status refresh for already-watched open PRs (which
// repo-level change detection can't see, since check runs don't update a
// repo's pushed_at).
package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ghcall/internal/cache"
	"ghcall/internal/config"
	"ghcall/internal/github"
)

const (
	graphqlBatchSize = 50 // repos per aliased GraphQL request
	prPerRepo        = 20 // most-recently-updated PRs fetched per repo
)

// PullRequestResult is the data ghcall reports for one matching PR.
type PullRequestResult struct {
	Number    int    `json:"number"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"body,omitempty"`
	Author    string `json:"author,omitempty"`
	UpdatedAt string `json:"updated_at"`
	State     string `json:"state"`
	CIState   string `json:"ci_status,omitempty"`
	// CIChanged marks a row surfaced by the phase-3 CI refresh rather than
	// by the PR itself having a newer updated_at.
	CIChanged bool `json:"ci_changed,omitempty"`
}

// FilterResult attributes a matched PR back to the named filter that found it.
type FilterResult struct {
	Filter string             `json:"filter"`
	Repo   string             `json:"repo"`
	PR     PullRequestResult  `json:"pr"`
}

type Pipeline struct {
	cfg  *config.Config
	rest *github.RESTClient
	gql  *github.GraphQLClient
	c    *cache.Cache
}

func New(cfg *config.Config, rest *github.RESTClient, gql *github.GraphQLClient, c *cache.Cache) *Pipeline {
	return &Pipeline{cfg: cfg, rest: rest, gql: gql, c: c}
}

// repoNeeds is the union, across every filter that references a repo, of
// what phase 2 must fetch for it.
type repoNeeds struct {
	states map[string]bool
	fields github.FieldSet
}

func statesFor(state string) []string {
	switch state {
	case "open":
		return []string{"OPEN"}
	case "closed":
		return []string{"CLOSED", "MERGED"}
	default:
		return []string{"OPEN", "CLOSED", "MERGED"}
	}
}

func fieldSetFor(fields []string) github.FieldSet {
	var fs github.FieldSet
	for _, f := range fields {
		switch f {
		case "body":
			fs.Body = true
		case "ci_status":
			fs.CI = true
		}
	}
	return fs
}

func (p *Pipeline) collectRepoNeeds() (map[config.RepoRef]*repoNeeds, error) {
	needs := map[config.RepoRef]*repoNeeds{}
	for _, f := range p.cfg.Filters {
		refs, err := f.RepoRefs()
		if err != nil {
			return nil, err
		}
		fs := fieldSetFor(f.Fields)
		states := statesFor(f.State)

		for _, ref := range refs {
			n, ok := needs[ref]
			if !ok {
				n = &repoNeeds{states: map[string]bool{}}
				needs[ref] = n
			}
			for _, s := range states {
				n.states[s] = true
			}
			n.fields = github.MergeFieldSets(n.fields, fs)
		}
	}
	return needs, nil
}

// dirtyRepo is a repo phase 1 found changed, carrying the PR-updated_at
// cursor from *before* this run so phase 2 can tell which PRs are new.
type dirtyRepo struct {
	ref       config.RepoRef
	oldCursor string
}

// checkChanges runs phase 1: a bounded-concurrency conditional GET per repo.
// Unchanged repos (304) cost nothing against the rate limit and are dropped
// here without ever reaching phase 2.
func (p *Pipeline) checkChanges(repos []config.RepoRef) ([]dirtyRepo, error) {
	sem := make(chan struct{}, p.cfg.Concurrency.MaxInFlight)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var dirty []dirtyRepo
	var firstErr error

	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	for _, ref := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(ref config.RepoRef) {
			defer wg.Done()
			defer func() { <-sem }()

			prev, err := p.c.GetRepo(ref.Owner, ref.Name)
			if err != nil {
				fail(err)
				return
			}
			var etag string
			if prev != nil {
				etag = prev.ETag
			}

			res, err := p.rest.CheckRepo(ref.Owner, ref.Name, etag)
			if err != nil {
				fail(err)
				return
			}
			if !res.Changed {
				return
			}

			cursor := ""
			if prev != nil {
				cursor = prev.LastPRCursor
			}
			if err := p.c.UpsertRepo(cache.RepoState{
				Owner: ref.Owner, Name: ref.Name,
				ETag: res.ETag, PushedAt: res.PushedAt,
				LastCheckedAt: time.Now().UTC(), LastPRCursor: cursor,
			}); err != nil {
				fail(err)
				return
			}

			mu.Lock()
			dirty = append(dirty, dirtyRepo{ref: ref, oldCursor: cursor})
			mu.Unlock()
		}(ref)
	}
	wg.Wait()
	return dirty, firstErr
}

// fetchedRepo pairs a repo's freshly-fetched PRs with its pre-run cursor.
type fetchedRepo struct {
	prs       []github.PullRequest
	oldCursor string
}

// fetchDirtyRepoPRs runs phase 2: batched, aliased GraphQL fetches for
// repos phase 1 flagged dirty, chunked to keep each request's node count
// well under GraphQL's cap. It also advances each repo's last_pr_cursor.
func (p *Pipeline) fetchDirtyRepoPRs(dirty []dirtyRepo, needs map[config.RepoRef]*repoNeeds) (map[config.RepoRef]fetchedRepo, error) {
	result := make(map[config.RepoRef]fetchedRepo, len(dirty))
	if len(dirty) == 0 {
		return result, nil
	}

	for start := 0; start < len(dirty); start += graphqlBatchSize {
		end := min(start+graphqlBatchSize, len(dirty))
		batch := dirty[start:end]

		queries := make([]github.RepoQuery, len(batch))
		for i, d := range batch {
			n := needs[d.ref]
			states := make([]string, 0, len(n.states))
			for s := range n.states {
				states = append(states, s)
			}
			sort.Strings(states)
			queries[i] = github.RepoQuery{Owner: d.ref.Owner, Name: d.ref.Name, States: states, Fields: n.fields}
		}

		fetched, _, err := p.gql.FetchRepoPRs(queries, prPerRepo)
		if err != nil {
			return nil, fmt.Errorf("fetching PRs: %w", err)
		}

		for _, d := range batch {
			prs := fetched[github.RepoRef{Owner: d.ref.Owner, Name: d.ref.Name}]
			result[d.ref] = fetchedRepo{prs: prs, oldCursor: d.oldCursor}

			// GraphQL returns PRs sorted updated-desc, and RFC3339 timestamps
			// sort correctly as plain strings, so the first node (if any) is
			// the new cursor.
			newCursor := d.oldCursor
			if len(prs) > 0 && prs[0].UpdatedAt > newCursor {
				newCursor = prs[0].UpdatedAt
			}

			prev, err := p.c.GetRepo(d.ref.Owner, d.ref.Name)
			if err != nil {
				return nil, err
			}
			if prev == nil {
				continue // shouldn't happen: phase 1 just upserted this repo
			}
			prev.LastPRCursor = newCursor
			if err := p.c.UpsertRepo(*prev); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

// matchFilters applies each filter's own state/author/updated-since rules
// to the repos it references, and records any open, CI-watched PR into the
// watched_prs cache for phase 3 to keep refreshing later.
func (p *Pipeline) matchFilters(fetched map[config.RepoRef]fetchedRepo) ([]FilterResult, error) {
	var results []FilterResult

	for _, f := range p.cfg.Filters {
		refs, err := f.RepoRefs()
		if err != nil {
			return nil, err
		}
		wantStates := map[string]bool{}
		for _, s := range statesFor(f.State) {
			wantStates[s] = true
		}
		authors := map[string]bool{}
		for _, a := range f.Authors {
			authors[strings.ToLower(a)] = true
		}

		for _, ref := range refs {
			fr, ok := fetched[ref]
			if !ok {
				continue // repo untouched since last run: nothing new for this filter
			}
			for _, pr := range fr.prs {
				if !wantStates[pr.State] {
					continue
				}
				if fr.oldCursor != "" && pr.UpdatedAt <= fr.oldCursor {
					continue // already reported in a previous run
				}
				if len(authors) > 0 && !authors[strings.ToLower(pr.Author)] {
					continue
				}

				results = append(results, FilterResult{
					Filter: f.Name,
					Repo:   ref.String(),
					PR:     resultFromPR(pr),
				})

				if f.WatchCI && pr.State == "OPEN" {
					if err := p.c.UpsertWatchedPR(cache.WatchedPR{
						Owner: ref.Owner, Name: ref.Name, Number: pr.Number,
						UpdatedAt: pr.UpdatedAt, CIState: pr.CIState, IsOpen: true,
					}); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return results, nil
}

func resultFromPR(pr github.PullRequest) PullRequestResult {
	return PullRequestResult{
		Number: pr.Number, Title: pr.Title, Body: pr.Body, Author: pr.Author,
		UpdatedAt: pr.UpdatedAt, State: pr.State, CIState: pr.CIState,
	}
}

// refreshWatchedCI runs phase 3: re-checks CI/merge status directly by PR
// number for every currently-open watched PR, independent of whether its
// repo was flagged dirty in phase 1.
func (p *Pipeline) refreshWatchedCI() ([]FilterResult, error) {
	watched, err := p.c.ListOpenWatchedPRs()
	if err != nil {
		return nil, err
	}
	if len(watched) == 0 {
		return nil, nil
	}

	refs := make([]github.PRRef, len(watched))
	for i, w := range watched {
		refs[i] = github.PRRef{Owner: w.Owner, Name: w.Name, Number: w.Number}
	}

	statuses, _, err := p.gql.RefreshPRStatus(refs)
	if err != nil {
		return nil, fmt.Errorf("refreshing CI status: %w", err)
	}

	filterFor := map[config.RepoRef]string{}
	for _, f := range p.cfg.Filters {
		if !f.WatchCI {
			continue
		}
		filterRefs, err := f.RepoRefs()
		if err != nil {
			return nil, err
		}
		for _, r := range filterRefs {
			filterFor[r] = f.Name
		}
	}

	var results []FilterResult
	for _, w := range watched {
		ref := github.PRRef{Owner: w.Owner, Name: w.Name, Number: w.Number}
		status, ok := statuses[ref]
		if !ok {
			continue // PR/repo became inaccessible; leave cached state as-is
		}

		stillOpen := status.State == "OPEN"
		ciChanged := status.CIState != w.CIState

		if ciChanged && stillOpen {
			results = append(results, FilterResult{
				Filter: filterFor[config.RepoRef{Owner: w.Owner, Name: w.Name}],
				Repo:   w.Owner + "/" + w.Name,
				PR: PullRequestResult{
					Number: w.Number, UpdatedAt: w.UpdatedAt,
					State: status.State, CIState: status.CIState, CIChanged: true,
				},
			})
		}

		if !stillOpen {
			if err := p.c.DeleteWatchedPR(w.Owner, w.Name, w.Number); err != nil {
				return nil, err
			}
			continue
		}
		if ciChanged {
			if err := p.c.UpsertWatchedPR(cache.WatchedPR{
				Owner: w.Owner, Name: w.Name, Number: w.Number,
				UpdatedAt: w.UpdatedAt, CIState: status.CIState, IsOpen: true,
			}); err != nil {
				return nil, err
			}
		}
	}
	return results, nil
}

// Run executes all three phases and returns every matched PR across all filters.
func (p *Pipeline) Run() ([]FilterResult, error) {
	needs, err := p.collectRepoNeeds()
	if err != nil {
		return nil, err
	}

	repos := make([]config.RepoRef, 0, len(needs))
	for r := range needs {
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].String() < repos[j].String() })

	dirty, err := p.checkChanges(repos)
	if err != nil {
		return nil, err
	}

	fetched, err := p.fetchDirtyRepoPRs(dirty, needs)
	if err != nil {
		return nil, err
	}

	results, err := p.matchFilters(fetched)
	if err != nil {
		return nil, err
	}

	ciResults, err := p.refreshWatchedCI()
	if err != nil {
		return nil, err
	}
	return append(results, ciResults...), nil
}
