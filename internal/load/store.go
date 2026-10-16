package load

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	db "git.bytestone.uk/hum3/gobank-db"
)

// Run is one load test as gobank-deploy made it: where it ran, the books
// it ran against, and the climb. It is a workflow instance, RunID that
// instance's id. The figures are zero until their stage has run.
type Run struct {
	RunID       string
	Environment string
	Scale       string
	ServerType  string
	MemoryGB    float64
	Version     string
	CreatedAt   time.Time
	Customers   int // on the books when the load began
	Result
}

// MigrationsTable is where this component records its schema versions.
const MigrationsTable = "load_schema_migrations"

// Migrations is the load schema's history. Append to change it.
var Migrations = []db.Migration{
	{Version: 1, Name: "load tests", Phase: db.Expand, SQL: `
CREATE TABLE IF NOT EXISTS load_runs (
	run_id       VARCHAR(36) PRIMARY KEY,
	environment  VARCHAR(40) NOT NULL,
	scale        VARCHAR(40) NOT NULL,
	server_type  VARCHAR(40) NOT NULL,
	memory_gb    DOUBLE PRECISION NOT NULL,
	version      VARCHAR(50) NOT NULL,
	created_at   TIMESTAMPTZ NOT NULL,
	customers    INTEGER NOT NULL DEFAULT 0,
	budget_ns    BIGINT NOT NULL DEFAULT 0,
	capacity     INTEGER NOT NULL DEFAULT 0,
	breach       TEXT NOT NULL DEFAULT '',
	rungs        TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_load_runs_created ON load_runs(created_at);
`},
}

// Store keeps load tests in the database shared with the workflow runs.
type Store struct{ db *sql.DB }

// NewStore brings the load tables to the current version.
func NewStore(ctx context.Context, d *sql.DB) (*Store, error) {
	if _, err := db.ApplyTo(ctx, d, MigrationsTable, Migrations); err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	return &Store{db: d}, nil
}

// Create records a load test as it starts.
func (s *Store) Create(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO load_runs (run_id, environment, scale, server_type, memory_gb, version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, r.RunID, r.Environment, r.Scale, r.ServerType, r.MemoryGB, r.Version, r.CreatedAt.UTC())
	return err
}

// Update writes the books and the climb.
func (s *Store) Update(ctx context.Context, r Run) error {
	rungs, err := json.Marshal(r.Rungs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE load_runs SET customers = $1, budget_ns = $2, capacity = $3, breach = $4, rungs = $5 WHERE run_id = $6`,
		r.Customers, int64(r.Budget), r.Capacity, r.Breach, string(rungs), r.RunID)
	return err
}

// Get is the load test for a workflow run; nil when there is none.
func (s *Store) Get(ctx context.Context, runID string) (*Run, error) {
	list, err := s.query(ctx, selectRuns+` WHERE run_id = $1`, runID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// List is the most recent load tests, newest first.
func (s *Store) List(ctx context.Context, limit int) ([]Run, error) {
	return s.query(ctx, selectRuns+` ORDER BY created_at DESC LIMIT $1`, limit)
}

const selectRuns = `SELECT run_id, environment, scale, server_type, memory_gb, version, created_at, customers, budget_ns, capacity, breach, rungs FROM load_runs`

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var budget int64
		var rungs string
		if err := rows.Scan(&r.RunID, &r.Environment, &r.Scale, &r.ServerType, &r.MemoryGB, &r.Version, &r.CreatedAt,
			&r.Customers, &budget, &r.Capacity, &r.Breach, &rungs); err != nil {
			return nil, err
		}
		r.Budget = time.Duration(budget)
		if err := json.Unmarshal([]byte(rungs), &r.Rungs); err != nil {
			return nil, fmt.Errorf("load run %s rungs: %w", r.RunID, err)
		}
		if len(r.Rungs) == 0 {
			r.Rungs = nil
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
