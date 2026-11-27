package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/check"
)

const checkSchema = `
CREATE TABLE IF NOT EXISTS check_requests (
	id TEXT PRIMARY KEY,
	repo TEXT NOT NULL,
	tree TEXT NOT NULL,
	commit_sha TEXT NOT NULL,
	check_json TEXT NOT NULL,
	check_hash TEXT NOT NULL,
	env TEXT NOT NULL,
	accepted_at INTEGER NOT NULL DEFAULT 0,
	open INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS check_requests_key ON check_requests (repo, tree, check_hash, env, open);
CREATE TABLE IF NOT EXISTS check_results (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL UNIQUE,
	repo TEXT NOT NULL,
	tree TEXT NOT NULL,
	check_hash TEXT NOT NULL,
	env TEXT NOT NULL,
	status TEXT NOT NULL,
	summary TEXT NOT NULL,
	log_url TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS check_results_key ON check_results (repo, tree, check_hash, env);
CREATE TABLE IF NOT EXISTS check_leases (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	lease_id TEXT NOT NULL UNIQUE,
	request_id TEXT NOT NULL,
	kid TEXT NOT NULL,
	until INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS check_leases_request ON check_leases (request_id, seq);`

var _ check.Store = (*Store)(nil)

const requestColumns = `id, repo, tree, commit_sha, check_json, check_hash, env, accepted_at, open, base, base_tree`

type scanner interface{ Scan(...any) error }

func scanRequest(row scanner) (check.Request, error) {
	var r check.Request
	var raw string
	var accepted int64
	if err := row.Scan(&r.ID, &r.Repo, &r.Tree, &r.Commit, &raw, &r.Hash, &r.Env, &accepted, &r.Open, &r.Base, &r.BaseTree); err != nil {
		return check.Request{}, err
	}
	r.AcceptedAt = fromMicro(accepted)
	return r, json.Unmarshal([]byte(raw), &r.Check)
}

func insertRequest(ctx context.Context, tx *sql.Tx, r check.Request) error {
	raw, err := json.Marshal(r.Check)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO check_requests (`+requestColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		r.ID, r.Repo, r.Tree, r.Commit, string(raw), r.Hash, r.Env, micro(r.AcceptedAt), r.Base, r.BaseTree)
	return err
}

func (s *Store) OpenRequest(ctx context.Context, r check.Request) (check.Request, bool, error) {
	out, created := r, false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		cur, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM check_requests
			WHERE repo = ? AND tree = ? AND check_hash = ? AND env = ? AND open = 1 ORDER BY rowid LIMIT 1`, r.Repo, r.Tree, r.Hash, r.Env))
		if err == nil {
			out = cur
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var passed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_results WHERE repo = ? AND tree = ? AND check_hash = ? AND env = ? AND status = 'pass'`,
			r.Repo, r.Tree, r.Hash, r.Env).Scan(&passed); err != nil || passed > 0 {
			return err
		}
		created = true
		return insertRequest(ctx, tx, r)
	})
	return out, created, err
}

func (s *Store) Request(ctx context.Context, id string) (check.Request, bool, error) {
	r, err := scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM check_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return check.Request{}, false, nil
	}
	return r, err == nil, err
}

func (s *Store) OpenRequests(ctx context.Context) ([]check.Request, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestColumns+` FROM check_requests WHERE open = 1 ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []check.Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Supersede(ctx context.Context, old string, next check.Request, now time.Time) (bool, error) {
	done := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var open, passed int
		if err := tx.QueryRowContext(ctx, `SELECT open, (SELECT COUNT(*) FROM check_results WHERE repo = r.repo AND tree = r.tree AND check_hash = r.check_hash AND env = r.env AND status = 'pass')
			FROM check_requests r WHERE id = ?`, old).Scan(&open, &passed); err != nil {
			return err
		}
		if open == 0 || passed > 0 {
			return nil
		}
		var until int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(until), 0) FROM check_leases WHERE request_id = ? AND until > ?`, old, micro(now)).Scan(&until); err != nil {
			return err
		}
		if until > 0 {
			return &check.LeasedError{Until: fromMicro(until)}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE check_requests SET open = 0, closed_at = ? WHERE id = ?`, micro(now), old); err != nil {
			return err
		}
		done = true
		return insertRequest(ctx, tx, next)
	})
	return done, err
}

