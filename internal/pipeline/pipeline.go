// Package pipeline orchestrates ghcall's three-phase fetch per run:
// cheap per-repo change-detection, batched PR fetches for repos that
// changed, and a CI-status refresh for already-watched open PRs (which
// repo-level change detection can't see, since check runs don't update a
// repo's last-push timestamp).
//
// Every phase groups its work by provider and hands it to the matching
// vcs.Provider, so a config that mixes GitHub and GitLab filters runs both
// in the same pass without either knowing about the other.
package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ghcall/internal/cache"
	"ghcall/internal/config"
	"ghcall/internal/vcs"
)

const prPerRepo = 20 // most-recently-updated PRs fetched per repo

// batchSizeFor is how many repos go into one aliased query. GitHub's node
// cap is generous; GitLab's GraphQL enforces a tighter query-complexity
// limit, so it gets smaller batches.
func batchSizeFor(provider string) int {
	switch provider {
	case vcs.GitLab:
		return 20
	default:
		return 50
	}
}

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

// FilterResult attributes a matched PR back to the named filter that found
// it. Provider is what tells a consumer whether Repo names a GitHub repo or
// a GitLab project path, and whether PR.Number is a PR number or an MR iid.
type FilterResult struct {
	Filter   string            `json:"filter"`
	Provider string            `json:"provider"`
	Repo     string            `json:"repo"`
	PR       PullRequestResult `json:"pr"`
}

type Pipeline struct {
	cfg       *config.Config
	providers map[string]vcs.Provider
	c         cache.Store
}

// New builds a pipeline over the providers keyed by vcs.Provider.Name().
// A config referencing a provider that isn't in the map fails at run time
// with a named error rather than a nil dereference.
func New(cfg *config.Config, providers map[string]vcs.Provider, c cache.Store) *Pipeline {
	return &Pipeline{cfg: cfg, providers: providers, c: c}
}

func (p *Pipeline) providerFor(ref vcs.Ref) (vcs.Provider, error) {
	prov, ok := p.providers[ref.Provider]
	if !ok {
		return nil, fmt.Errorf("no client configured for provider %q (repo %s)", ref.Provider, ref)
	}
	return prov, nil
}

// repoNeeds is the union, across every filter that references a repo, of
// what phase 2 must fetch for it.
type repoNeeds struct {
	states map[string]bool
	fields vcs.FieldSet
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

func fieldSetFor(fields []string) vcs.FieldSet {
	var fs vcs.FieldSet
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

func (p *Pipeline) collectRepoNeeds() (map[vcs.Ref]*repoNeeds, error) {
	needs := map[vcs.Ref]*repoNeeds{}
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
			n.fields = vcs.MergeFieldSets(n.fields, fs)
		}
	}
	return needs, nil
}

// dirtyRepo is a repo phase 1 found changed, carrying the PR-updated_at
// cursor from *before* this run so phase 2 can tell which PRs are new.
type dirtyRepo struct {
	ref       vcs.Ref
	oldCursor string
}

// checkChanges runs phase 1: a bounded-concurrency conditional GET per repo,
// across all providers at once. Unchanged repos (304, or an unmoved activity
// timestamp) cost nothing against the rate limit and are dropped here without
// ever reaching phase 2.
func (p *Pipeline) checkChanges(repos []vcs.Ref) ([]dirtyRepo, error) {
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
		prov, err := p.providerFor(ref)
		if err != nil {
			return nil, err
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(ref vcs.Ref, prov vcs.Provider) {
			defer wg.Done()
			defer func() { <-sem }()

			prev, err := p.c.GetRepo(ref)
			if err != nil {
				fail(err)
				return
			}
			var etag, pushedAt string
			if prev != nil {
				etag, pushedAt = prev.ETag, prev.PushedAt
			}

			res, err := prov.CheckRepo(ref, etag, pushedAt)
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
				Ref:  ref,
				ETag: res.ETag, PushedAt: res.PushedAt,
				LastCheckedAt: time.Now().UTC(), LastPRCursor: cursor,
			}); err != nil {
				fail(err)
				return
			}

			mu.Lock()
			dirty = append(dirty, dirtyRepo{ref: ref, oldCursor: cursor})
			mu.Unlock()
		}(ref, prov)
	}
	wg.Wait()
	return dirty, firstErr
}

// fetchedRepo pairs a repo's freshly-fetched PRs with its pre-run cursor.
type fetchedRepo struct {
	prs       []vcs.PullRequest
	oldCursor string
}

