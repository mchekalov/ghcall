package github

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ghcall/internal/vcs"
)

func ref(owner, name string) vcs.Ref {
	return vcs.Ref{Provider: vcs.GitHub, Owner: owner, Name: name}
}

func TestCheckRepoChanged(t *testing.T) {
	var gotPath, gotAuth, gotIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `W/"v2"`)
		json.NewEncoder(w).Encode(map[string]string{"pushed_at": "2026-09-22T10:00:00Z"})
	}))
	defer srv.Close()

	res, err := NewProvider("tok", srv.URL, srv.URL+"/graphql").CheckRepo(ref("mchekalov", "ghcall"), `W/"v1"`, "")
	if err != nil {
		t.Fatalf("CheckRepo: %v", err)
	}

	if gotPath != "/repos/mchekalov/ghcall" {
		t.Errorf("path = %q, want /repos/mchekalov/ghcall", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if gotIfNoneMatch != `W/"v1"` {
		t.Errorf("If-None-Match = %q, want the cached ETag", gotIfNoneMatch)
	}
	if !res.Changed || res.ETag != `W/"v2"` || res.PushedAt != "2026-09-22T10:00:00Z" {
		t.Errorf("CheckRepo = %+v, want a changed result carrying the new ETag and pushed_at", res)
	}
}

func TestCheckRepoNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	res, err := NewProvider("tok", srv.URL, "").CheckRepo(ref("o", "n"), `W/"v1"`, "")
	if err != nil {
		t.Fatalf("CheckRepo: %v", err)
	}
	if res.Changed || res.ETag != `W/"v1"` {
		t.Errorf("CheckRepo = %+v, want an unchanged result keeping the cached ETag", res)
	}
}

func TestCheckRepoErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := NewProvider("tok", srv.URL, "").CheckRepo(ref("o", "n"), "", ""); err == nil {
		t.Fatal("CheckRepo on a 403 should return an error")
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want it to name the status", err)
	}
}

func graphqlServer(t *testing.T, reply string, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		"rateLimit":{"remaining":4999,"resetAt":"2026-09-22T11:00:00Z"},
		"r0":{"pullRequests":{"nodes":[
			{"number":42,"title":"Bump dep","bodyText":"body","updatedAt":"2026-09-22T12:00:00Z",
			 "state":"OPEN","author":{"login":"renovate[bot]"},
			 "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE"}}}]}}
		]}},
		"r1":null
	}}`
	var req map[string]any
	srv := graphqlServer(t, reply, &req)
	defer srv.Close()

	a, b := ref("o", "a"), ref("o", "b")
	queries := []vcs.RepoQuery{
		{Ref: a, States: []string{"OPEN"}, Fields: vcs.FieldSet{Body: true, CI: true}},
		{Ref: b, States: []string{"CLOSED", "MERGED"}},
	}

	got, err := NewProvider("tok", "", srv.URL).FetchRepoPRs(queries, 20)
	if err != nil {
		t.Fatalf("FetchRepoPRs: %v", err)
	}

	query, _ := req["query"].(string)
	for _, want := range []string{
		"r0: repository(owner: $owner0, name: $name0)",
		"pullRequests(states: [OPEN], first: 20",
		"pullRequests(states: [CLOSED, MERGED], first: 20",
		"bodyText",
		"statusCheckRollup",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q:\n%s", want, query)
		}
	}
	if strings.Count(query, "bodyText") != 1 {
		t.Errorf("body selection leaked into the repo that did not ask for it:\n%s", query)
	}

	prs := got[a]
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1: %+v", len(prs), prs)
	}
	want := vcs.PullRequest{
		Number: 42, Title: "Bump dep", Body: "body", Author: "renovate[bot]",
		UpdatedAt: "2026-09-22T12:00:00Z", State: "OPEN", CIState: "FAILURE",
	}
	if prs[0] != want {
		t.Errorf("PR = %+v, want %+v", prs[0], want)
	}
	if v, ok := got[b]; !ok || v != nil {
		t.Errorf("got[%s] = %+v, want a present nil entry for an inaccessible repo", b, v)
	}
}

func TestRefreshPRStatus(t *testing.T) {
	reply := `{"data":{
		"p0":{"pullRequest":{"state":"OPEN","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}},
		"p1":{"pullRequest":null}
	}}`
	srv := graphqlServer(t, reply, nil)
	defer srv.Close()

	a := vcs.PRRef{Ref: ref("o", "a"), Number: 1}
	b := vcs.PRRef{Ref: ref("o", "b"), Number: 2}

	got, err := NewProvider("tok", "", srv.URL).RefreshPRStatus([]vcs.PRRef{a, b})
	if err != nil {
		t.Fatalf("RefreshPRStatus: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1 (a deleted PR is left unset): %+v", len(got), got)
	}
	if want := (vcs.PRStatus{State: "OPEN", CIState: "SUCCESS"}); got[a] != want {
		t.Errorf("status = %+v, want %+v", got[a], want)
	}
}

func TestGraphQLErrorsSurface(t *testing.T) {
	srv := graphqlServer(t, `{"errors":[{"message":"rate limited"},{"message":"and more"}]}`, nil)
	defer srv.Close()

	_, err := NewProvider("tok", "", srv.URL).FetchRepoPRs(
		[]vcs.RepoQuery{{Ref: ref("o", "a"), States: []string{"OPEN"}}}, 20)
	if err == nil || !strings.Contains(err.Error(), "rate limited; and more") {
		t.Fatalf("err = %v, want every GraphQL error message surfaced", err)
	}
}

func TestProviderNameAndDefaults(t *testing.T) {
	p := NewProvider("tok", "", "")
	if p.Name() != vcs.GitHub {
		t.Errorf("Name() = %q, want %q", p.Name(), vcs.GitHub)
	}
	if p.rest.baseURL != DefaultRESTBase {
		t.Errorf("empty base URL = %q, want it defaulted to %q", p.rest.baseURL, DefaultRESTBase)
	}
	if p.gql.endpoint != DefaultGraphQLEndpoint {
		t.Errorf("empty graphql URL = %q, want it defaulted to %q", p.gql.endpoint, DefaultGraphQLEndpoint)
	}
	// A trailing slash on a GHE base URL must not produce a double slash.
	if got := NewProvider("tok", "https://ghe.example.com/api/v3/", "").rest.baseURL; got != "https://ghe.example.com/api/v3" {
		t.Errorf("base URL = %q, want the trailing slash trimmed", got)
	}
}
