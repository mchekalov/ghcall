// Package config loads and validates ghcall's YAML configuration file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level shape of ghcall's config file.
type Config struct {
	GitHub      GitHubConfig `yaml:"github"`
	Cache       CacheConfig  `yaml:"cache"`
	Concurrency Concurrency  `yaml:"concurrency"`
	Agent       AgentConfig  `yaml:"agent"`
	Filters     []Filter     `yaml:"filters"`
}

type GitHubConfig struct {
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
	Name    string   `yaml:"name"`
	Repos   []string `yaml:"repos"`
	Authors []string `yaml:"authors"`
	State   string   `yaml:"state"`
	Fields  []string `yaml:"fields"`
	WatchCI bool     `yaml:"watch_ci"`
}

// RepoRef is an owner/name pair parsed from a Filter's repo list.
type RepoRef struct {
	Owner string
	Name  string
}

func (r RepoRef) String() string { return r.Owner + "/" + r.Name }

func (f Filter) RepoRefs() ([]RepoRef, error) {
	refs := make([]RepoRef, 0, len(f.Repos))
	for _, r := range f.Repos {
		ref, err := ParseRepo(r)
		if err != nil {
			return nil, fmt.Errorf("filter %q: %w", f.Name, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// ParseRepo splits "owner/name" into a RepoRef.
func ParseRepo(s string) (RepoRef, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" {
		return RepoRef{}, fmt.Errorf("invalid repo %q, want \"owner/name\"", s)
	}
	return RepoRef{Owner: owner, Name: name}, nil
}

var validFields = map[string]bool{
	"number": true, "title": true, "body": true, "author": true,
	"updated_at": true, "created_at": true, "state": true,
	"ci_status": true, "url": true,
}

var validStates = map[string]bool{"open": true, "closed": true, "all": true}

const (
	defaultTokenEnv          = "GITHUB_TOKEN"
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
	seen := map[string]bool{}
	for _, f := range c.Filters {
		if f.Name == "" {
			return fmt.Errorf("filter has no name")
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate filter name %q", f.Name)
		}
		seen[f.Name] = true

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
	tok := os.Getenv(c.GitHub.TokenEnv)
	if tok == "" {
		return "", fmt.Errorf("environment variable %s is not set", c.GitHub.TokenEnv)
	}
	return tok, nil
}