func (s *Store) AddResult(ctx context.Context, r check.Result) (bool, error) {
	added := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO check_results (request_id, repo, tree, check_hash, env, status, summary, log_url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (request_id) DO NOTHING`,
			r.RequestID, r.Repo, r.Tree, r.Hash, r.Env, string(r.Status), r.Summary, r.LogURL)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		added = true
		if _, err := tx.ExecContext(ctx, `UPDATE check_requests SET open = 0, closed_at = ? WHERE id = ?`, micro(s.now()), r.RequestID); err != nil {
			return err
		}
		if r.Status == check.StatusPass {
			_, err = tx.ExecContext(ctx, `UPDATE check_requests SET open = 0, closed_at = ? WHERE open = 1 AND repo = ? AND tree = ? AND check_hash = ? AND env = ?`, micro(s.now()), r.Repo, r.Tree, r.Hash, r.Env)
		}
		return err
	})
	return added, err
}

func (s *Store) Results(ctx context.Context, repo, tree, hash, env string) ([]check.Result, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, request_id, status, summary, log_url FROM check_results
		WHERE repo = ? AND tree = ? AND check_hash = ? AND env = ? ORDER BY id`, repo, tree, hash, env)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []check.Result
	for rows.Next() {
		r := check.Result{Repo: repo, Tree: tree, Hash: hash, Env: env}
		var status string
		if err := rows.Scan(&r.ID, &r.RequestID, &status, &r.Summary, &r.LogURL); err != nil {
			return nil, err
		}
		r.Status = check.Status(status)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MaxResultID(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM check_results`).Scan(&n)
	return n, err
}

func (s *Store) Lease(ctx context.Context, repos []string, kid, leaseID string, now, until time.Time) (check.Request, bool, error) {
	if len(repos) == 0 {
		return check.Request{}, false, nil
	}
	var out check.Request
	leased := false
	args := make([]any, 0, len(repos)+1)
	for _, r := range repos {
		args = append(args, r)
	}
	args = append(args, micro(now))
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM check_requests r
			WHERE open = 1 AND repo IN (?`+strings.Repeat(`, ?`, len(repos)-1)+`)
			AND NOT EXISTS (SELECT 1 FROM check_leases l WHERE l.request_id = r.id AND l.until > ?)
			ORDER BY rowid LIMIT 1`, args...))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO check_leases (lease_id, request_id, kid, until) VALUES (?, ?, ?, ?)`, leaseID, r.ID, kid, micro(until)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE check_requests SET accepted_at = ? WHERE id = ?`, micro(now), r.ID); err != nil {
			return err
		}
		r.AcceptedAt = now
		out, leased = r, true
		return nil
	})
	return out, leased, err
}

func (s *Store) RenewLease(ctx context.Context, leaseID, kid string, repos []string, until time.Time) (bool, error) {
	if len(repos) == 0 {
		return false, nil
	}
	args := []any{micro(until), leaseID, kid}
	for _, r := range repos {
		args = append(args, r)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE check_leases SET until = ? WHERE lease_id = ? AND kid = ?
		AND EXISTS (SELECT 1 FROM check_requests r WHERE r.id = check_leases.request_id AND r.open = 1 AND r.repo IN (?`+strings.Repeat(`, ?`, len(repos)-1)+`))
		AND NOT EXISTS (SELECT 1 FROM check_leases n WHERE n.request_id = check_leases.request_id AND n.seq > check_leases.seq)`,
		args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) Leased(ctx context.Context, requestID, kid string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_leases WHERE request_id = ? AND kid = ?`, requestID, kid).Scan(&n)
	return n > 0, err
}
