package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Postgres is a Store backed by PostgreSQL.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects, applies any pending migrations, and returns the store. url is a
// libpq-style connection string or URL.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// migrationLockID is an arbitrary constant so concurrent servers apply migrations one at a time.
const migrationLockID = 727274

func (p *Postgres) migrate(ctx context.Context) error {
	type migration struct {
		version int
		name    string
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	var all []migration
	for _, e := range entries {
		num, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("migration %q: name must start with a number", e.Name())
		}
		all = append(all, migration{v, e.Name()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY, applied_at timestamptz NOT NULL)`); err != nil {
		return err
	}
	for _, m := range all {
		var applied bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", m.version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrationFiles.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("applying migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)", m.version, now()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

const runCols = `id, pipeline_name, pipeline_yaml, status, reason, source_repo, source_ref, source_sha,
	trigger, created_at, started_at, finished_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var status, trigger string
	err := row.Scan(&r.ID, &r.PipelineName, &r.PipelineYAML, &status, &r.Reason, &r.SourceRepo,
		&r.SourceRef, &r.SourceSHA, &trigger, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	r.Status, r.Trigger = RunStatus(status), Trigger(trigger)
	r.CreatedAt = r.CreatedAt.UTC()
	r.StartedAt, r.FinishedAt = utc(r.StartedAt), utc(r.FinishedAt)
	return r, nil
}

const stepCols = `run_id, step_id, status, exit_code, error, started_at, finished_at`

func scanStep(row pgx.Row) (Step, error) {
	var s Step
	var status string
	err := row.Scan(&s.RunID, &s.StepID, &status, &s.ExitCode, &s.Error, &s.StartedAt, &s.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, err
	}
	s.Status = StepStatus(status)
	s.StartedAt, s.FinishedAt = utc(s.StartedAt), utc(s.FinishedAt)
	return s, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (p *Postgres) CreateRun(ctx context.Context, r Run, stepIDs []string) (Run, error) {
	r.ID = newID()
	r.CreatedAt = now()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO runs (id, pipeline_name, pipeline_yaml, status, source_repo,
		source_ref, source_sha, trigger, created_at) VALUES ($1, $2, $3, 'queued', $4, $5, $6, $7, $8)`,
		r.ID, r.PipelineName, r.PipelineYAML, r.SourceRepo, r.SourceRef, r.SourceSHA, string(r.Trigger), r.CreatedAt)
	if err != nil {
		return Run{}, fmt.Errorf("inserting run: %w", err)
	}
	for i, id := range stepIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO steps (run_id, step_id, ord, status) VALUES ($1, $2, $3, 'pending')`,
			r.ID, id, i); err != nil {
			return Run{}, fmt.Errorf("inserting step %q: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Run{}, err
	}
	r.Status = RunQueued
	r.StartedAt, r.FinishedAt = nil, nil
	return r, nil
}

func (p *Postgres) GetRun(ctx context.Context, id string) (Run, []Step, error) {
	r, err := scanRun(p.pool.QueryRow(ctx, "SELECT "+runCols+" FROM runs WHERE id = $1", id))
	if err != nil {
		return Run{}, nil, err
	}
	rows, err := p.pool.Query(ctx, "SELECT "+stepCols+" FROM steps WHERE run_id = $1 ORDER BY ord", id)
	if err != nil {
		return Run{}, nil, err
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		s, err := scanStep(rows)
		if err != nil {
			return Run{}, nil, err
		}
		steps = append(steps, s)
	}
	return r, steps, rows.Err()
}

func (p *Postgres) ListRuns(ctx context.Context, opts ListOptions) ([]Run, error) {
	query := "SELECT " + runCols + " FROM runs"
	var args []any
	if opts.Before != "" {
		var seq int64
		err := p.pool.QueryRow(ctx, "SELECT seq FROM runs WHERE id = $1", opts.Before).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("before %q: %w", opts.Before, ErrNotFound)
		}
		if err != nil {
			return nil, err
		}
		query += " WHERE seq < $1"
		args = append(args, seq)
	}
	query += " ORDER BY seq DESC LIMIT " + strconv.Itoa(opts.limit())
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) ClaimQueuedRun(ctx context.Context) (Run, bool, error) {
	r, err := scanRun(p.pool.QueryRow(ctx, `UPDATE runs SET status = 'running', started_at = $1
		WHERE id = (SELECT id FROM runs WHERE status = 'queued' ORDER BY seq LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+runCols, now()))
	if errors.Is(err, ErrNotFound) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

func (p *Postgres) UpdateRun(ctx context.Context, id string, u RunUpdate) (Run, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(ctx)
	var cur string
	err = tx.QueryRow(ctx, "SELECT status FROM runs WHERE id = $1 FOR UPDATE", id).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if !RunStatus(cur).canMoveTo(u.Status) {
		return Run{}, fmt.Errorf("run %s: %s -> %s: %w", id, cur, u.Status, ErrInvalidTransition)
	}
	t := now()
	r, err := scanRun(tx.QueryRow(ctx, `UPDATE runs SET status = $2,
		reason = CASE WHEN $3 <> '' THEN $3 ELSE reason END,
		started_at = CASE WHEN $2 = 'running' THEN $4 ELSE started_at END,
		finished_at = CASE WHEN $2 IN ('succeeded', 'failed', 'canceled') THEN $4 ELSE finished_at END
		WHERE id = $1 RETURNING `+runCols, id, string(u.Status), u.Reason, t))
	if err != nil {
		return Run{}, err
	}
	return r, tx.Commit(ctx)
}

func (p *Postgres) UpdateStep(ctx context.Context, runID, stepID string, u StepUpdate) (Step, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Step{}, err
	}
	defer tx.Rollback(ctx)
	var cur string
	err = tx.QueryRow(ctx, "SELECT status FROM steps WHERE run_id = $1 AND step_id = $2 FOR UPDATE", runID, stepID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, err
	}
	if !StepStatus(cur).canMoveTo(u.Status) {
		return Step{}, fmt.Errorf("step %s/%s: %s -> %s: %w", runID, stepID, cur, u.Status, ErrInvalidTransition)
	}
	s, err := scanStep(tx.QueryRow(ctx, `UPDATE steps SET status = $3, exit_code = $4, error = $5,
		started_at = CASE WHEN $3 = 'running' THEN $6 ELSE started_at END,
		finished_at = CASE WHEN $3 NOT IN ('pending', 'running') THEN $6 ELSE finished_at END
		WHERE run_id = $1 AND step_id = $2 RETURNING `+stepCols,
		runID, stepID, string(u.Status), u.ExitCode, u.Error, now()))
	if err != nil {
		return Step{}, err
	}
	return s, tx.Commit(ctx)
}

func (p *Postgres) AppendLog(ctx context.Context, runID, stepID string, data []byte) (int64, error) {
	// Locking the step row serializes writers to one step's log, so MAX(seq)+1 cannot collide.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var locked int
	err = tx.QueryRow(ctx, "SELECT 1 FROM steps WHERE run_id = $1 AND step_id = $2 FOR UPDATE", runID, stepID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	var seq int64
	err = tx.QueryRow(ctx, `INSERT INTO log_chunks (run_id, step_id, seq, data, created_at)
		SELECT $1::text, $2::text, COALESCE(MAX(seq), 0) + 1, $3::bytea, $4::timestamptz
		FROM log_chunks WHERE run_id = $1 AND step_id = $2 RETURNING seq`,
		runID, stepID, data, now()).Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit(ctx)
}

func (p *Postgres) ReadLog(ctx context.Context, runID, stepID string, afterSeq int64, limit int) ([]LogChunk, error) {
	var exists bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM steps WHERE run_id = $1 AND step_id = $2)", runID, stepID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	query := `SELECT seq, data, created_at FROM log_chunks WHERE run_id = $1 AND step_id = $2 AND seq > $3 ORDER BY seq`
	if limit > 0 {
		query += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := p.pool.Query(ctx, query, runID, stepID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogChunk
	for rows.Next() {
		c := LogChunk{RunID: runID, StepID: stepID}
		if err := rows.Scan(&c.Seq, &c.Data, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.CreatedAt = c.CreatedAt.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *Postgres) FailInterruptedRuns(ctx context.Context, reason string) (int, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	t := now()
	rows, err := tx.Query(ctx, `UPDATE runs SET status = 'failed', reason = $1, finished_at = $2
		WHERE status = 'running' RETURNING id`, reason, t)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE steps SET status = 'canceled', finished_at = $2
			WHERE run_id = ANY($1) AND status IN ('pending', 'running')`, ids, t); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}
