// Package github holds the two GitHub clients ghcall's pipeline uses:
// a REST client for cheap conditional change-detection, and a GraphQL
// client for batched, field-selective PR/CI fetches. Provider pairs them
// into one vcs.Provider.
//
// Both clients take their endpoint as a field rather than a constant, so a
// GitHub Enterprise install can be pointed at, and so the tests can point
// at an httptest server.
package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultRESTBase and DefaultGraphQLEndpoint are github.com's public API.
const (
	DefaultRESTBase        = "https://api.github.com"
	DefaultGraphQLEndpoint = "https://api.github.com/graphql"
)

// RESTClient does conditional GETs against the REST API. A 304 response
// costs nothing against the primary rate limit, so this is the cheap
// "did anything change" check the pipeline runs for every tracked repo.
type RESTClient struct {
	httpClient *http.Client
	token      string
	baseURL    string
}

// NewRESTClient targets github.com. Pass a base URL for GitHub Enterprise.
func NewRESTClient(token string) *RESTClient {
	return NewRESTClientWithBase(token, DefaultRESTBase)
}

func NewRESTClientWithBase(token, baseURL string) *RESTClient {
	if baseURL == "" {
		baseURL = DefaultRESTBase
	}
	return &RESTClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		token:      token,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
	}
}

// RepoCheckResult is the outcome of a conditional GET /repos/{owner}/{name}.
type RepoCheckResult struct {
	Changed  bool   // false means the server returned 304: nothing to do
	ETag     string // new ETag to cache (unchanged if Changed is false)
	PushedAt string // GitHub's pushed_at, only set when Changed is true
}

// CheckRepo performs a conditional GET, sending etag as If-None-Match when non-empty.
func (c *RESTClient) CheckRepo(owner, name, etag string) (RepoCheckResult, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", c.baseURL, owner, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return RepoCheckResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RepoCheckResult{}, fmt.Errorf("checking %s/%s: %w", owner, name, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return RepoCheckResult{Changed: false, ETag: etag}, nil
	case http.StatusOK:
		var body struct {
			PushedAt string `json:"pushed_at"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return RepoCheckResult{}, fmt.Errorf("decoding repo %s/%s: %w", owner, name, err)
		}
		return RepoCheckResult{
			Changed:  true,
			ETag:     resp.Header.Get("ETag"),
			PushedAt: body.PushedAt,
		}, nil
	default:
		return RepoCheckResult{}, fmt.Errorf("checking %s/%s: unexpected status %s", owner, name, resp.Status)
	}
}
