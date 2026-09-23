package cache

import (
	"database/sql"
	"fmt"
	"time"

	"ghcall/internal/vcs"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgSchemaLockID guards schema creation. Several ghcall pods can start at the
// same time, and concurrent CREATE TABLE IF NOT EXISTS statements can both
// pass the existence check, leaving one with a 42P07 duplicate_table error.
// Taking a transaction-scoped advisory lock first serializes them — and it
// is what makes the drop-and-recreate on a schema-version bump safe too:
// no pod can read a half-rebuilt cache.
const pgSchemaLockID = 7304531225068403201 // arbitrary, must match across ghcall versions

const postgresMetaSchema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

const postgresSchema = `
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
	number       BIGINT NOT NULL,
	updated_at   TEXT NOT NULL,
	ci_state     TEXT,
	is_open      BOOLEAN NOT NULL,
	PRIMARY KEY (provider, owner, name, number)
);
`

const (
	pgMaxIdleConns    = 2
	pgConnMaxLifetime = 30 * time.Minute
	pgDefaultMaxOpen  = 17 // fallback when the caller passes nothing usable
)

type postgresStore struct {
	db *sql.DB
}

// openPostgres connects to dsn and applies the schema. Unlike SQLite there is
// no single-writer constraint, so phase 1's goroutines get a real pool sized
// by maxOpenConns.
func openPostgres(dsn string, maxOpenConns int) (*postgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres cache: %w", err)
	}
	if maxOpenConns <= 0 {
		maxOpenConns = pgDefaultMaxOpen
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(pgMaxIdleConns)
	db.SetConnMaxLifetime(pgConnMaxLifetime)

	if err := applyPostgresSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &postgresStore{db: db}, nil
}

func applyPostgresSchema(db *sql.DB) error {
	// meta has to exist before the lock transaction can read it, and its own
	// creation is racy for the same reason the rest is — so it goes through
	// a short lock transaction of its own.
	if err := lockedExec(db, postgresMetaSchema); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrating postgres cache: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, int64(pgSchemaLockID)); err != nil {
		return fmt.Errorf("locking postgres cache schema: %w", err)
	}

	var have string
	err = tx.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&have)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("reading postgres cache schema version: %w", err)
	}
	if have == schemaVersion {
		// Another pod already rebuilt it while we waited for the lock.
		return tx.Commit()
	}

	for _, stmt := range []string{dropSchema, postgresSchema} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrating postgres cache: %w", err)
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO meta (key, value) VALUES ('schema_version', $1)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, schemaVersion); err != nil {
		return fmt.Errorf("migrating postgres cache: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrating postgres cache: %w", err)
	}
	return nil
}

func lockedExec(db *sql.DB, stmt string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrating postgres cache: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, int64(pgSchemaLockID)); err != nil {
		return fmt.Errorf("locking postgres cache schema: %w", err)
	}
	if _, err := tx.Exec(stmt); err != nil {
		return fmt.Errorf("migrating postgres cache: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrating postgres cache: %w", err)
	}
	return nil
}

func (c *postgresStore) Close() error { return c.db.Close() }

// GetRepo returns the cached state for a repo, or nil if it has never been seen.
func (c *postgresStore) GetRepo(ref vcs.Ref) (*RepoState, error) {
	row := c.db.QueryRow(
		`SELECT provider, owner, name, etag, pushed_at, last_checked_at, last_pr_cursor
		 FROM repos WHERE provider = $1 AND owner = $2 AND name = $3`,
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
func (c *postgresStore) UpsertRepo(s RepoState) error {
	_, err := c.db.Exec(`
		INSERT INTO repos (provider, owner, name, etag, pushed_at, last_checked_at, last_pr_cursor)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (provider, owner, name) DO UPDATE SET
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
func (c *postgresStore) ListOpenWatchedPRs() ([]WatchedPR, error) {
	rows, err := c.db.Query(
		`SELECT provider, owner, name, number, updated_at, ci_state, is_open
		 FROM watched_prs WHERE is_open`)
	if err != nil {
		return nil, fmt.Errorf("listing watched PRs: %w", err)
	}
	defer rows.Close()

	var out []WatchedPR
	for rows.Next() {
		var p WatchedPR
		var ciState sql.NullString
		if err := rows.Scan(&p.Ref.Provider, &p.Ref.Owner, &p.Ref.Name, &p.Number, &p.UpdatedAt, &ciState, &p.IsOpen); err != nil {
			return nil, fmt.Errorf("scanning watched PR: %w", err)
		}
		p.CIState = ciState.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertWatchedPR records or updates a tracked PR's state.
func (c *postgresStore) UpsertWatchedPR(p WatchedPR) error {
	_, err := c.db.Exec(`
		INSERT INTO watched_prs (provider, owner, name, number, updated_at, ci_state, is_open)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (provider, owner, name, number) DO UPDATE SET
			updated_at = excluded.updated_at,
			ci_state = excluded.ci_state,
			is_open = excluded.is_open`,
		p.Ref.Provider, p.Ref.Owner, p.Ref.Name, p.Number, p.UpdatedAt, p.CIState, p.IsOpen)
	if err != nil {
		return fmt.Errorf("saving watched PR %s#%d: %w", p.Ref, p.Number, err)
	}
	return nil
}

// DeleteWatchedPR drops a PR from the watch list (e.g. once closed).
func (c *postgresStore) DeleteWatchedPR(ref vcs.PRRef) error {
	_, err := c.db.Exec(
		`DELETE FROM watched_prs WHERE provider = $1 AND owner = $2 AND name = $3 AND number = $4`,
		ref.Ref.Provider, ref.Ref.Owner, ref.Ref.Name, ref.Number)
	if err != nil {
		return fmt.Errorf("deleting watched PR %s#%d: %w", ref.Ref, ref.Number, err)
	}
	return nil
}
