package github

import "ghcall/internal/vcs"

// Provider pairs the REST and GraphQL clients into the single interface the
// pipeline talks to. The split between them is a cost decision (conditional
// GETs are free, GraphQL points are not), not something callers should care
// about.
type Provider struct {
	rest *RESTClient
	gql  *GraphQLClient
}

// NewProvider builds a GitHub provider. Empty base/graphql URLs mean
// github.com; set them for a GitHub Enterprise install.
func NewProvider(token, baseURL, graphqlURL string) *Provider {
	return &Provider{
		rest: NewRESTClientWithBase(token, baseURL),
		gql:  NewGraphQLClientWithEndpoint(token, graphqlURL),
	}
}

func (p *Provider) Name() string { return vcs.GitHub }

// CheckRepo ignores pushedAt: GitHub's ETags on GET /repos are reliable, so
// there is no need for the last-activity fallback the GitLab provider uses.
func (p *Provider) CheckRepo(ref vcs.Ref, etag, _ string) (vcs.CheckResult, error) {
	res, err := p.rest.CheckRepo(ref.Owner, ref.Name, etag)
	if err != nil {
		return vcs.CheckResult{}, err
	}
	return vcs.CheckResult{Changed: res.Changed, ETag: res.ETag, PushedAt: res.PushedAt}, nil
}

func (p *Provider) FetchRepoPRs(queries []vcs.RepoQuery, first int) (map[vcs.Ref][]vcs.PullRequest, error) {
	prs, _, err := p.gql.FetchRepoPRs(queries, first)
	return prs, err
}

func (p *Provider) RefreshPRStatus(refs []vcs.PRRef) (map[vcs.PRRef]vcs.PRStatus, error) {
	statuses, _, err := p.gql.RefreshPRStatus(refs)
	return statuses, err
}
