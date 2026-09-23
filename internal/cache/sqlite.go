package cache

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ghcall/internal/vcs"

	_ "modernc.org/sqlite"
)

// The meta table is never dropped: it is what tells the next run which
// shape the cache tables are in.
const sqliteMetaSchema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS repos (
	provider         TEXT NOT NULL,
	owner            TEXT NOT NULL,
	name             TEXT NOT NULL,
	etag             TEXT,
	pushed_at        TEXT,
	last_checked_at  TEXT NOT NULL,
	last_pr_cursor   TEXT,
	PRIMARY KEY (provider, owner, name)
);

CREATE TABLE IF NOT EXISTS watched_prs (
	provider     TEXT NOT NULL,
	owner        TEXT NOT NULL,
	name         TEXT NOT NULL,
	number       INTEGER NOT NULL,
	updated_at   TEXT NOT NULL,
	ci_state     TEXT,
	is_open      INTEGER NOT NULL,
	PRIMARY KEY (provider, owner, name, number)
);
`

const dropSchema = `
DROP TABLE IF EXISTS repos;
DROP TABLE IF EXISTS watched_prs;
`

type sqliteStore struct {
	db *sql.DB
}

// openSQLite opens (creating if needed) the SQLite cache at path and applies the schema.
func openSQLite(path string) (*sqliteStore, error) {
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
	if err := applySQLiteSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating cache %s: %w", path, err)
	}
	return &sqliteStore{db: db}, nil
}

// applySQLiteSchema brings the cache to schemaVersion, rebuilding it from
// empty whenever the stored version differs (including the first run, when
// there is none). A cache rebuild costs one run's worth of re-reporting,
// which is cheaper and far less error-prone than migrating a primary key.
func applySQLiteSchema(db *sql.DB) error {
	if _, err := db.Exec(sqliteMetaSchema); err != nil {
		return err
	}
	var have string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&have)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if have == schemaVersion {
		return nil
	}
	if _, err := db.Exec(dropSchema); err != nil {
		return err
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO meta (key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, schemaVersion)
	return err
}

func (c *sqliteStore) Close() error { return c.db.Close() }

// GetRepo returns the cached state for a repo, or nil if it has never been seen.
func (c *sqliteStore) GetRepo(ref vcs.Ref) (*RepoState, error) {
	row := c.db.QueryRow(
		`SELECT provider, owner, name, etag, pushed_at, last_checked_at, last_pr_cursor
		 FROM repos WHERE provider = ? AND owner = ? AND name = ?`,
		ref.Provider, ref.Owner, ref.Name)

	var s RepoState
	var etag, pushedAt, cursor sql.NullString
	var lastChecked string
	if err := row.Scan(&s.Ref.Provider, &s.Ref.Owner, &s.Ref.Name, &etag, &pushedAt, &lastChecked, &cursor); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("reading repo state %s: %w", ref, err)
	}
	s.ETag, s.PushedAt, s.LastPRCursor = etag.String, pushedAt.String, cursor.String
	if t, err := time.Parse(time.RFC3339, lastChecked); err == nil {
		s.LastCheckedAt = t
	}
	return &s, nil
}

// UpsertRepo writes back a repo's change-detection state.
func (c *sqliteStore) UpsertRepo(s RepoState) error {
	_, err := c.db.Exec(`
		INSERT INTO repos (provider, owner, name, etag, pushed_at, last_checked_at, last_pr_cursor)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, owner, name) DO UPDATE SET
			etag = excluded.etag,
			pushed_at = excluded.pushed_at,
			last_checked_at = excluded.last_checked_at,
			last_pr_cursor = excluded.last_pr_cursor`,
		s.Ref.Provider, s.Ref.Owner, s.Ref.Name,
		s.ETag, s.PushedAt, s.LastCheckedAt.Format(time.RFC3339), s.LastPRCursor)
	if err != nil {
		return fmt.Errorf("saving repo state %s: %w", s.Ref, err)
	}
	return nil
}

// ListOpenWatchedPRs returns all currently-open watched PRs, for the CI-status refresh pass.
func (c *sqliteStore) ListOpenWatchedPRs() ([]WatchedPR, error) {
	rows, err := c.db.Query(
		`SELECT provider, owner, name, number, updated_at, ci_state, is_open
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
		if err := rows.Scan(&p.Ref.Provider, &p.Ref.Owner, &p.Ref.Name, &p.Number, &p.UpdatedAt, &ciState, &isOpen); err != nil {
			return nil, fmt.Errorf("scanning watched PR: %w", err)
		}
		p.CIState = ciState.String
		p.IsOpen = isOpen != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertWatchedPR records or updates a tracked PR's state.
func (c *sqliteStore) UpsertWatchedPR(p WatchedPR) error {
	isOpen := 0
	if p.IsOpen {
		isOpen = 1
	}
	_, err := c.db.Exec(`
		INSERT INTO watched_prs (provider, owner, name, number, updated_at, ci_state, is_open)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, owner, name, number) DO UPDATE SET
			updated_at = excluded.updated_at,
			ci_state = excluded.ci_state,
			is_open = excluded.is_open`,
		p.Ref.Provider, p.Ref.Owner, p.Ref.Name, p.Number, p.UpdatedAt, p.CIState, isOpen)
	if err != nil {
		return fmt.Errorf("saving watched PR %s#%d: %w", p.Ref, p.Number, err)
	}
	return nil
}

// DeleteWatchedPR drops a PR from the watch list (e.g. once closed).
func (c *sqliteStore) DeleteWatchedPR(ref vcs.PRRef) error {
	_, err := c.db.Exec(
		`DELETE FROM watched_prs WHERE provider = ? AND owner = ? AND name = ? AND number = ?`,
		ref.Ref.Provider, ref.Ref.Owner, ref.Ref.Name, ref.Number)
	if err != nil {
		return fmt.Errorf("deleting watched PR %s#%d: %w", ref.Ref, ref.Number, err)
	}
	return nil
}
