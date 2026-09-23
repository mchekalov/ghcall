package gitlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"ghcall/internal/vcs"
)

// GitLab's GraphQL endpoint supports the same aliased-batch trick the GitHub
// client uses, so one request covers many projects.
//
// GraphQL rather than REST is a deliberate choice: the REST merge-request
// *list* endpoint omits head_pipeline, so getting CI status over REST would
// mean an N+1 fetch per MR — exactly the cost this pipeline exists to avoid.

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

func (p *Provider) execute(query string, variables map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, fmt.Errorf("encoding graphql request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/api/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// GitLab's GraphQL endpoint documents bearer auth for personal access
	// tokens; the REST v4 API documents PRIVATE-TOKEN. Same token either way.
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql request: unexpected status %s", resp.Status)
	}

	var gr graphqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("decoding graphql response: %w", err)
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}
	return gr.Data, nil
}

func mrSelection(fs vcs.FieldSet) string {
	sel := "iid title updatedAt state author { username }"
	if fs.Body {
		sel += " description"
	}
	if fs.CI {
		sel += " headPipeline { status }"
	}
	return sel
}

type mrNode struct {
	IID         string `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	UpdatedAt   string `json:"updatedAt"`
	State       string `json:"state"`
	Author      struct {
		Username string `json:"username"`
	} `json:"author"`
	HeadPipeline *struct {
		Status string `json:"status"`
	} `json:"headPipeline"`
}

// FetchRepoPRs runs one aliased batch query covering all of queries and
// returns each project's merge requests, most-recently-updated first and
// capped at `first` per project. Callers chunk queries themselves.
func (p *Provider) FetchRepoPRs(queries []vcs.RepoQuery, first int) (map[vcs.Ref][]vcs.PullRequest, error) {
	result := make(map[vcs.Ref][]vcs.PullRequest, len(queries))
	if len(queries) == 0 {
		return result, nil
	}

	var decls []string
	var body strings.Builder
	variables := map[string]any{}
	for i, q := range queries {
		decls = append(decls, fmt.Sprintf("$path%d: ID!", i))
		variables[fmt.Sprintf("path%d", i)] = q.Ref.String()

		fmt.Fprintf(&body, "  p%d: project(fullPath: $path%d) {\n", i, i)
		fmt.Fprintf(&body, "    mergeRequests(state: %s, sort: UPDATED_DESC, first: %d) {\n",
			mrStateArg(q.States), first)
		fmt.Fprintf(&body, "      nodes { %s }\n", mrSelection(q.Fields))
		body.WriteString("    }\n  }\n")
	}

	query := fmt.Sprintf("query(%s) {\n%s}", strings.Join(decls, ", "), body.String())

	data, err := p.execute(query, variables)
	if err != nil {
		return nil, err
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parsing graphql response: %w", err)
	}

	for i, q := range queries {
		raw, ok := parsed[fmt.Sprintf("p%d", i)]
		if !ok || string(raw) == "null" {
			result[q.Ref] = nil
			continue
		}
		var projectResp struct {
			MergeRequests struct {
				Nodes []mrNode `json:"nodes"`
			} `json:"mergeRequests"`
		}
		if err := json.Unmarshal(raw, &projectResp); err != nil {
			return nil, fmt.Errorf("parsing project %s: %w", q.Ref, err)
		}
		mrs := make([]vcs.PullRequest, 0, len(projectResp.MergeRequests.Nodes))
		for _, n := range projectResp.MergeRequests.Nodes {
			mrs = append(mrs, pullRequestFromNode(n))
		}
		result[q.Ref] = mrs
	}
	return result, nil
}

// pullRequestFromNode maps one MR onto ghcall's canonical shape. GitLab's
// iid is the per-project number users see in the MR URL, which is what the
// pipeline's Number means; the global `id` is deliberately not used.
func pullRequestFromNode(n mrNode) vcs.PullRequest {
	iid, _ := strconv.Atoi(n.IID) // non-numeric iid is not a thing GitLab emits
	pr := vcs.PullRequest{
		Number:    iid,
		Title:     n.Title,
		Body:      n.Description,
		Author:    n.Author.Username,
		UpdatedAt: n.UpdatedAt,
		State:     MRState(n.State),
	}
	if n.HeadPipeline != nil {
		pr.CIState = PipelineState(n.HeadPipeline.Status)
	}
	return pr
}

// RefreshPRStatus re-checks merge and pipeline state for known MRs by iid.
// Like GitHub's, this bypasses project-level change detection: a pipeline
// finishing does not move the project's last_activity_at, so this is the
// only way to see a CI transition on an MR already fetched.
func (p *Provider) RefreshPRStatus(refs []vcs.PRRef) (map[vcs.PRRef]vcs.PRStatus, error) {
	result := make(map[vcs.PRRef]vcs.PRStatus, len(refs))
	if len(refs) == 0 {
		return result, nil
	}

	var decls []string
	var body strings.Builder
	variables := map[string]any{}
	for i, r := range refs {
		decls = append(decls, fmt.Sprintf("$path%d: ID!", i), fmt.Sprintf("$iid%d: String!", i))
		variables[fmt.Sprintf("path%d", i)] = r.Ref.String()
		variables[fmt.Sprintf("iid%d", i)] = strconv.Itoa(r.Number)

		fmt.Fprintf(&body, "  p%d: project(fullPath: $path%d) {\n", i, i)
		fmt.Fprintf(&body, "    mergeRequest(iid: $iid%d) {\n", i)
		body.WriteString("      state title author { username }\n")
		body.WriteString("      headPipeline { status }\n")
		body.WriteString("    }\n  }\n")
	}

	query := fmt.Sprintf("query(%s) {\n%s}", strings.Join(decls, ", "), body.String())

	data, err := p.execute(query, variables)
	if err != nil {
		return nil, err
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parsing graphql response: %w", err)
	}

	for i, r := range refs {
		raw, ok := parsed[fmt.Sprintf("p%d", i)]
		if !ok || string(raw) == "null" {
			continue // project became inaccessible or was deleted; leave unset
		}
		var projectResp struct {
			MergeRequest *struct {
				State  string `json:"state"`
				Title  string `json:"title"`
				Author struct {
					Username string `json:"username"`
				} `json:"author"`
				HeadPipeline *struct {
					Status string `json:"status"`
				} `json:"headPipeline"`
			} `json:"mergeRequest"`
		}
		if err := json.Unmarshal(raw, &projectResp); err != nil {
			return nil, fmt.Errorf("parsing merge request %s!%d: %w", r.Ref, r.Number, err)
		}
		if projectResp.MergeRequest == nil {
			continue
		}
		status := vcs.PRStatus{
			State:  MRState(projectResp.MergeRequest.State),
			Title:  projectResp.MergeRequest.Title,
			Author: projectResp.MergeRequest.Author.Username,
		}
		if projectResp.MergeRequest.HeadPipeline != nil {
			status.CIState = PipelineState(projectResp.MergeRequest.HeadPipeline.Status)
		}
		result[r] = status
	}
	return result, nil
}
