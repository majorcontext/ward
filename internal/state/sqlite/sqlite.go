// Package sqlite is the SQLite state store of ward: the mirror, the check results and the lands.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"

	"github.com/majorcontext/motte/api"

	"github.com/majorcontext/ward/internal/mirror"
)

// Store is the state of ward on one SQLite file: the mirror, the check requests and results, the lands and the user tokens.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Option changes how Open builds a Store.
type Option func(*Store)

// WithClock sets the clock that stamps the end of a land and the close of a check request. The default is the wall clock.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

var _ mirror.Store = (*Store)(nil)

// Open opens the database at path and applies the migrations that it has not seen.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	q := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "temp_store(MEMORY)"}, "_txlock": {"immediate"}}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	if err := migrate(ctx, db, s.now()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: schema: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func micro(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

func fromMicro(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMicro(v).UTC()
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sqlite: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	return nil
}

func (s *Store) Service(ctx context.Context) (mirror.Service, error) {
	out := mirror.Service{LastSeq: map[int]int64{}}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT subscription_id, events_cursor FROM service`).Scan(&out.SubscriptionID, &out.EventsCursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT partition, last_seq FROM partitions`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p int
			var seq int64
			if err := rows.Scan(&p, &seq); err != nil {
				return err
			}
			out.LastSeq[p] = seq
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) SetSubscription(ctx context.Context, id, cursor string, clearSeq bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO service (id, subscription_id, events_cursor) VALUES (1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET subscription_id = excluded.subscription_id, events_cursor = excluded.events_cursor`, id, cursor); err != nil {
			return err
		}
		if clearSeq {
			_, err := tx.ExecContext(ctx, `DELETE FROM partitions`)
			return err
		}
		return nil
	})
}

// advance stores the sequence of an event in its partition when it is new, and says how it compared.
func advance(ctx context.Context, tx *sql.Tx, e api.Event) (mirror.HookResult, error) {
	var last int64
	err := tx.QueryRowContext(ctx, `SELECT last_seq FROM partitions WHERE partition = ?`, e.Partition).Scan(&last)
	res := mirror.HookInOrder
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res = mirror.HookBaseline
	case err != nil:
		return 0, err
	case e.Seq <= last:
		return mirror.HookRepeat, nil
	case e.Seq > last+1:
		res = mirror.HookGap
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO partitions (partition, last_seq) VALUES (?, ?)
		ON CONFLICT (partition) DO UPDATE SET last_seq = excluded.last_seq`, e.Partition, e.Seq)
	return res, err
}

