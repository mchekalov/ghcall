package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const graphqlEndpoint = "https://api.github.com/graphql"

// GraphQLClient executes batched, aliased GraphQL queries against the
// GitHub API. Unlike RESTClient, every call here costs its computed point
// value regardless of whether anything changed, so callers should only
// invoke it for repos/PRs already known to need a look (see pipeline).
type GraphQLClient struct {
	httpClient *http.Client
	token      string
}

func NewGraphQLClient(token string) *GraphQLClient {
	return &GraphQLClient{httpClient: &http.Client{Timeout: 60 * time.Second}, token: token}
}

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
}

// RateLimit reports the API's own view of remaining GraphQL budget, requested
// alongside every batch query so the pipeline can back off before exhausting it.
type RateLimit struct {
	Remaining int    `json:"remaining"`
	ResetAt   string `json:"resetAt"`
}

func (c *GraphQLClient) execute(query string, variables map[string]any) (json.RawMessage, RateLimit, error) {
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, RateLimit{}, fmt.Errorf("encoding graphql request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, graphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, RateLimit{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, RateLimit{}, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, RateLimit{}, fmt.Errorf("graphql request: unexpected status %s", resp.Status)
	}

	var gr graphqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, RateLimit{}, fmt.Errorf("decoding graphql response: %w", err)
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return nil, RateLimit{}, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}

	var withLimit struct {
		RateLimit RateLimit `json:"rateLimit"`
	}
	_ = json.Unmarshal(gr.Data, &withLimit)

	return gr.Data, withLimit.RateLimit, nil
}

// RepoRef identifies a repo independent of any particular filter config.
type RepoRef struct {
	Owner string
	Name  string
}

// FieldSet controls which optional, cost-bearing sub-selections a PR query
// includes. number/title/updatedAt/author/state are always fetched since the
// pipeline needs them regardless of filter fields.
type FieldSet struct {
	Body bool // fetch bodyText
	CI   bool // fetch statusCheckRollup via the head commit
}

func (fs FieldSet) merge(other FieldSet) FieldSet {
	return FieldSet{Body: fs.Body || other.Body, CI: fs.CI || other.CI}
}

// MergeFieldSets is exported for the pipeline to compute, per repo, the
// union of fields needed across every filter that references it.
func MergeFieldSets(sets ...FieldSet) FieldSet {
	var out FieldSet
	for _, s := range sets {
		out = out.merge(s)
	}
	return out
}

func prSelection(fs FieldSet) string {
	sel := "number title updatedAt state author { login }"
	if fs.Body {
		sel += " bodyText"
	}
	if fs.CI {
		sel += " commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }"
	}
	return sel
}

// PullRequest is the subset of PR data the pipeline understands, decoded
// from whichever fields were actually requested.
type PullRequest struct {
	Number    int
	Title     string
	Body      string
	Author    string
	UpdatedAt string
	State     string // OPEN, CLOSED, MERGED
	CIState   string // "" if not requested or no checks reported yet
}

// RepoQuery is one repo's worth of PR-list request within a batch.
type RepoQuery struct {
	Owner  string
	Name   string
	States []string // GraphQL PullRequestState values: OPEN, CLOSED, MERGED
	Fields FieldSet
}

