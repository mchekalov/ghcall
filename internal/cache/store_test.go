package cache

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ghcall/internal/config"
)

// storeFactory opens a fresh, empty Store for one subtest, or skips if the
// backend isn't available in this environment.
type storeFactory func(t *testing.T) Store

func sqliteFactory(t *testing.T) Store {
	t.Helper()
	cfg := config.CacheConfig{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "cache.db"),
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("opening sqlite store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// postgresFactory gives every subtest its own schema so the suite is
// re-runnable against a long-lived database.
func postgresFactory(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("GHCALL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("GHCALL_TEST_POSTGRES_DSN not set")
	}

	schema := fmt.Sprintf("ghcall_test_%d_%d", time.Now().UnixNano(), os.Getpid())
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer db.Close()
		db.Exec("DROP SCHEMA " + schema + " CASCADE")
	})

	t.Setenv("GHCALL_TEST_DSN_SCOPED", withSearchPath(dsn, schema))

	s, err := Open(config.CacheConfig{
		Driver:       config.DriverPostgres,
		DSNEnv:       "GHCALL_TEST_DSN_SCOPED",
		MaxOpenConns: 17,
	})
	if err != nil {
		t.Fatalf("opening postgres store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// withSearchPath pins a DSN to one schema, handling both the URL form
// ("postgres://…") and the keyword/value form ("host=… dbname=…").
func withSearchPath(dsn, schema string) string {
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + schema
	}
	sep := "?"
	if strings.Contains(dsn[strings.Index(dsn, "://")+3:], "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

func TestStoreConformance(t *testing.T) {
	drivers := map[string]storeFactory{
		"sqlite":   sqliteFactory,
		"postgres": postgresFactory,
	}

	cases := []struct {
		name string
		run  func(t *testing.T, s Store)
	}{
		{"GetRepoUnknown", testGetRepoUnknown},
		{"UpsertRepoRoundTrip", testUpsertRepoRoundTrip},
		{"UpsertRepoUpdatesInPlace", testUpsertRepoUpdatesInPlace},
		{"RepoNullColumnsScanEmpty", testRepoNullColumnsScanEmpty},
		{"ListOpenWatchedPRsExcludesClosed", testListOpenWatchedPRsExcludesClosed},
		{"UpsertWatchedPRUpdatesInPlace", testUpsertWatchedPRUpdatesInPlace},
		{"DeleteMissingWatchedPRIsNotAnError", testDeleteMissingWatchedPR},
		{"ConcurrentUpsertRepo", testConcurrentUpsertRepo},
	}

	for driver, factory := range drivers {
		t.Run(driver, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					tc.run(t, factory(t))
				})
			}
		})
	}
}

func testGetRepoUnknown(t *testing.T, s Store) {
	got, err := s.GetRepo("nobody", "nothing")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got != nil {
		t.Fatalf("GetRepo on unknown repo = %+v, want nil", got)
	}
}

func testUpsertRepoRoundTrip(t *testing.T, s Store) {
	// Truncated to a second: LastCheckedAt is stored as RFC3339 text.
	checked := time.Now().UTC().Truncate(time.Second)
	want := RepoState{
		Owner:         "mchekalov",
		Name:          "ghcall",
		ETag:          `W/"abc123"`,
		PushedAt:      "2026-09-22T10:00:00Z",
		LastCheckedAt: checked,
		LastPRCursor:  "Y3Vyc29yOnYyOpHOAA",
	}
	if err := s.UpsertRepo(want); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}

	got, err := s.GetRepo(want.Owner, want.Name)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got == nil {
		t.Fatal("GetRepo returned nil after upsert")
	}
	if got.Owner != want.Owner || got.Name != want.Name ||
		got.ETag != want.ETag || got.PushedAt != want.PushedAt ||
		got.LastPRCursor != want.LastPRCursor {
		t.Errorf("GetRepo = %+v, want %+v", *got, want)
	}
	if !got.LastCheckedAt.Equal(want.LastCheckedAt) {
		t.Errorf("LastCheckedAt = %v, want %v", got.LastCheckedAt, want.LastCheckedAt)
	}
}