func trigger(ctx context.Context, tx *sql.Tx, mirrorName, ref string, partition int, seq int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO refs (mirror, ref, last_partition, last_seq, dirty, version, dirty_since, next_attempt)
		VALUES (?, ?, ?, ?, 1, 1, ?, ?)
		ON CONFLICT (mirror, ref) DO UPDATE SET
			dirty = 1,
			version = version + 1,
			dirty_since = CASE WHEN dirty = 1 THEN dirty_since ELSE excluded.dirty_since END,
			next_attempt = excluded.next_attempt,
			last_partition = CASE WHEN excluded.last_partition >= 0 AND (last_partition <> excluded.last_partition OR excluded.last_seq > last_seq)
				THEN excluded.last_partition ELSE last_partition END,
			last_seq = CASE WHEN excluded.last_partition >= 0 AND (last_partition <> excluded.last_partition OR excluded.last_seq > last_seq)
				THEN excluded.last_seq ELSE last_seq END`,
		mirrorName, ref, partition, seq, micro(now), micro(now))
	return err
}

func (s *Store) RecordHook(ctx context.Context, hit mirror.Hit, now time.Time) (mirror.HookResult, error) {
	var res mirror.HookResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if res, err = advance(ctx, tx, hit.Event); err != nil || res == mirror.HookRepeat {
			return err
		}
		for _, m := range hit.Mirrors {
			if err := trigger(ctx, tx, m, hit.Event.Ref, hit.Event.Partition, hit.Event.Seq, now); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

func (s *Store) RecordPage(ctx context.Context, hits []mirror.Hit, cursor string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, h := range hits {
			e := h.Event
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO partitions (partition, last_seq) VALUES (?, ?)
				ON CONFLICT (partition) DO UPDATE SET last_seq = MAX(last_seq, excluded.last_seq)`, e.Partition, e.Seq); err != nil {
				return err
			}
			for _, m := range h.Mirrors {
				if err := trigger(ctx, tx, m, e.Ref, e.Partition, e.Seq, now); err != nil {
					return err
				}
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE service SET events_cursor = ?`, cursor)
		return err
	})
}

const refColumns = `mirror, ref, last_partition, last_seq, dirty, version, dirty_since, pushed, attempts, next_attempt, failure, message, failure_since, pushed_at`

func scanRefs(rows *sql.Rows) ([]mirror.Ref, error) {
	defer func() { _ = rows.Close() }()
	var out []mirror.Ref
	for rows.Next() {
		var r mirror.Ref
		var pushed sql.NullString
		var dirty, dirtySince, next, failSince, pushedAt int64
		var failure string
		if err := rows.Scan(&r.Mirror, &r.Name, &r.LastPartition, &r.LastSeq, &dirty, &r.Version, &dirtySince, &pushed,
			&r.Attempts, &next, &failure, &r.Message, &failSince, &pushedAt); err != nil {
			return nil, err
		}
		r.Dirty, r.DirtySince, r.Pushed, r.PushedSet = dirty == 1, fromMicro(dirtySince), pushed.String, pushed.Valid
		r.NextAttempt, r.Failure, r.FailureSince, r.PushedAt = fromMicro(next), mirror.Class(failure), fromMicro(failSince), fromMicro(pushedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Due(ctx context.Context, name string, now time.Time) ([]mirror.Ref, time.Time, error) {
	var due []mirror.Ref
	var next time.Time
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+refColumns+` FROM refs WHERE mirror = ? AND dirty = 1 AND next_attempt <= ? ORDER BY ref`, name, micro(now))
		if err != nil {
			return err
		}
		if due, err = scanRefs(rows); err != nil {
			return err
		}
		var later sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(next_attempt) FROM refs WHERE mirror = ? AND dirty = 1 AND next_attempt > ?`, name, micro(now)).Scan(&later); err != nil {
			return err
		}
		if later.Valid {
			next = fromMicro(later.Int64)
		}
		return nil
	})
	return due, next, err
}

func (s *Store) exec(ctx context.Context, q string, args ...any) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	})
}

func (s *Store) RecordPush(ctx context.Context, name, ref string, version int64, pushed string, now time.Time) error {
	return s.exec(ctx, `
		UPDATE refs SET pushed = ?, pushed_at = ?, failure = '', message = '', failure_since = 0, attempts = 0,
			dirty = CASE WHEN version = ? THEN 0 ELSE dirty END
		WHERE mirror = ? AND ref = ?`, pushed, micro(now), version, name, ref)
}

func (s *Store) RecordFailure(ctx context.Context, name, ref string, version int64, class mirror.Class, message string, next, now time.Time) error {
	return s.exec(ctx, `
		UPDATE refs SET failure = ?, message = ?, failure_since = CASE WHEN failure = '' THEN ? ELSE failure_since END,
			attempts = attempts + 1, next_attempt = CASE WHEN version = ? THEN ? ELSE next_attempt END
		WHERE mirror = ? AND ref = ?`, string(class), message, micro(now), version, micro(next), name, ref)
}

func (s *Store) SetPushed(ctx context.Context, name, ref, pushed string) error {
	return s.exec(ctx, `UPDATE refs SET pushed = ? WHERE mirror = ? AND ref = ?`, pushed, name, ref)
}

func (s *Store) Reconciled(ctx context.Context, name string, targets map[string]string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for ref, pushed := range targets {
			if err := trigger(ctx, tx, name, ref, -1, 0, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE refs SET pushed = ? WHERE mirror = ? AND ref = ?`, pushed, name, ref); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mirrors (name) VALUES (?)`, name)
		return err
	})
}

