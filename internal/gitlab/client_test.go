package gitlab

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ghcall/internal/vcs"
)

func nestedRef() vcs.Ref {
	return vcs.Ref{Provider: vcs.GitLab, Owner: "group/subgroup", Name: "project"}
}

// TestCheckRepoEncodesNestedPath is the nested-namespace contract: a bare
// slash in the path would be read as a URL separator and 404.
func TestCheckRepoEncodesNestedPath(t *testing.T) {
	var gotPath, gotRawPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawPath = r.URL.Path, r.URL.EscapedPath()
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		w.Header().Set("ETag", `W/"v1"`)
		json.NewEncoder(w).Encode(map[string]string{"last_activity_at": "2026-09-22T10:00:00Z"})
	}))
	defer srv.Close()

	res, err := New(srv.URL, "glpat-xxx").CheckRepo(nestedRef(), "", "")
	if err != nil {
		t.Fatalf("CheckRepo: %v", err)
	}

	if want := "/api/v4/projects/group%2Fsubgroup%2Fproject"; gotRawPath != want {
		t.Errorf("request path = %q, want %q", gotRawPath, want)
	}
	// The server-side decoded form proves the encoding round-trips to the
	// full path rather than to three path segments.
	if want := "/api/v4/projects/group/subgroup/project"; gotPath != want {
		t.Errorf("decoded path = %q, want %q", gotPath, want)
	}
	if gotToken != "glpat-xxx" {
		t.Errorf("PRIVATE-TOKEN = %q, want the configured token", gotToken)
	}
	if !res.Changed || res.ETag != `W/"v1"` || res.PushedAt != "2026-09-22T10:00:00Z" {
		t.Errorf("CheckRepo = %+v, want a changed result carrying the ETag and last_activity_at", res)
	}
}

func TestCheckRepoNotModified(t *testing.T) {
	var gotIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	res, err := New(srv.URL, "t").CheckRepo(nestedRef(), `W/"v1"`, "2026-09-22T10:00:00Z")
	if err != nil {
		t.Fatalf("CheckRepo: %v", err)
	}
	if gotIfNoneMatch != `W/"v1"` {
		t.Errorf("If-None-Match = %q, want the cached ETag", gotIfNoneMatch)
	}
	if res.Changed {
		t.Error("304 must report Changed=false")
	}
	if res.ETag != `W/"v1"` || res.PushedAt != "2026-09-22T10:00:00Z" {
		t.Errorf("CheckRepo = %+v, want the cached state preserved", res)
	}
}

// TestCheckRepoLastActivityFallback covers the case ETags do not: GitLab
// answers 200 (possibly with no ETag at all), but nothing has happened.
func TestCheckRepoLastActivityFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"last_activity_at": "2026-09-22T10:00:00Z"})
	}))
	defer srv.Close()

	p := New(srv.URL, "t")

	res, err := p.CheckRepo(nestedRef(), "", "2026-09-22T10:00:00Z")
	if err != nil {
		t.Fatalf("CheckRepo: %v", err)
	}
	if res.Changed {
		t.Errorf("CheckRepo = %+v, want Changed=false when last_activity_at has not moved", res)
	}

	res, err = p.CheckRepo(nestedRef(), "", "2026-09-21T10:00:00Z")
	if err != nil {
		t.Fatalf("CheckRepo: %v", err)
	}
	if !res.Changed {
		t.Errorf("CheckRepo = %+v, want Changed=true when last_activity_at moved", res)
	}
}

func TestCheckRepoErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "t").CheckRepo(nestedRef(), "", ""); err == nil {
		t.Fatal("CheckRepo on a 404 should return an error")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to name the status", err)
	}
}