type prNodesResponse struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	BodyText  string `json:"bodyText"`
	UpdatedAt string `json:"updatedAt"`
	State     string `json:"state"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// FetchRepoPRs runs one aliased batch query covering all of queries, and
// returns each repo's matching PRs (already sorted updated-desc by GitHub,
// capped at `first` per repo). Callers are responsible for chunking queries
// into batches that stay well under GraphQL's node cap.
func (c *GraphQLClient) FetchRepoPRs(queries []RepoQuery, first int) (map[RepoRef][]PullRequest, RateLimit, error) {
	result := make(map[RepoRef][]PullRequest, len(queries))
	if len(queries) == 0 {
		return result, RateLimit{}, nil
	}

	var decls []string
	var body strings.Builder
	variables := map[string]any{}
	body.WriteString("  rateLimit { remaining resetAt }\n")
	for i, q := range queries {
		decls = append(decls, fmt.Sprintf("$owner%d: String!", i), fmt.Sprintf("$name%d: String!", i))
		variables[fmt.Sprintf("owner%d", i)] = q.Owner
		variables[fmt.Sprintf("name%d", i)] = q.Name

		states := "[" + strings.Join(q.States, ", ") + "]"
		fmt.Fprintf(&body, "  r%d: repository(owner: $owner%d, name: $name%d) {\n", i, i, i)
		fmt.Fprintf(&body, "    pullRequests(states: %s, first: %d, orderBy: {field: UPDATED_AT, direction: DESC}) {\n", states, first)
		fmt.Fprintf(&body, "      nodes { %s }\n", prSelection(q.Fields))
		body.WriteString("    }\n  }\n")
	}

	query := fmt.Sprintf("query(%s) {\n%s}", strings.Join(decls, ", "), body.String())

	data, rl, err := c.execute(query, variables)
	if err != nil {
		return nil, rl, err
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, rl, fmt.Errorf("parsing graphql response: %w", err)
	}

	for i, q := range queries {
		ref := RepoRef{Owner: q.Owner, Name: q.Name}
		raw, ok := parsed[fmt.Sprintf("r%d", i)]
		if !ok || string(raw) == "null" {
			result[ref] = nil
			continue
		}
		var repoResp struct {
			PullRequests struct {
				Nodes []prNodesResponse `json:"nodes"`
			} `json:"pullRequests"`
		}
		if err := json.Unmarshal(raw, &repoResp); err != nil {
			return nil, rl, fmt.Errorf("parsing repository %s/%s: %w", q.Owner, q.Name, err)
		}
		prs := make([]PullRequest, 0, len(repoResp.PullRequests.Nodes))
		for _, n := range repoResp.PullRequests.Nodes {
			prs = append(prs, pullRequestFromNode(n))
		}
		result[ref] = prs
	}
	return result, rl, nil
}

func pullRequestFromNode(n prNodesResponse) PullRequest {
	pr := PullRequest{
		Number:    n.Number,
		Title:     n.Title,
		Body:      n.BodyText,
		Author:    n.Author.Login,
		UpdatedAt: n.UpdatedAt,
		State:     n.State,
	}
	if len(n.Commits.Nodes) > 0 {
		pr.CIState = n.Commits.Nodes[0].Commit.StatusCheckRollup.State
	}
	return pr
}

// PRRef identifies a single PR for the CI-status refresh pass.
type PRRef struct {
	Owner  string
	Name   string
	Number int
}

// PRStatus is the result of refreshing one watched PR's status.
type PRStatus struct {
	State   string // OPEN, CLOSED, MERGED
	CIState string
}

// RefreshPRStatus re-checks CI/merge status for already-known PRs directly
// by number, bypassing repo-level change detection entirely: a check run
// finishing does not update the repo's pushed_at, so this is the only way
// to catch a CI transition on a PR that was already fetched.
func (c *GraphQLClient) RefreshPRStatus(refs []PRRef) (map[PRRef]PRStatus, RateLimit, error) {
	result := make(map[PRRef]PRStatus, len(refs))
	if len(refs) == 0 {
		return result, RateLimit{}, nil
	}

	var decls []string
	var body strings.Builder
	variables := map[string]any{}
	body.WriteString("  rateLimit { remaining resetAt }\n")
	for i, r := range refs {
		decls = append(decls,
			fmt.Sprintf("$owner%d: String!", i),
			fmt.Sprintf("$name%d: String!", i),
			fmt.Sprintf("$number%d: Int!", i))
		variables[fmt.Sprintf("owner%d", i)] = r.Owner
		variables[fmt.Sprintf("name%d", i)] = r.Name
		variables[fmt.Sprintf("number%d", i)] = r.Number

		fmt.Fprintf(&body, "  p%d: repository(owner: $owner%d, name: $name%d) {\n", i, i, i)
		fmt.Fprintf(&body, "    pullRequest(number: $number%d) {\n", i)
		body.WriteString("      state\n")
		body.WriteString("      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }\n")
		body.WriteString("    }\n  }\n")
	}

	query := fmt.Sprintf("query(%s) {\n%s}", strings.Join(decls, ", "), body.String())

	data, rl, err := c.execute(query, variables)
	if err != nil {
		return nil, rl, err
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, rl, fmt.Errorf("parsing graphql response: %w", err)
	}

	for i, r := range refs {
		raw, ok := parsed[fmt.Sprintf("p%d", i)]
		if !ok || string(raw) == "null" {
			continue // repo/PR became inaccessible or was deleted; leave unset
		}
		var repoResp struct {
			PullRequest *struct {
				State   string `json:"state"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup struct {
								State string `json:"state"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		}
		if err := json.Unmarshal(raw, &repoResp); err != nil {
			return nil, rl, fmt.Errorf("parsing pull request %s/%s#%d: %w", r.Owner, r.Name, r.Number, err)
		}
		if repoResp.PullRequest == nil {
			continue
		}
		status := PRStatus{State: repoResp.PullRequest.State}
		if len(repoResp.PullRequest.Commits.Nodes) > 0 {
			status.CIState = repoResp.PullRequest.Commits.Nodes[0].Commit.StatusCheckRollup.State
		}
		result[r] = status
	}
	return result, rl, nil
}
