// Package vcs holds the vocabulary ghcall's pipeline speaks: repo and PR
// references, the PR fields it understands, and the three operations it
// needs from a forge. GitHub and GitLab each implement Provider; nothing
// above this package knows which one it is talking to.
//
// The canonical state vocabulary is GitHub's — OPEN/CLOSED/MERGED for a PR,
// SUCCESS/FAILURE/ERROR/PENDING for CI — because that is what the agent
// trigger and the filter config already speak. Providers whose own
// vocabulary differs map into it (see internal/gitlab/map.go).
package vcs

// Provider names accepted by a filter's `provider` key.
const (
	GitHub = "github"
	GitLab = "gitlab"
)

// Ref identifies a repository on one provider. Owner may contain slashes:
// GitLab namespaces nest arbitrarily deep ("group/subgroup/project" has
// owner "group/subgroup"), while GitHub owners never do.
type Ref struct {
	Provider string
	Owner    string
	Name     string
}

// String is the human-facing repo path, without the provider — this is what
// ghcall prints as a result's "repo" field, alongside a separate "provider".
func (r Ref) String() string { return r.Owner + "/" + r.Name }

// PRRef identifies a single pull/merge request.
type PRRef struct {
	Ref    Ref
	Number int
}

// PullRequest is the subset of PR data the pipeline understands, decoded
// from whichever fields were actually requested. On GitLab, Number is the
// merge request's iid (its per-project number), not its global id.
type PullRequest struct {
	Number    int
	Title     string
	Body      string
	Author    string
	UpdatedAt string
	State     string // OPEN, CLOSED, MERGED
	CIState   string // "" if not requested or no checks reported yet
}

// FieldSet controls which optional, cost-bearing sub-selections a PR query
// includes. number/title/updatedAt/author/state are always fetched since the
// pipeline needs them regardless of filter fields.
type FieldSet struct {
	Body bool // fetch the PR/MR description
	CI   bool // fetch the head commit's CI rollup / head pipeline status
}

func (fs FieldSet) merge(other FieldSet) FieldSet {
	return FieldSet{Body: fs.Body || other.Body, CI: fs.CI || other.CI}
}

// MergeFieldSets computes, per repo, the union of fields needed across every
// filter that references it.
func MergeFieldSets(sets ...FieldSet) FieldSet {
	var out FieldSet
	for _, s := range sets {
		out = out.merge(s)
	}
	return out
}

// RepoQuery is one repo's worth of PR-list request within a batch.
type RepoQuery struct {
	Ref    Ref
	States []string // canonical states: OPEN, CLOSED, MERGED
	Fields FieldSet
}

// PRStatus is the result of refreshing one watched PR's status.
type PRStatus struct {
	State   string // OPEN, CLOSED, MERGED
	CIState string
	// Title and Author are re-read alongside the status so a CI-transition
	// row carries the same identifying fields a phase-2 row does; the
	// watched_prs cache does not store them.
	Title  string
	Author string
}

// CheckResult is the outcome of a repo-level change check.
type CheckResult struct {
	Changed  bool   // false means nothing changed since the cached state
	ETag     string // new ETag to cache (unchanged if Changed is false)
	PushedAt string // last push/activity timestamp, only set when Changed is true
}

// Provider is the forge-specific half of the pipeline: a cheap per-repo
// change check, a batched PR fetch for repos that changed, and a status
// refresh for PRs already being watched.
type Provider interface {
	// Name returns the provider key ("github", "gitlab") that Refs carry.
	Name() string

	// CheckRepo reports whether a repo changed since the cached state.
	// Both cached values are passed because ETags are the primary signal
	// but not a universal one: GitLab does not send them on every project
	// response, so its implementation falls back to comparing pushedAt
	// (the project's last_activity_at) instead.
	CheckRepo(ref Ref, etag, pushedAt string) (CheckResult, error)

	// FetchRepoPRs returns each queried repo's matching PRs, newest-updated
	// first, capped at first per repo. Callers chunk queries themselves.
	FetchRepoPRs(queries []RepoQuery, first int) (map[Ref][]PullRequest, error)

	// RefreshPRStatus re-checks merge and CI state for known PRs by number.
	RefreshPRStatus(refs []PRRef) (map[PRRef]PRStatus, error)
}
