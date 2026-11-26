package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/land"
)

// A file that an earlier release made has the tables and no schema_migrations. Open must keep its rows and bring it to the
// current version; a failure leaves a production state file unusable.
func TestOpenMigratesAFileOfTheEarlierRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		mirrorSchema + checkSchema + landSchema + userTokenSchema,
		`INSERT INTO lands (id, repo, ref, ended, body) VALUES ('l1', 'r', 'main', 1, '{"id":"l1","repo":"r","ref":"main","state":"landed"}')`,
		`INSERT INTO check_requests (id, repo, tree, commit_sha, check_json, check_hash, env, open) VALUES ('c1', 'r', 't', 'x', '{}', 'h', 'e', 0)`,
		`INSERT INTO mirrors (name) VALUES ('m')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = old.Close()

	now := time.Unix(1000, 0)
	s, err := Open(t.Context(), path, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var versions int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != len(migrations) {
		t.Fatalf("versions = %d (%v), want %d", versions, err, len(migrations))
	}
	if l, ok, err := s.Land(t.Context(), "l1"); err != nil || !ok || l.State != land.Landed {
		t.Fatalf("land = %+v, %v, %v", l, ok, err)
	}
	if ok, err := s.HasMirror(t.Context(), "m"); err != nil || !ok {
		t.Fatalf("mirror = %v, %v", ok, err)
	}
	if n, err := s.Prune(t.Context(), now); err != nil || n != 0 {
		t.Fatalf("prune at the upgrade = %d, %v: rows that predate the upgrade must outlive one retention period", n, err)
	}
	if n, err := s.Prune(t.Context(), now.Add(time.Second)); err != nil || n != 2 {
		t.Fatalf("prune after the upgrade = %d, %v, want the ended land and the closed request", n, err)
	}
}
