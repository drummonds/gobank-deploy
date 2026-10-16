// Package perf records gobank's performance runs as gobank-deploy makes
// them: an environment created at a scale, customers added for a span,
// days run for another, the two rates read off the demo, the server
// removed. One row per run, filled in stage by stage, in gobank-deploy's
// database beside the workflow runs and the drills, its schema versioned
// on its own.
package perf

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	db "git.bytestone.uk/hum3/gobank-db"
)

// Run is one performance run: where it ran, and what it measured. It is a
// workflow instance, RunID that instance's id; its state and stages are
// the run's. The figures are zero until their stage has run.
type Run struct {
	RunID       string
	Environment string
	Scale       string  // small, large, ... as asked for
	ServerType  string  // the provider's type the scale became, e.g. cx53
	MemoryGB    float64 // the server's RAM, shared with PostgreSQL
	Version     string  // the gobank release measured
	CreatedAt   time.Time

	// The add span: customers added through the real pipeline, flat out,
	// and the rate the demo reported batch by batch.
	AddSpan         time.Duration
	Customers       int // on the books when the span ended
	CustomersPerSec float64

	// The days span: the run going flat out, and the pass's rate over the
	// whole day as the demo quotes it, with the last day it is made of.
	DaysSpan          time.Duration
	Days              int // simulated days completed in the span
	AccountDaysPer12h int64
	LastDay           time.Duration
	LastDayAccounts   int

	// What the demo measured of every route it served over the run, read
	// off about.json just before the server is removed (gobank v0.51.0
	// onwards; none before).
	Routes []Route

	// The data's size on the demo's PostgreSQL, read with the routes:
	// every table biggest first, and the whole database (gobank with
	// about.json's tables; none before).
	Tables        []Table
	DatabaseBytes int64
}

// Table is one table's share of the database: its size on disk with
// indexes and TOAST, and the rows PostgreSQL estimates it holds.
type Table struct {
	Name  string
	Rows  int64
	Bytes int64
}

// Route is one route's figures as the demo measured them since its
// process started: how many requests, and the p95 of the latest 1,024.
type Route struct {
	Pattern string // as gobank registered it: "GET /v1/health", "/customers/"
	Count   int64
	P95     time.Duration
}

// MigrationsTable is where this component records its schema versions.
const MigrationsTable = "perf_schema_migrations"

// Migrations is the perf schema's history. Append to change it.
var Migrations = []db.Migration{
	{Version: 1, Name: "performance runs", Phase: db.Expand, SQL: `
CREATE TABLE IF NOT EXISTS perf_runs (
	run_id               VARCHAR(36) PRIMARY KEY,
	environment          VARCHAR(40) NOT NULL,
	scale                VARCHAR(40) NOT NULL,
	server_type          VARCHAR(40) NOT NULL,
	memory_gb            DOUBLE PRECISION NOT NULL,
	version              VARCHAR(50) NOT NULL,
	created_at           TIMESTAMPTZ NOT NULL,
	add_span_ns          BIGINT NOT NULL DEFAULT 0,
	customers            INTEGER NOT NULL DEFAULT 0,
	customers_per_sec    DOUBLE PRECISION NOT NULL DEFAULT 0,
	days_span_ns         BIGINT NOT NULL DEFAULT 0,
	days                 INTEGER NOT NULL DEFAULT 0,
	account_days_per_12h BIGINT NOT NULL DEFAULT 0,
	last_day_ns          BIGINT NOT NULL DEFAULT 0,
	last_day_accounts    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_perf_runs_created ON perf_runs(created_at);
`},
	{Version: 2, Name: "the routes' figures", Phase: db.Expand, SQL: `
ALTER TABLE perf_runs ADD COLUMN IF NOT EXISTS routes TEXT NOT NULL DEFAULT '[]';
`},
	{Version: 3, Name: "the tables' sizes", Phase: db.Expand, SQL: `
ALTER TABLE perf_runs ADD COLUMN IF NOT EXISTS tables TEXT NOT NULL DEFAULT '[]';
ALTER TABLE perf_runs ADD COLUMN IF NOT EXISTS database_bytes BIGINT NOT NULL DEFAULT 0;
`},
}

