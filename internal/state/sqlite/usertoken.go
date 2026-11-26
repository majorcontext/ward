package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/identity/usertoken"
)

const userTokenSchema = `
CREATE TABLE IF NOT EXISTS user_tokens (
	id TEXT PRIMARY KEY,
	hash TEXT NOT NULL UNIQUE,
	principal TEXT NOT NULL,
	scope TEXT NOT NULL,
	repos TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0
);`

const userTokenColumns = `id, principal, scope, repos, created_at, expires_at, revoked_at`

var _ usertoken.Store = (*Store)(nil)

func (s *Store) CreateUserToken(ctx context.Context, hash string, r usertoken.Record) error {
	repos, err := json.Marshal(append([]string{}, r.Repos...))
	if err != nil {
		return err
	}
	err = s.exec(ctx, `INSERT INTO user_tokens (id, hash, principal, scope, repos, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, hash, r.Principal, r.Scope, string(repos), micro(r.CreatedAt), micro(r.ExpiresAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return usertoken.ErrExists
	}
	return err
}

func scanUserToken(row scanner) (usertoken.Record, error) {
	var r usertoken.Record
	var repos string
	var created, expires, revoked int64
	if err := row.Scan(&r.ID, &r.Principal, &r.Scope, &repos, &created, &expires, &revoked); err != nil {
		return usertoken.Record{}, err
	}
	r.CreatedAt, r.ExpiresAt, r.RevokedAt = fromMicro(created), fromMicro(expires), fromMicro(revoked)
	return r, json.Unmarshal([]byte(repos), &r.Repos)
}

func (s *Store) UserTokenByHash(ctx context.Context, hash string) (usertoken.Record, error) {
	r, err := scanUserToken(s.db.QueryRowContext(ctx, `SELECT `+userTokenColumns+` FROM user_tokens WHERE hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return usertoken.Record{}, usertoken.ErrNotFound
	}
	return r, err
}

func (s *Store) UserTokens(ctx context.Context) ([]usertoken.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userTokenColumns+` FROM user_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []usertoken.Record
	for rows.Next() {
		r, err := scanUserToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RevokeUserToken(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE user_tokens SET revoked_at = CASE WHEN revoked_at = 0 THEN ? ELSE revoked_at END WHERE id = ?`, micro(at), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return usertoken.ErrNotFound
	}
	return nil
}
