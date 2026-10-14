package scenario

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	db "git.bytestone.uk/hum3/gobank-db"
)

// MigrationsTable is where this component records its schema versions.
const MigrationsTable = "scenario_schema_migrations"

// Migrations is the scenario schema's history. Append to change it. The
// configuration, steps and probes are kept as JSON: they are read whole,
// never queried by field.
var Migrations = []db.Migration{
	{Version: 1, Name: "scenario runs", Phase: db.Expand, SQL: `
CREATE TABLE IF NOT EXISTS scenario_runs (
	run_id       VARCHAR(36) PRIMARY KEY,
	environment  VARCHAR(40) NOT NULL,
	scale        VARCHAR(40) NOT NULL,
	server_type  VARCHAR(40) NOT NULL,
	memory_gb    DOUBLE PRECISION NOT NULL,
	version      VARCHAR(50) NOT NULL,
	created_at   TIMESTAMPTZ NOT NULL,
	config       TEXT NOT NULL,
	steps        TEXT NOT NULL DEFAULT '[]',
	probes       TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_scenario_runs_created ON scenario_runs(created_at);
`},
}

// Store keeps scenario runs in a database shared with the workflow store,
// the drills and the performance runs.
type Store struct{ db *sql.DB }

// New brings the scenario tables to the current version and returns the store.
func New(ctx context.Context, d *sql.DB) (*Store, error) {
	if _, err := db.ApplyTo(ctx, d, MigrationsTable, Migrations); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	return &Store{db: d}, nil
}

// Create records a run as it starts: where it runs and what was asked.
func (s *Store) Create(ctx context.Context, r Run) error {
	cfg, steps, probes, err := encode(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO scenario_runs (run_id, environment, scale, server_type, memory_gb, version, created_at, config, steps, probes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.RunID, r.Environment, r.Scale, r.ServerType, r.MemoryGB, r.Version, r.CreatedAt.UTC(), cfg, steps, probes)
	return err
}

// Update writes the run's steps and probes as they complete.
func (s *Store) Update(ctx context.Context, r Run) error {
	cfg, steps, probes, err := encode(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE scenario_runs SET config = $1, steps = $2, probes = $3 WHERE run_id = $4`, cfg, steps, probes, r.RunID)
	return err
}

// Get is the run for a workflow run; nil when there is none.
func (s *Store) Get(ctx context.Context, runID string) (*Run, error) {
	list, err := s.query(ctx, selectRuns+` WHERE run_id = $1`, runID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// List is the most recent runs, newest first.
func (s *Store) List(ctx context.Context, limit int) ([]Run, error) {
	return s.query(ctx, selectRuns+` ORDER BY created_at DESC LIMIT $1`, limit)
}

const selectRuns = `SELECT run_id, environment, scale, server_type, memory_gb, version, created_at, config, steps, probes FROM scenario_runs`

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var cfg, steps, probes string
		if err := rows.Scan(&r.RunID, &r.Environment, &r.Scale, &r.ServerType, &r.MemoryGB, &r.Version, &r.CreatedAt, &cfg, &steps, &probes); err != nil {
			return nil, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if err := json.Unmarshal([]byte(cfg), &r.Config); err != nil {
			return nil, fmt.Errorf("scenario %s config: %w", r.RunID, err)
		}
		if err := json.Unmarshal([]byte(steps), &r.Steps); err != nil {
			return nil, fmt.Errorf("scenario %s steps: %w", r.RunID, err)
		}
		if err := json.Unmarshal([]byte(probes), &r.Probes); err != nil {
			return nil, fmt.Errorf("scenario %s probes: %w", r.RunID, err)
		}
		for i := range r.Steps {
			r.Steps[i].Started = r.Steps[i].Started.UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func encode(r Run) (cfg, steps, probes string, err error) {
	c, err := json.Marshal(r.Config)
	if err != nil {
		return "", "", "", err
	}
	st, err := json.Marshal(orEmpty(r.Steps))
	if err != nil {
		return "", "", "", err
	}
	pr, err := json.Marshal(orEmpty(r.Probes))
	if err != nil {
		return "", "", "", err
	}
	return string(c), string(st), string(pr), nil
}

// orEmpty keeps a nil slice as [] in the JSON, so a run with no steps yet
// reads back the same way it was written.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