// graphqlServer captures the request body and replies with raw JSON.
func graphqlServer(t *testing.T, reply string, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			t.Errorf("graphql path = %q, want /api/graphql", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer glpat-xxx" {
			t.Errorf("Authorization = %q, want the bearer token", got)
		}
		body, _ := io.ReadAll(r.Body)
		if captured != nil {
			var parsed map[string]any
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
			*captured = parsed
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
}

func TestFetchRepoPRsAliasedBatch(t *testing.T) {
	reply := `{"data":{
		"p0":{"mergeRequests":{"nodes":[
			{"iid":"7","title":"Update dep","description":"body text","updatedAt":"2026-09-22T12:00:00Z",
			 "state":"opened","author":{"username":"renovate"},"headPipeline":{"status":"FAILED"}},
			{"iid":"6","title":"Older","updatedAt":"2026-09-22T09:00:00Z",
			 "state":"merged","author":{"username":"someone"},"headPipeline":null}
		]}},
		"p1":null
	}}`
	var req map[string]any
	srv := graphqlServer(t, reply, &req)
	defer srv.Close()

	a := nestedRef()
	b := vcs.Ref{Provider: vcs.GitLab, Owner: "group", Name: "gone"}
	queries := []vcs.RepoQuery{
		{Ref: a, States: []string{"OPEN"}, Fields: vcs.FieldSet{Body: true, CI: true}},
		{Ref: b, States: []string{"OPEN", "CLOSED", "MERGED"}},
	}

	got, err := New(srv.URL, "glpat-xxx").FetchRepoPRs(queries, 20)
	if err != nil {
		t.Fatalf("FetchRepoPRs: %v", err)
	}

	query, _ := req["query"].(string)
	for _, want := range []string{
		"p0: project(fullPath: $path0)",
		"p1: project(fullPath: $path1)",
		"mergeRequests(state: opened, sort: UPDATED_DESC, first: 20)",
		"mergeRequests(state: all, sort: UPDATED_DESC, first: 20)",
		"description",
		"headPipeline { status }",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q:\n%s", want, query)
		}
	}
	vars, _ := req["variables"].(map[string]any)
	if vars["path0"] != "group/subgroup/project" {
		t.Errorf("path0 = %v, want the full nested path", vars["path0"])
	}
	// The second query asked for neither body nor CI, so those selections
	// must not be paid for across the whole batch.
	if strings.Count(query, "description") != 1 || strings.Count(query, "headPipeline") != 1 {
		t.Errorf("field selections leaked across aliases:\n%s", query)
	}

	prs := got[a]
	if len(prs) != 2 {
		t.Fatalf("got %d MRs for %s, want 2: %+v", len(prs), a, prs)
	}
	want := vcs.PullRequest{
		Number: 7, Title: "Update dep", Body: "body text", Author: "renovate",
		UpdatedAt: "2026-09-22T12:00:00Z", State: "OPEN", CIState: "FAILURE",
	}
	if prs[0] != want {
		t.Errorf("MR[0] = %+v, want %+v", prs[0], want)
	}
	if prs[1].State != "MERGED" || prs[1].CIState != "" {
		t.Errorf("MR[1] = %+v, want MERGED with no CI state (no pipeline)", prs[1])
	}
	if v, ok := got[b]; !ok || v != nil {
		t.Errorf("got[%s] = %+v, want a present nil entry for an inaccessible project", b, v)
	}
}

func TestRefreshPRStatus(t *testing.T) {
	reply := `{"data":{
		"p0":{"mergeRequest":{"state":"opened","title":"Update dependency x","author":{"username":"renovate"},"headPipeline":{"status":"SUCCESS"}}},
		"p1":{"mergeRequest":null},
		"p2":null
	}}`
	var req map[string]any
	srv := graphqlServer(t, reply, &req)
	defer srv.Close()

	a := vcs.PRRef{Ref: nestedRef(), Number: 7}
	b := vcs.PRRef{Ref: nestedRef(), Number: 8}
	c := vcs.PRRef{Ref: vcs.Ref{Provider: vcs.GitLab, Owner: "group", Name: "gone"}, Number: 1}

	got, err := New(srv.URL, "glpat-xxx").RefreshPRStatus([]vcs.PRRef{a, b, c})
	if err != nil {
		t.Fatalf("RefreshPRStatus: %v", err)
	}

	vars, _ := req["variables"].(map[string]any)
	if vars["iid0"] != "7" {
		t.Errorf("iid0 = %v, want the iid as a string (GitLab types it String!)", vars["iid0"])
	}

	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1 (missing MRs are left unset): %+v", len(got), got)
	}
	want := vcs.PRStatus{State: "OPEN", CIState: "SUCCESS", Title: "Update dependency x", Author: "renovate"}
	if got[a] != want {
		t.Errorf("status = %+v, want %+v", got[a], want)
	}
}

func TestGraphQLErrorsSurface(t *testing.T) {
	srv := graphqlServer(t, `{"errors":[{"message":"insufficient scope"}]}`, nil)
	defer srv.Close()

	_, err := New(srv.URL, "glpat-xxx").FetchRepoPRs(
		[]vcs.RepoQuery{{Ref: nestedRef(), States: []string{"OPEN"}}}, 20)
	if err == nil || !strings.Contains(err.Error(), "insufficient scope") {
		t.Fatalf("err = %v, want the GraphQL error message surfaced", err)
	}
}

func TestEmptyBatchesSkipTheNetwork(t *testing.T) {
	p := New("http://127.0.0.1:0", "t") // any request would fail
	if got, err := p.FetchRepoPRs(nil, 20); err != nil || len(got) != 0 {
		t.Errorf("FetchRepoPRs(nil) = %+v, %v, want an empty result and no error", got, err)
	}
	if got, err := p.RefreshPRStatus(nil); err != nil || len(got) != 0 {
		t.Errorf("RefreshPRStatus(nil) = %+v, %v, want an empty result and no error", got, err)
	}
}
