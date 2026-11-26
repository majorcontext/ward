package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const mirrorSchema = `
CREATE TABLE IF NOT EXISTS service (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	subscription_id TEXT NOT NULL,
	events_cursor TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS partitions (partition INTEGER PRIMARY KEY, last_seq INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS mirrors (name TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS seeds (mirror TEXT PRIMARY KEY, state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS seed_refs (mirror TEXT NOT NULL, ref TEXT NOT NULL, commit_sha TEXT NOT NULL, next TEXT NOT NULL, PRIMARY KEY (mirror, ref));
CREATE TABLE IF NOT EXISTS seed_cleanups (mirror TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS refs (
	mirror TEXT NOT NULL,
	ref TEXT NOT NULL,
	last_partition INTEGER NOT NULL DEFAULT -1,
	last_seq INTEGER NOT NULL DEFAULT 0,
	dirty INTEGER NOT NULL DEFAULT 0,
	version INTEGER NOT NULL DEFAULT 0,
	dirty_since INTEGER NOT NULL DEFAULT 0,
	pushed TEXT,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt INTEGER NOT NULL DEFAULT 0,
	failure TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	failure_since INTEGER NOT NULL DEFAULT 0,
	pushed_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (mirror, ref)
);`

// migrations is the history of the schema. A migration never changes after release: a change is a new entry. The first
// entry creates every table with IF NOT EXISTS, so a file that an earlier release made without schema_migrations
// takes it as its baseline.
var migrations = []string{
	mirrorSchema + checkSchema + landSchema + userTokenSchema + `
CREATE INDEX IF NOT EXISTS check_requests_open ON check_requests (open);`,
	`ALTER TABLE lands ADD COLUMN ended_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE check_requests ADD COLUMN closed_at INTEGER NOT NULL DEFAULT 0;`,
}

func migrate(ctx context.Context, db *sql.DB, now time.Time) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
		return err
	}
	if have > len(migrations) {
		return fmt.Errorf("the file has schema version %d and this release knows %d", have, len(migrations))
	}
	for v := have + 1; v <= len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if v == 2 {
			for _, q := range []string{
				`UPDATE lands SET ended_at = ? WHERE ended = 1`,
				`UPDATE check_requests SET closed_at = ? WHERE open = 0`,
			} {
				if _, err := tx.ExecContext(ctx, q, micro(now)); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("migration %d: %w", v, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, micro(now)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