func testUpsertRepoUpdatesInPlace(t *testing.T, s Store) {
	base := RepoState{Owner: "o", Name: "n", ETag: "first", LastCheckedAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.UpsertRepo(base); err != nil {
		t.Fatalf("first UpsertRepo: %v", err)
	}
	base.ETag = "second"
	base.LastPRCursor = "cursor"
	if err := s.UpsertRepo(base); err != nil {
		t.Fatalf("second UpsertRepo: %v", err)
	}

	got, err := s.GetRepo("o", "n")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got.ETag != "second" || got.LastPRCursor != "cursor" {
		t.Errorf("GetRepo = %+v, want the second write to have replaced the first", *got)
	}
}

func testRepoNullColumnsScanEmpty(t *testing.T, s Store) {
	// Everything but last_checked_at is nullable; empty strings must come
	// back as empty strings rather than failing the scan.
	if err := s.UpsertRepo(RepoState{Owner: "o", Name: "n", LastCheckedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	got, err := s.GetRepo("o", "n")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got.ETag != "" || got.PushedAt != "" || got.LastPRCursor != "" {
		t.Errorf("GetRepo = %+v, want empty etag/pushed_at/last_pr_cursor", *got)
	}
}

func testListOpenWatchedPRsExcludesClosed(t *testing.T, s Store) {
	open := WatchedPR{Owner: "o", Name: "n", Number: 1, UpdatedAt: "2026-09-22T10:00:00Z", CIState: "FAILURE", IsOpen: true}
	closed := WatchedPR{Owner: "o", Name: "n", Number: 2, UpdatedAt: "2026-09-22T11:00:00Z", CIState: "SUCCESS", IsOpen: false}
	for _, p := range []WatchedPR{open, closed} {
		if err := s.UpsertWatchedPR(p); err != nil {
			t.Fatalf("UpsertWatchedPR %d: %v", p.Number, err)
		}
	}

	got, err := s.ListOpenWatchedPRs()
	if err != nil {
		t.Fatalf("ListOpenWatchedPRs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListOpenWatchedPRs returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0] != open {
		t.Errorf("ListOpenWatchedPRs[0] = %+v, want %+v", got[0], open)
	}
}

func testUpsertWatchedPRUpdatesInPlace(t *testing.T, s Store) {
	p := WatchedPR{Owner: "o", Name: "n", Number: 7, UpdatedAt: "2026-09-22T10:00:00Z", CIState: "PENDING", IsOpen: true}
	if err := s.UpsertWatchedPR(p); err != nil {
		t.Fatalf("first UpsertWatchedPR: %v", err)
	}
	p.CIState = "FAILURE"
	p.UpdatedAt = "2026-09-22T12:00:00Z"
	if err := s.UpsertWatchedPR(p); err != nil {
		t.Fatalf("second UpsertWatchedPR: %v", err)
	}

	got, err := s.ListOpenWatchedPRs()
	if err != nil {
		t.Fatalf("ListOpenWatchedPRs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (upsert duplicated the row): %+v", len(got), got)
	}
	if got[0] != p {
		t.Errorf("row = %+v, want %+v", got[0], p)
	}

	if err := s.DeleteWatchedPR("o", "n", 7); err != nil {
		t.Fatalf("DeleteWatchedPR: %v", err)
	}
	got, err = s.ListOpenWatchedPRs()
	if err != nil {
		t.Fatalf("ListOpenWatchedPRs after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows after delete, want 0", len(got))
	}
}

func testDeleteMissingWatchedPR(t *testing.T, s Store) {
	if err := s.DeleteWatchedPR("o", "n", 999); err != nil {
		t.Errorf("DeleteWatchedPR on missing row = %v, want nil", err)
	}
}

// testConcurrentUpsertRepo reproduces the phase-1 shape: config's default
// max_in_flight goroutines all writing repo state at once.
func testConcurrentUpsertRepo(t *testing.T, s Store) {
	const n = 15
	var wg sync.WaitGroup
	errs := make([]error, n)
	now := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.UpsertRepo(RepoState{
				Owner:         "o",
				Name:          fmt.Sprintf("repo-%d", i),
				ETag:          fmt.Sprintf("etag-%d", i),
				LastCheckedAt: now,
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent UpsertRepo %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		got, err := s.GetRepo("o", fmt.Sprintf("repo-%d", i))
		if err != nil {
			t.Fatalf("GetRepo repo-%d: %v", i, err)
		}
		if got == nil || got.ETag != fmt.Sprintf("etag-%d", i) {
			t.Errorf("repo-%d = %+v, want etag-%d", i, got, i)
		}
	}
}