// fetchDirtyRepoPRs runs phase 2: batched, aliased GraphQL fetches for
// repos phase 1 flagged dirty, grouped by provider and chunked to that
// provider's batch size. It also advances each repo's last_pr_cursor.
func (p *Pipeline) fetchDirtyRepoPRs(dirty []dirtyRepo, needs map[vcs.Ref]*repoNeeds) (map[vcs.Ref]fetchedRepo, error) {
	result := make(map[vcs.Ref]fetchedRepo, len(dirty))
	if len(dirty) == 0 {
		return result, nil
	}

	byProvider := map[string][]dirtyRepo{}
	for _, d := range dirty {
		byProvider[d.ref.Provider] = append(byProvider[d.ref.Provider], d)
	}

	for _, name := range sortedKeys(byProvider) {
		repos := byProvider[name]
		prov, err := p.providerFor(repos[0].ref)
		if err != nil {
			return nil, err
		}
		batchSize := batchSizeFor(name)

		for start := 0; start < len(repos); start += batchSize {
			end := min(start+batchSize, len(repos))
			batch := repos[start:end]

			queries := make([]vcs.RepoQuery, len(batch))
			for i, d := range batch {
				n := needs[d.ref]
				states := make([]string, 0, len(n.states))
				for s := range n.states {
					states = append(states, s)
				}
				sort.Strings(states)
				queries[i] = vcs.RepoQuery{Ref: d.ref, States: states, Fields: n.fields}
			}

			fetched, err := prov.FetchRepoPRs(queries, prPerRepo)
			if err != nil {
				return nil, fmt.Errorf("fetching PRs from %s: %w", name, err)
			}

			for _, d := range batch {
				prs := fetched[d.ref]
				result[d.ref] = fetchedRepo{prs: prs, oldCursor: d.oldCursor}

				// Providers return PRs sorted updated-desc, and RFC3339
				// timestamps sort correctly as plain strings, so the first
				// node (if any) is the new cursor.
				newCursor := d.oldCursor
				if len(prs) > 0 && prs[0].UpdatedAt > newCursor {
					newCursor = prs[0].UpdatedAt
				}

				prev, err := p.c.GetRepo(d.ref)
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
	}
	return result, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// matchFilters applies each filter's own state/author/updated-since rules
// to the repos it references, and records any open, CI-watched PR into the
// watched_prs cache for phase 3 to keep refreshing later.
func (p *Pipeline) matchFilters(fetched map[vcs.Ref]fetchedRepo) ([]FilterResult, error) {
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
					Filter:   f.Name,
					Provider: ref.Provider,
					Repo:     ref.String(),
					PR:       resultFromPR(pr),
				})

				if f.WatchCI && pr.State == "OPEN" {
					if err := p.c.UpsertWatchedPR(cache.WatchedPR{
						Ref: ref, Number: pr.Number,
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

func resultFromPR(pr vcs.PullRequest) PullRequestResult {
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

	byProvider := map[string][]cache.WatchedPR{}
	for _, w := range watched {
		byProvider[w.Ref.Provider] = append(byProvider[w.Ref.Provider], w)
	}

	statuses := map[vcs.PRRef]vcs.PRStatus{}
	for _, name := range sortedKeys(byProvider) {
		group := byProvider[name]
		prov, err := p.providerFor(group[0].Ref)
		if err != nil {
			return nil, err
		}
		batchSize := batchSizeFor(name)

		for start := 0; start < len(group); start += batchSize {
			end := min(start+batchSize, len(group))
			refs := make([]vcs.PRRef, 0, end-start)
			for _, w := range group[start:end] {
				refs = append(refs, w.PRRef())
			}
			got, err := prov.RefreshPRStatus(refs)
			if err != nil {
				return nil, fmt.Errorf("refreshing CI status on %s: %w", name, err)
			}
			for ref, st := range got {
				statuses[ref] = st
			}
		}
	}

	filterFor := map[vcs.Ref]string{}
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
		status, ok := statuses[w.PRRef()]
		if !ok {
			continue // PR/repo became inaccessible; leave cached state as-is
		}

		stillOpen := status.State == "OPEN"
		ciChanged := status.CIState != w.CIState

		if ciChanged && stillOpen {
			results = append(results, FilterResult{
				Filter:   filterFor[w.Ref],
				Provider: w.Ref.Provider,
				Repo:     w.Ref.String(),
				PR: PullRequestResult{
					Number: w.Number, Title: status.Title, Author: status.Author,
					UpdatedAt: w.UpdatedAt,
					State:     status.State, CIState: status.CIState, CIChanged: true,
				},
			})
		}

		if !stillOpen {
			if err := p.c.DeleteWatchedPR(w.PRRef()); err != nil {
				return nil, err
			}
			continue
		}
		if ciChanged {
			if err := p.c.UpsertWatchedPR(cache.WatchedPR{
				Ref: w.Ref, Number: w.Number,
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

	repos := make([]vcs.Ref, 0, len(needs))
	for r := range needs {
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].Provider != repos[j].Provider {
			return repos[i].Provider < repos[j].Provider
		}
		return repos[i].String() < repos[j].String()
	})

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
