package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ghcall/internal/vcs"
)

func TestParseRepo(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		in       string
		want     vcs.Ref
		wantErr  string
	}{
		{
			name: "github owner/name", provider: vcs.GitHub, in: "mchekalov/ghcall",
			want: vcs.Ref{Provider: vcs.GitHub, Owner: "mchekalov", Name: "ghcall"},
		},
		{
			name: "gitlab flat path", provider: vcs.GitLab, in: "group/project",
			want: vcs.Ref{Provider: vcs.GitLab, Owner: "group", Name: "project"},
		},
		{
			// The whole point of switching from Cut to LastIndex: the owner
			// is everything up to the final slash.
			name: "gitlab nested namespace", provider: vcs.GitLab, in: "group/subgroup/project",
			want: vcs.Ref{Provider: vcs.GitLab, Owner: "group/subgroup", Name: "project"},
		},
		{
			name: "gitlab deeply nested", provider: vcs.GitLab, in: "a/b/c/d/project",
			want: vcs.Ref{Provider: vcs.GitLab, Owner: "a/b/c/d", Name: "project"},
		},
		{
			name: "github rejects nesting", provider: vcs.GitHub, in: "group/subgroup/project",
			wantErr: "nested namespaces",
		},
		{name: "no slash", provider: vcs.GitHub, in: "ghcall", wantErr: "invalid repo"},
		{name: "leading slash", provider: vcs.GitLab, in: "/group/project", wantErr: "invalid repo"},
		{name: "trailing slash", provider: vcs.GitLab, in: "group/project/", wantErr: "invalid repo"},
		{name: "empty middle segment", provider: vcs.GitLab, in: "group//project", wantErr: "invalid repo"},
		{name: "empty", provider: vcs.GitHub, in: "", wantErr: "invalid repo"},
		{name: "slash only", provider: vcs.GitHub, in: "/", wantErr: "invalid repo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRepo(tc.provider, tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseRepo(%q, %q) = %+v, want error containing %q", tc.provider, tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepo(%q, %q): %v", tc.provider, tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRepo(%q, %q) = %+v, want %+v", tc.provider, tc.in, got, tc.want)
			}
		})
	}
}

// write drops a config file into a temp dir and loads it.
func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return Load(path)
}

const gitlabFilter = `
filters:
  - name: renovate-mrs
    provider: gitlab
    repos: [group/subgroup/project]
    authors: [renovate]
    state: open
    fields: [number, title, author, updated_at, ci_status]
    watch_ci: true
`

func TestLoadGitLabFilter(t *testing.T) {
	cfg, err := load(t, "gitlab:\n  base_url: https://git.example.com\n"+gitlabFilter)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitLab.TokenEnv != defaultGitLabTokenEnv {
		t.Errorf("gitlab.token_env = %q, want the default %q", cfg.GitLab.TokenEnv, defaultGitLabTokenEnv)
	}

	refs, err := cfg.Filters[0].RepoRefs()
	if err != nil {
		t.Fatalf("RepoRefs: %v", err)
	}
	want := vcs.Ref{Provider: vcs.GitLab, Owner: "group/subgroup", Name: "project"}
	if len(refs) != 1 || refs[0] != want {
		t.Errorf("RepoRefs = %+v, want [%+v]", refs, want)
	}

	if got := cfg.Providers(); len(got) != 1 || got[0] != vcs.GitLab {
		t.Errorf("Providers() = %v, want just [gitlab]", got)
	}
}

func TestLoadGitLabWithoutBaseURLIsRejected(t *testing.T) {
	_, err := load(t, gitlabFilter)
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("Load = %v, want a base_url error", err)
	}
}

func TestLoadDefaultsProviderToGitHub(t *testing.T) {
	cfg, err := load(t, `
filters:
  - name: prs
    repos: [mchekalov/ghcall]
    fields: [number, title]
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Filters[0].Provider != vcs.GitHub {
		t.Errorf("provider = %q, want %q for a config that predates gitlab support", cfg.Filters[0].Provider, vcs.GitHub)
	}
	if got := cfg.Providers(); len(got) != 1 || got[0] != vcs.GitHub {
		t.Errorf("Providers() = %v, want just [github]", got)
	}
}

func TestLoadRejectsUnknownProvider(t *testing.T) {
	_, err := load(t, `
filters:
  - name: prs
    provider: bitbucket
    repos: [o/n]
    fields: [number]
`)
	if err == nil || !strings.Contains(err.Error(), "invalid provider") {
		t.Fatalf("Load = %v, want an invalid provider error", err)
	}
}

// created_at and url used to validate but were never fetched or emitted.
func TestLoadRejectsUnimplementedFields(t *testing.T) {
	for _, field := range []string{"created_at", "url"} {
		t.Run(field, func(t *testing.T) {
			_, err := load(t, `
filters:
  - name: prs
    repos: [o/n]
    fields: [number, `+field+`]
`)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Load with field %q = %v, want an unknown field error", field, err)
			}
		})
	}
}

func TestTokens(t *testing.T) {
	cfg, err := load(t, "gitlab:\n  base_url: https://git.example.com\n  token_env: MY_GITLAB_TOKEN\n"+gitlabFilter)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := cfg.GitLabToken(); err == nil {
		t.Fatal("GitLabToken with the env var unset should fail")
	}
	t.Setenv("MY_GITLAB_TOKEN", "glpat-xxx")
	got, err := cfg.GitLabToken()
	if err != nil {
		t.Fatalf("GitLabToken: %v", err)
	}
	if got != "glpat-xxx" {
		t.Errorf("GitLabToken = %q, want the env var's value", got)
	}
}
