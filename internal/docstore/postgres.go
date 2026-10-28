package docstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a Store backed by one table, console_docs.
type Postgres struct{ pool *pgxpool.Pool }

const schema = `
CREATE TABLE IF NOT EXISTS console_docs (
    kind       text        NOT NULL,
    id         text        NOT NULL,
    body       jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, id)
)`

// OpenPostgres connects and creates the table if it is missing.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("docstore: connecting to postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("docstore: preparing schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Create(ctx context.Context, kind, id string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO console_docs (kind, id, body) VALUES ($1, $2, $3)`, kind, id, body)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrExists
	}
	return err
}

func (p *Postgres) Put(ctx context.Context, kind, id string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO console_docs (kind, id, body) VALUES ($1, $2, $3)
		ON CONFLICT (kind, id) DO UPDATE SET body = EXCLUDED.body, updated_at = now()`, kind, id, body)
	return err
}

func (p *Postgres) Get(ctx context.Context, kind, id string, out any) error {
	var body []byte
	err := p.pool.QueryRow(ctx, `SELECT body FROM console_docs WHERE kind = $1 AND id = $2`, kind, id).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (p *Postgres) List(ctx context.Context, kind string, opts ListOptions) ([]Doc, error) {
	q := `SELECT id, body FROM console_docs WHERE kind = $1 ORDER BY id COLLATE "C"`
	if opts.Desc {
		q += ` DESC`
	}
	args := []any{kind}
	if opts.Limit > 0 {
		q += ` LIMIT $2`
		args = append(args, opts.Limit)
	}
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		d := Doc{Kind: kind}
		if err := rows.Scan(&d.ID, &d.Body); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Postgres) Delete(ctx context.Context, kind, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM console_docs WHERE kind = $1 AND id = $2`, kind, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) Count(ctx context.Context, kind string) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM console_docs WHERE kind = $1`, kind).Scan(&n)
	return n, err
}
