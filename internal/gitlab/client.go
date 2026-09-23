// Package gitlab implements vcs.Provider against a self-hosted GitLab:
// conditional REST project checks for change detection, and batched,
// aliased GraphQL queries for merge requests and their pipeline status.
//
// Scope is detection only. ghcall never approves, merges, rebases or
// comments on a GitLab MR — merge policy lives in the agent container —
// and GitLab MRs are not sent to the agent at all (see internal/agent).
package gitlab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ghcall/internal/vcs"
)

// Provider is one GitLab install. BaseURL is the instance root, e.g.
// https://git.example.com — not the /api/v4 path.
type Provider struct {
	httpClient *http.Client
	token      string
	baseURL    string
}

// New builds a provider for the GitLab instance at baseURL, authenticating
// with a personal access token (api or read_api scope is enough: ghcall
// only reads).
func New(baseURL, token string) *Provider {
	return &Provider{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		token:      token,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
	}
}

func (p *Provider) Name() string { return vcs.GitLab }

// projectPath renders a Ref as the URL-encoded full path GitLab's REST API
// wants. Namespaces nest, so "group/subgroup/project" has to arrive as
// "group%2Fsubgroup%2Fproject" — a bare slash would be read as a path
// separator and 404.
func projectPath(ref vcs.Ref) string {
	return url.PathEscape(ref.String())
}

// CheckRepo runs the cheap per-repo change check: a conditional
// GET /api/v4/projects/{path}. A 304 means nothing changed and costs
// nothing.
//
// GitLab's ETag coverage on this endpoint is less dependable than GitHub's
// — some versions and some proxy setups omit it entirely, which would make
// every repo look changed on every run. So the project's last_activity_at
// (cached in the same repos.pushed_at column GitHub's pushed_at uses) is a
// second, always-available signal: if the server answered 200 but activity
// has not moved since the cached value, treat the repo as unchanged.
func (p *Provider) CheckRepo(ref vcs.Ref, etag, pushedAt string) (vcs.CheckResult, error) {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s", p.baseURL, projectPath(ref))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return vcs.CheckResult{}, err
	}
	req.Header.Set("PRIVATE-TOKEN", p.token)
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return vcs.CheckResult{}, fmt.Errorf("checking %s: %w", ref, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return vcs.CheckResult{Changed: false, ETag: etag, PushedAt: pushedAt}, nil
	case http.StatusOK:
		var body struct {
			LastActivityAt string `json:"last_activity_at"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return vcs.CheckResult{}, fmt.Errorf("decoding project %s: %w", ref, err)
		}
		newETag := resp.Header.Get("ETag")
		if body.LastActivityAt != "" && body.LastActivityAt == pushedAt {
			return vcs.CheckResult{Changed: false, ETag: newETag, PushedAt: pushedAt}, nil
		}
		return vcs.CheckResult{
			Changed:  true,
			ETag:     newETag,
			PushedAt: body.LastActivityAt,
		}, nil
	default:
		return vcs.CheckResult{}, fmt.Errorf("checking %s: unexpected status %s", ref, resp.Status)
	}
}
