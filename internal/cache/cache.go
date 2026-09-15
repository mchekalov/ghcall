// Package cache persists repo change-detection state and watched-PR state
// between ghcall runs, in a local SQLite file.
package cache

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS repos (
	owner            TEXT NOT NULL,
	name             TEXT NOT NULL,
	etag             TEXT,
	pushed_at        TEXT,
	last_checked_at  TEXT NOT NULL,
	last_pr_cursor   TEXT,
	PRIMARY KEY (owner, name)
);

CREATE TABLE IF NOT EXISTS watched_prs (
	owner        TEXT NOT NULL,
	name         TEXT NOT NULL,
	number       INTEGER NOT NULL,
	updated_at   TEXT NOT NULL,
	ci_state     TEXT,
	is_open      INTEGER NOT NULL,
	PRIMARY KEY (owner, name, number)
);
`

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

type Cache struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite cache at path and applies the schema.
func Open(path string) (*Cache, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating cache dir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening cache %s: %w", path, err)
	}
	// The pipeline hits the cache from many concurrent phase-1 goroutines;
	// SQLite only supports one writer at a time, so serialize through a
	// single connection rather than hitting SQLITE_BUSY under concurrency.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating cache %s: %w", path, err)
	}
	return &Cache{db: db}, nil
}

func (c *Cache) Close() error { return c.db.Close() }

// GetRepo returns the cached state for a repo, or nil if it has never been seen.
func (c *Cache) GetRepo(owner, name string) (*RepoState, error) {
	row := c.db.QueryRow(
		`SELECT owner, name, etag, pushed_at, last_checked_at, last_pr_cursor
		 FROM repos WHERE owner = ? AND name = ?`, owner, name)

	var s RepoState
	var etag, pushedAt, cursor sql.NullString
	var lastChecked string
	if err := row.Scan(&s.Owner, &s.Name, &etag, &pushedAt, &lastChecked, &cursor); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("reading repo state %s/%s: %w", owner, name, err)
	}
	s.ETag, s.PushedAt, s.LastPRCursor = etag.String, pushedAt.String, cursor.String
	if t, err := time.Parse(time.RFC3339, lastChecked); err == nil {
		s.LastCheckedAt = t
	}
	return &s, nil
}

// UpsertRepo writes back a repo's change-detection state.
func (c *Cache) UpsertRepo(s RepoState) error {
	_, err := c.db.Exec(`
		INSERT INTO repos (owner, name, etag, pushed_at, last_checked_at, last_pr_cursor)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, name) DO UPDATE SET
			etag = excluded.etag,
			pushed_at = excluded.pushed_at,
			last_checked_at = excluded.last_checked_at,
			last_pr_cursor = excluded.last_pr_cursor`,
		s.Owner, s.Name, s.ETag, s.PushedAt, s.LastCheckedAt.Format(time.RFC3339), s.LastPRCursor)
	if err != nil {
		return fmt.Errorf("saving repo state %s/%s: %w", s.Owner, s.Name, err)
	}
	return nil
}

// ListOpenWatchedPRs returns all currently-open watched PRs, for the CI-status refresh pass.
func (c *Cache) ListOpenWatchedPRs() ([]WatchedPR, error) {
	rows, err := c.db.Query(
		`SELECT owner, name, number, updated_at, ci_state, is_open
		 FROM watched_prs WHERE is_open = 1`)
	if err != nil {
		return nil, fmt.Errorf("listing watched PRs: %w", err)
	}
	defer rows.Close()

	var out []WatchedPR
	for rows.Next() {
		var p WatchedPR
		var ciState sql.NullString
		var isOpen int
		if err := rows.Scan(&p.Owner, &p.Name, &p.Number, &p.UpdatedAt, &ciState, &isOpen); err != nil {
			return nil, fmt.Errorf("scanning watched PR: %w", err)
		}
		p.CIState = ciState.String
		p.IsOpen = isOpen != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertWatchedPR records or updates a tracked PR's state.
func (c *Cache) UpsertWatchedPR(p WatchedPR) error {
	isOpen := 0
	if p.IsOpen {
		isOpen = 1
	}
	_, err := c.db.Exec(`
		INSERT INTO watched_prs (owner, name, number, updated_at, ci_state, is_open)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, name, number) DO UPDATE SET
			updated_at = excluded.updated_at,
			ci_state = excluded.ci_state,
			is_open = excluded.is_open`,
		p.Owner, p.Name, p.Number, p.UpdatedAt, p.CIState, isOpen)
	if err != nil {
		return fmt.Errorf("saving watched PR %s/%s#%d: %w", p.Owner, p.Name, p.Number, err)
	}
	return nil
}

// DeleteWatchedPR drops a PR from the watch list (e.g. once closed).
func (c *Cache) DeleteWatchedPR(owner, name string, number int) error {
	_, err := c.db.Exec(
		`DELETE FROM watched_prs WHERE owner = ? AND name = ? AND number = ?`,
		owner, name, number)
	if err != nil {
		return fmt.Errorf("deleting watched PR %s/%s#%d: %w", owner, name, number, err)
	}
	return nil
}
