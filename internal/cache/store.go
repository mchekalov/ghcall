// Package cache persists repo change-detection state and watched-PR state
// between ghcall runs, in either a local SQLite file or a shared PostgreSQL
// database. Which one is used is decided by config's cache.driver; both
// implementations satisfy Store and are covered by the same conformance tests.
package cache

import (
	"fmt"
	"time"

	"ghcall/internal/config"
)

// RepoState is the cached change-detection state for one repo.
type RepoState struct {
	Owner         string
	Name          string
	ETag          string
	PushedAt      string
	LastCheckedAt time.Time
	LastPRCursor  string
}

// WatchedPR is a cached open PR being tracked for CI status changes.
type WatchedPR struct {
	Owner     string
	Name      string
	Number    int
	UpdatedAt string
	CIState   string
	IsOpen    bool
}

// Store is the persistence contract the pipeline depends on. The SQLite and
// PostgreSQL implementations differ only in placeholder syntax, boolean
// representation and connection-pool sizing.
type Store interface {
	GetRepo(owner, name string) (*RepoState, error)
	UpsertRepo(RepoState) error
	ListOpenWatchedPRs() ([]WatchedPR, error)
	UpsertWatchedPR(WatchedPR) error
	DeleteWatchedPR(owner, name string, number int) error
	Close() error
}

// Open dispatches on cfg.Driver, returning the matching Store with its schema
// already applied.
func Open(cfg config.CacheConfig) (Store, error) {
	switch cfg.Driver {
	case config.DriverSQLite, "":
		return openSQLite(cfg.Path)
	case config.DriverPostgres:
		dsn, err := cfg.DSN()
		if err != nil {
			return nil, err
		}
		return openPostgres(dsn, cfg.MaxOpenConns)
	default:
		return nil, fmt.Errorf("unknown cache driver %q (want %s|%s)",
			cfg.Driver, config.DriverSQLite, config.DriverPostgres)
	}
}