func (s *Store) HasMirror(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirrors WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

func (s *Store) Seed(ctx context.Context, name string) (string, error) {
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM seeds WHERE mirror = ?`, name).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}

func (s *Store) SetSeed(ctx context.Context, name, state string) error {
	return s.exec(ctx, `INSERT INTO seeds (mirror, state) VALUES (?, ?) ON CONFLICT (mirror) DO UPDATE SET state = excluded.state`, name, state)
}

func (s *Store) SeedRefs(ctx context.Context, name string) (map[string]mirror.SeedRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref, commit_sha, next FROM seed_refs WHERE mirror = ?`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]mirror.SeedRef{}
	for rows.Next() {
		var ref string
		var r mirror.SeedRef
		if err := rows.Scan(&ref, &r.Commit, &r.Next); err != nil {
			return nil, err
		}
		out[ref] = r
	}
	return out, rows.Err()
}

func (s *Store) SetSeedRef(ctx context.Context, name, ref string, r mirror.SeedRef) error {
	return s.exec(ctx, `INSERT INTO seed_refs (mirror, ref, commit_sha, next) VALUES (?, ?, ?, ?)
		ON CONFLICT (mirror, ref) DO UPDATE SET commit_sha = excluded.commit_sha, next = excluded.next`, name, ref, r.Commit, r.Next)
}

func (s *Store) SeedCleaned(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM seed_cleanups WHERE mirror = ?`, name).Scan(&n)
	return n > 0, err
}

func (s *Store) FinishSeedCleanup(ctx context.Context, name string, refs []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, ref := range refs {
			for _, table := range []string{"refs", "seed_refs"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE mirror = ? AND ref = ?`, name, ref); err != nil {
					return err
				}
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO seed_cleanups (mirror) VALUES (?)`, name)
		return err
	})
}

func (s *Store) StateRefs(ctx context.Context, name string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref FROM refs WHERE mirror = ? ORDER BY ref`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Refs(ctx context.Context, name, after string, limit int) ([]mirror.Ref, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+refColumns+` FROM refs WHERE mirror = ? AND ref > ? ORDER BY ref LIMIT ?`, name, after, limit)
	if err != nil {
		return nil, err
	}
	return scanRefs(rows)
}

func (s *Store) Stats(ctx context.Context) ([]mirror.Stats, error) {
	var out []mirror.Stats
	err := s.tx(ctx, func(tx *sql.Tx) error {
		byName := map[string]*mirror.Stats{}
		get := func(n string) *mirror.Stats {
			if byName[n] == nil {
				byName[n] = &mirror.Stats{Mirror: n, Failing: map[mirror.Class]int{}}
			}
			return byName[n]
		}
		rows, err := tx.QueryContext(ctx, `SELECT name FROM mirrors`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()
				return err
			}
			get(n).Reconciled = true
		}
		_ = rows.Close()
		rows, err = tx.QueryContext(ctx, `SELECT mirror, dirty, failure, COUNT(*), MIN(CASE WHEN dirty = 1 THEN dirty_since END) FROM refs GROUP BY mirror, dirty, failure`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var n, failure string
			var dirty, count int
			var oldest sql.NullInt64
			if err := rows.Scan(&n, &dirty, &failure, &count, &oldest); err != nil {
				return err
			}
			st := get(n)
			if dirty == 1 {
				st.Dirty += count
				if t := fromMicro(oldest.Int64); oldest.Valid && (st.OldestDirty.IsZero() || t.Before(st.OldestDirty)) {
					st.OldestDirty = t
				}
			}
			if failure != "" {
				st.Failing[mirror.Class(failure)] += count
			}
		}
		for _, st := range byName {
			out = append(out, *st)
		}
		return rows.Err()
	})
	return out, err
}

// Prune deletes the lands that ended and the check requests that closed before the time before, and the leases that
// expired before it and belong to no open request. A lease of an open request stays, because Leased and RenewLease read
// it. It returns the number of rows deleted.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM check_requests WHERE open = 0 AND closed_at < ?`,
			`DELETE FROM check_leases WHERE until < ? AND NOT EXISTS (SELECT 1 FROM check_requests r WHERE r.id = request_id AND r.open = 1)`,
			`DELETE FROM lands WHERE ended = 1 AND ended_at < ?`,
		} {
			res, err := tx.ExecContext(ctx, q, micro(before))
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return nil
	})
	return total, err
}