// route is a Route as the routes column holds it, the p95 in nanoseconds.
type route struct {
	Pattern string `json:"pattern"`
	Count   int64  `json:"count"`
	P95     int64  `json:"p95_ns"`
}

// table is a Table as the tables column holds it.
type table struct {
	Name  string `json:"name"`
	Rows  int64  `json:"rows"`
	Bytes int64  `json:"bytes"`
}

// Store keeps performance runs in a database shared with the workflow
// store and the drills.
type Store struct{ db *sql.DB }

// New brings the perf tables to the current version and returns the store.
func New(ctx context.Context, d *sql.DB) (*Store, error) {
	if _, err := db.ApplyTo(ctx, d, MigrationsTable, Migrations); err != nil {
		return nil, fmt.Errorf("perf: %w", err)
	}
	return &Store{db: d}, nil
}

// Create records a run as it starts: where it runs, nothing measured yet.
func (s *Store) Create(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO perf_runs (run_id, environment, scale, server_type, memory_gb, version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.RunID, r.Environment, r.Scale, r.ServerType, r.MemoryGB, r.Version, r.CreatedAt.UTC())
	return err
}

// Update writes the run's figures as a stage completes them.
func (s *Store) Update(ctx context.Context, r Run) error {
	routes := make([]route, len(r.Routes))
	for i, rt := range r.Routes {
		routes[i] = route{rt.Pattern, rt.Count, int64(rt.P95)}
	}
	js, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	tables := make([]table, len(r.Tables))
	for i, t := range r.Tables {
		tables[i] = table(t)
	}
	tjs, err := json.Marshal(tables)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE perf_runs SET add_span_ns = $1, customers = $2, customers_per_sec = $3,
		days_span_ns = $4, days = $5, account_days_per_12h = $6, last_day_ns = $7, last_day_accounts = $8, routes = $9,
		tables = $10, database_bytes = $11
		WHERE run_id = $12`,
		int64(r.AddSpan), r.Customers, r.CustomersPerSec, int64(r.DaysSpan), r.Days, r.AccountDaysPer12h, int64(r.LastDay), r.LastDayAccounts, string(js),
		string(tjs), r.DatabaseBytes, r.RunID)
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

const selectRuns = `SELECT run_id, environment, scale, server_type, memory_gb, version, created_at,
	add_span_ns, customers, customers_per_sec, days_span_ns, days, account_days_per_12h, last_day_ns, last_day_accounts, routes, tables, database_bytes FROM perf_runs`

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var addSpan, daysSpan, lastDay int64
		var js, tjs string
		if err := rows.Scan(&r.RunID, &r.Environment, &r.Scale, &r.ServerType, &r.MemoryGB, &r.Version, &r.CreatedAt,
			&addSpan, &r.Customers, &r.CustomersPerSec, &daysSpan, &r.Days, &r.AccountDaysPer12h, &lastDay, &r.LastDayAccounts, &js,
			&tjs, &r.DatabaseBytes); err != nil {
			return nil, err
		}
		var routes []route
		if err := json.Unmarshal([]byte(js), &routes); err != nil {
			return nil, fmt.Errorf("perf run %s routes: %w", r.RunID, err)
		}
		for _, rt := range routes {
			r.Routes = append(r.Routes, Route{rt.Pattern, rt.Count, time.Duration(rt.P95)})
		}
		var tables []table
		if err := json.Unmarshal([]byte(tjs), &tables); err != nil {
			return nil, fmt.Errorf("perf run %s tables: %w", r.RunID, err)
		}
		for _, t := range tables {
			r.Tables = append(r.Tables, Table(t))
		}
		r.AddSpan, r.DaysSpan, r.LastDay = time.Duration(addSpan), time.Duration(daysSpan), time.Duration(lastDay)
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
