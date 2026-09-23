// Package config loads and validates ghcall's YAML configuration file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"ghcall/internal/vcs"
)

// Config is the top-level shape of ghcall's config file.
type Config struct {
	GitHub      GitHubConfig `yaml:"github"`
	GitLab      GitLabConfig `yaml:"gitlab"`
	Cache       CacheConfig  `yaml:"cache"`
	Concurrency Concurrency  `yaml:"concurrency"`
	Agent       AgentConfig  `yaml:"agent"`
	Filters     []Filter     `yaml:"filters"`
}

// GitHubConfig points at github.com when the URLs are left empty. Set both
// for a GitHub Enterprise install.
type GitHubConfig struct {
	TokenEnv   string `yaml:"token_env"`
	BaseURL    string `yaml:"base_url"`
	GraphQLURL string `yaml:"graphql_url"`
}

// GitLabConfig is required only when some filter sets `provider: gitlab`.
// BaseURL is the instance root (https://git.example.com), not its /api path;
// there is no default, since every GitLab ghcall talks to is self-hosted.
type GitLabConfig struct {
	BaseURL  string `yaml:"base_url"`
	TokenEnv string `yaml:"token_env"`
}

// Cache driver names accepted by cache.driver.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// CacheConfig selects and configures the cache backend. Path applies to the
// sqlite driver only; DSNEnv and MaxOpenConns to postgres only. The DSN is
// read from the environment rather than the YAML on purpose: it carries the
// database password, and the YAML is meant to be mountable as a ConfigMap.
type CacheConfig struct {
	Driver       string `yaml:"driver"`
	Path         string `yaml:"path"`
	DSNEnv       string `yaml:"dsn_env"`
	MaxOpenConns int    `yaml:"max_open_conns"`
}

// DSN returns the PostgreSQL connection string from the configured
// environment variable.
func (c CacheConfig) DSN() (string, error) {
	dsn := os.Getenv(c.DSNEnv)
	if dsn == "" {
		return "", fmt.Errorf("environment variable %s is not set", c.DSNEnv)
	}
	return dsn, nil
}

type Concurrency struct {
	MaxInFlight int `yaml:"max_in_flight"`
}

// Agent launcher names accepted by agent.launcher.
const (
	LauncherDocker     = "docker"
	LauncherKubernetes = "kubernetes"
)

// AgentConfig controls the CI-autofix agent trigger: for every PR a filter
// reports with a failing CI state, ghcall starts one run of this image.
// Absent/zero Image means the trigger is disabled entirely — ghcall behaves
// exactly as before, detect-and-print only.
//
// Launcher picks how that run is started: `docker` shells out to a local
// container runtime and blocks until it exits, `kubernetes` creates one
// Job per PR against the in-cluster API server and returns immediately.
type AgentConfig struct {
	Image             string           `yaml:"image"`
	Launcher          string           `yaml:"launcher"`
	DockerBin         string           `yaml:"docker_bin"`
	EnvPassthrough    []string         `yaml:"env_passthrough"`
	MaxConcurrentRuns int              `yaml:"max_concurrent_runs"`
	PerRunTimeoutSec  int              `yaml:"per_run_timeout_seconds"`
	Kubernetes        KubernetesConfig `yaml:"kubernetes"`
}

// KubernetesConfig configures the Job the kubernetes launcher creates.
// EnvFromSecrets is how the agent's own credentials (Bedrock/LiteLLM) reach
// it: they are mounted onto the agent Job directly, so they never have to be
// present in ghcall's own pod and EnvPassthrough can stay empty in-cluster.
type KubernetesConfig struct {
	Namespace        string            `yaml:"namespace"`
	ServiceAccount   string            `yaml:"service_account"`
	EnvFromSecrets   []string          `yaml:"env_from_secrets"`
	ImagePullSecrets []string          `yaml:"image_pull_secrets"`
	JobTTLSeconds    int               `yaml:"job_ttl_seconds"`
	Labels           map[string]string `yaml:"labels"`
	// Resources is passed through verbatim into the container spec, so any
	// shape the API server accepts works without ghcall knowing about it.
	Resources map[string]any `yaml:"resources"`
}

// Enabled reports whether the agent trigger should run at all.
func (a AgentConfig) Enabled() bool { return a.Image != "" }

