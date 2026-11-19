package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/majorcontext/ward/internal/land"
)

const landSchema = `
CREATE TABLE IF NOT EXISTS lands (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL UNIQUE,
	repo TEXT NOT NULL,
	ref TEXT NOT NULL,
	ended INTEGER NOT NULL DEFAULT 0,
	body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS lands_queue ON lands (repo, ref, ended, seq);`

var _ land.Store = (*Store)(nil)

func decodeLand(row scanner) (land.Land, error) {
	var raw string
	if err := row.Scan(&raw); err != nil {
		return land.Land{}, err
	}
	var l land.Land
	err := json.Unmarshal([]byte(raw), &l)
	return l, err
}

func (s *Store) InsertLand(ctx context.Context, l land.Land) error {
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO lands (id, repo, ref, ended, body) VALUES (?, ?, ?, 0, ?)`, l.ID, l.Repo, l.Ref, string(raw))
}

func (s *Store) Land(ctx context.Context, id string) (land.Land, bool, error) {
	l, err := decodeLand(s.db.QueryRowContext(ctx, `SELECT body FROM lands WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return land.Land{}, false, nil
	}
	return l, err == nil, err
}

func (s *Store) NextLand(ctx context.Context, key land.Key) (land.Land, bool, error) {
	l, err := decodeLand(s.db.QueryRowContext(ctx, `SELECT body FROM lands WHERE repo = ? AND ref = ? AND ended = 0 ORDER BY seq LIMIT 1`, key.Repo, key.Ref))
	if errors.Is(err, sql.ErrNoRows) {
		return land.Land{}, false, nil
	}
	return l, err == nil, err
}

func (s *Store) SaveLand(ctx context.Context, l land.Land) error {
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE lands SET ended = ?, body = ? WHERE id = ?`, l.State.Ended(), string(raw), l.ID)
}

func (s *Store) UnfinishedKeys(ctx context.Context) ([]land.Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT repo, ref FROM lands WHERE ended = 0`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []land.Key
	for rows.Next() {
		var k land.Key
		if err := rows.Scan(&k.Repo, &k.Ref); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
