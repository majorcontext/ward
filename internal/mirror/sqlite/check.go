package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
CREATE INDEX IF NOT EXISTS check_results_key ON check_results (repo, tree, check_hash, env);`

var _ check.Store = (*Store)(nil)

const requestColumns = `id, repo, tree, commit_sha, check_json, check_hash, env, accepted_at, open`

type scanner interface{ Scan(...any) error }

func scanRequest(row scanner) (check.Request, error) {
	var r check.Request
	var raw string
	var accepted int64
	if err := row.Scan(&r.ID, &r.Repo, &r.Tree, &r.Commit, &raw, &r.Hash, &r.Env, &accepted, &r.Open); err != nil {
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
	_, err = tx.ExecContext(ctx, `INSERT INTO check_requests (`+requestColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		r.ID, r.Repo, r.Tree, r.Commit, string(raw), r.Hash, r.Env, micro(r.AcceptedAt))
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

func (s *Store) SetAccepted(ctx context.Context, id string, at time.Time) error {
	return s.exec(ctx, `UPDATE check_requests SET accepted_at = ? WHERE id = ?`, micro(at), id)
}

func (s *Store) Supersede(ctx context.Context, old string, next check.Request) (bool, error) {
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
		if _, err := tx.ExecContext(ctx, `UPDATE check_requests SET open = 0 WHERE id = ?`, old); err != nil {
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
		if _, err := tx.ExecContext(ctx, `UPDATE check_requests SET open = 0 WHERE id = ?`, r.RequestID); err != nil {
			return err
		}
		if r.Status == check.StatusPass {
			_, err = tx.ExecContext(ctx, `UPDATE check_requests SET open = 0 WHERE repo = ? AND tree = ? AND check_hash = ? AND env = ?`, r.Repo, r.Tree, r.Hash, r.Env)
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