// Filter is one named query: a list of repos, an optional author allowlist,
// a PR state, and the set of fields to fetch/emit for matching PRs.
type Filter struct {
	Name string `yaml:"name"`
	// Provider selects the forge this filter's repos live on: github
	// (default) or gitlab.
	Provider string   `yaml:"provider"`
	Repos    []string `yaml:"repos"`
	Authors  []string `yaml:"authors"`
	State    string   `yaml:"state"`
	Fields   []string `yaml:"fields"`
	WatchCI  bool     `yaml:"watch_ci"`
}

// RepoRefs parses this filter's repo list into provider-tagged refs.
func (f Filter) RepoRefs() ([]vcs.Ref, error) {
	refs := make([]vcs.Ref, 0, len(f.Repos))
	for _, r := range f.Repos {
		ref, err := ParseRepo(f.ProviderOrDefault(), r)
		if err != nil {
			return nil, fmt.Errorf("filter %q: %w", f.Name, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// ProviderOrDefault is the filter's provider, defaulting to github for
// configs written before gitlab support existed.
func (f Filter) ProviderOrDefault() string {
	if f.Provider == "" {
		return vcs.GitHub
	}
	return f.Provider
}

// ParseRepo splits a repo path into a Ref. The last segment is the repo
// name and everything before it the owner, because GitLab namespaces nest
// arbitrarily deep ("group/subgroup/project"). GitHub has no nested
// namespaces, so an extra slash there is a typo worth rejecting rather
// than a path that will 404 later.
func ParseRepo(provider, s string) (vcs.Ref, error) {
	invalid := func() (vcs.Ref, error) {
		return vcs.Ref{}, fmt.Errorf("invalid repo %q, want \"owner/name\"", s)
	}
	i := strings.LastIndex(s, "/")
	if i <= 0 || i == len(s)-1 {
		return invalid()
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" {
			return invalid()
		}
	}
	owner, name := s[:i], s[i+1:]
	if provider == vcs.GitHub && strings.Contains(owner, "/") {
		return vcs.Ref{}, fmt.Errorf(
			"invalid repo %q: github has no nested namespaces, want \"owner/name\"", s)
	}
	return vcs.Ref{Provider: provider, Owner: owner, Name: name}, nil
}

// validFields is exactly the set ghcall fetches and emits. created_at and
// url used to pass validation without ever being fetched, which made a
// config look like it asked for something it silently never got; they are
// rejected now rather than lying.
var validFields = map[string]bool{
	"number": true, "title": true, "body": true, "author": true,
	"updated_at": true, "state": true, "ci_status": true,
}

var validStates = map[string]bool{"open": true, "closed": true, "all": true}

const (
	defaultTokenEnv          = "GITHUB_TOKEN"
	defaultGitLabTokenEnv    = "GITLAB_TOKEN"
	defaultCachePath         = "~/.cache/ghcall/cache.db"
	defaultCacheDSNEnv       = "GHCALL_DB_DSN"
	defaultMaxInFlight       = 15
	defaultDockerBin         = "docker"
	defaultMaxConcurrentRuns = 2
	defaultPerRunTimeoutSec  = 25 * 60 // 25 minutes
	defaultJobTTLSeconds     = 3600    // also the kubernetes launcher's dedupe window
)

// Load reads, defaults, and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.GitHub.TokenEnv == "" {
		c.GitHub.TokenEnv = defaultTokenEnv
	}
	if c.GitLab.TokenEnv == "" {
		c.GitLab.TokenEnv = defaultGitLabTokenEnv
	}
	if c.Cache.Driver == "" {
		c.Cache.Driver = DriverSQLite
	}
	if c.Concurrency.MaxInFlight <= 0 {
		c.Concurrency.MaxInFlight = defaultMaxInFlight
	}
	switch c.Cache.Driver {
	case DriverSQLite:
		if c.Cache.Path == "" {
			c.Cache.Path = defaultCachePath
		}
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(c.Cache.Path, "~/") {
			c.Cache.Path = filepath.Join(home, c.Cache.Path[2:])
		}
	case DriverPostgres:
		if c.Cache.DSNEnv == "" {
			c.Cache.DSNEnv = defaultCacheDSNEnv
		}
		if c.Cache.MaxOpenConns <= 0 {
			// Phase 1 runs MaxInFlight goroutines that all write; leave two
			// spare connections for phases 2 and 3.
			c.Cache.MaxOpenConns = c.Concurrency.MaxInFlight + 2
		}
	}
	if c.Agent.Enabled() {
		if c.Agent.Launcher == "" {
			c.Agent.Launcher = LauncherDocker
		}
		if c.Agent.DockerBin == "" {
			c.Agent.DockerBin = defaultDockerBin
		}
		if c.Agent.Kubernetes.JobTTLSeconds <= 0 {
			c.Agent.Kubernetes.JobTTLSeconds = defaultJobTTLSeconds
		}
		if c.Agent.MaxConcurrentRuns <= 0 {
			c.Agent.MaxConcurrentRuns = defaultMaxConcurrentRuns
		}
		if c.Agent.PerRunTimeoutSec <= 0 {
			c.Agent.PerRunTimeoutSec = defaultPerRunTimeoutSec
		}
	}
	for i := range c.Filters {
		if c.Filters[i].State == "" {
			c.Filters[i].State = "open"
		}
		if c.Filters[i].Provider == "" {
			c.Filters[i].Provider = vcs.GitHub
		}
	}
}

func (c *Config) validate() error {
	switch c.Cache.Driver {
	case DriverSQLite:
	case DriverPostgres:
		if c.Cache.DSNEnv == "" {
			return fmt.Errorf("cache: driver %s requires dsn_env", DriverPostgres)
		}
	default:
		return fmt.Errorf("cache: invalid driver %q (want %s|%s)", c.Cache.Driver, DriverSQLite, DriverPostgres)
	}
	if c.Agent.Enabled() {
		switch c.Agent.Launcher {
		case LauncherDocker, LauncherKubernetes:
		default:
			return fmt.Errorf("agent: invalid launcher %q (want %s|%s)",
				c.Agent.Launcher, LauncherDocker, LauncherKubernetes)
		}
	}
	if len(c.Filters) == 0 {
		return fmt.Errorf("no filters defined")
	}
	if c.usesProvider(vcs.GitLab) && c.GitLab.BaseURL == "" {
		return fmt.Errorf("gitlab: base_url is required when a filter sets provider: %s", vcs.GitLab)
	}

	seen := map[string]bool{}
	for _, f := range c.Filters {
		if f.Name == "" {
			return fmt.Errorf("filter has no name")
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate filter name %q", f.Name)
		}
		seen[f.Name] = true

		switch f.Provider {
		case vcs.GitHub, vcs.GitLab:
		default:
			return fmt.Errorf("filter %q: invalid provider %q (want %s|%s)",
				f.Name, f.Provider, vcs.GitHub, vcs.GitLab)
		}

		if len(f.Repos) == 0 {
			return fmt.Errorf("filter %q: no repos", f.Name)
		}
		if _, err := f.RepoRefs(); err != nil {
			return err
		}
		if !validStates[f.State] {
			return fmt.Errorf("filter %q: invalid state %q (want open|closed|all)", f.Name, f.State)
		}
		if len(f.Fields) == 0 {
			return fmt.Errorf("filter %q: no fields", f.Name)
		}
		for _, field := range f.Fields {
			if !validFields[field] {
				return fmt.Errorf("filter %q: unknown field %q", f.Name, field)
			}
		}
	}
	return nil
}

// Token returns the GitHub token from the configured environment variable.
func (c *Config) Token() (string, error) {
	return tokenFromEnv(c.GitHub.TokenEnv)
}

// GitLabToken returns the GitLab personal access token from the configured
// environment variable. Only needed when some filter uses provider: gitlab.
func (c *Config) GitLabToken() (string, error) {
	return tokenFromEnv(c.GitLab.TokenEnv)
}

func tokenFromEnv(name string) (string, error) {
	tok := os.Getenv(name)
	if tok == "" {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return tok, nil
}

// Providers lists the distinct providers this config's filters reference,
// so a run only builds — and only demands credentials for — the forges it
// will actually talk to.
func (c *Config) Providers() []string {
	var out []string
	for _, name := range []string{vcs.GitHub, vcs.GitLab} {
		if c.usesProvider(name) {
			out = append(out, name)
		}
	}
	return out
}

func (c *Config) usesProvider(name string) bool {
	for _, f := range c.Filters {
		if f.ProviderOrDefault() == name {
			return true
		}
	}
	return false
}
