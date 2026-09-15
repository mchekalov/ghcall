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
	Filters     []Filter     `yaml:"filters"`
}

type GitHubConfig struct {
	TokenEnv string `yaml:"token_env"`
}

type CacheConfig struct {
	Path string `yaml:"path"`
}

type Concurrency struct {
	MaxInFlight int `yaml:"max_in_flight"`
}

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
	defaultTokenEnv    = "GITHUB_TOKEN"
	defaultCachePath   = "~/.cache/ghcall/cache.db"
	defaultMaxInFlight = 15
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
	if c.Cache.Path == "" {
		c.Cache.Path = defaultCachePath
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(c.Cache.Path, "~/") {
		c.Cache.Path = filepath.Join(home, c.Cache.Path[2:])
	}
	if c.Concurrency.MaxInFlight <= 0 {
		c.Concurrency.MaxInFlight = defaultMaxInFlight
	}
	for i := range c.Filters {
		if c.Filters[i].State == "" {
			c.Filters[i].State = "open"
		}
	}
}

func (c *Config) validate() error {
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
