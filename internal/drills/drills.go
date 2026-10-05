// Package drills records gobank's upgrade drill (ADR-0003) as gobank-deploy
// runs it: each drill with what the demo looked like before and after every
// hop. The tables live in gobank-deploy's database beside gobank-workflow's
// run records, each component's schema versioned on its own.
package drills

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	db "git.bytestone.uk/hum3/gobank-db"
	"github.com/google/uuid"
)

// Drill is one rehearsal on an environment: upgrade from release From to
// To, roll back to From, forward to To again. It is a workflow instance,
// RunID that instance's id; its state and stages are the run's.
type Drill struct {
	RunID        string
	Environment  string
	From, To     string // releases N and N+1
	CreatedAt    time.Time
	Observations []Observation // in Moments order
}

// Moment is when in a drill the demo was observed: before anything, and
// after each hop.
type Moment string

const (
	Before     Moment = "before"
	Upgraded   Moment = "upgraded"
	RolledBack Moment = "rolled_back"
	Forward    Moment = "forward"
)

// Moments in drill order.
var Moments = []Moment{Before, Upgraded, RolledBack, Forward}

func (m Moment) order() int {
	for i, x := range Moments {
		if x == m {
			return i
		}
	}
	return len(Moments)
}

// Observation is the demo as read at a moment: the version answering,
// where the run is, and the newest row of its restart record. Position and
// Restart are nil when the release offers no about.json (gobank before
// it had one), so the version is all that can be checked.
type Observation struct {
	Moment     Moment
	ObservedAt time.Time
	Version    string
	Position   *Position
	Restart    *Restart
}

// Position is where the run is, as the demo's dashboard leads with it.
type Position struct {
	Day       string // YYYY-MM-DD
	DayCount  int
	Customers int
	Savings   string // formatted as the demo shows them
	Lending   string
}

// Restart is the newest row of the demo's restart record: the process it
// followed, how long the bank was unserved in between, and whether the
// handover was intact (the same day and customers on both sides).
type Restart struct {
	PreviousVersion string // "" when the previous process kept no record
	DayCount        int    // the run's day count at this start, which an intact handover makes the stop's too
	Downtime        time.Duration
	DowntimeKnown   bool  // false after an unclean stop
	Intact          *bool // nil when the record cannot say
}

// MigrationsTable is where this component records its schema versions;
// gobank-workflow's live in schema_migrations.
const MigrationsTable = "deploy_schema_migrations"

// Migrations is the drill schema's history. Append to change it.
var Migrations = []db.Migration{
	{Version: 1, Name: "drills and their observations", Phase: db.Expand, SQL: `
CREATE TABLE IF NOT EXISTS drills (
	run_id       VARCHAR(36) PRIMARY KEY,
	environment  VARCHAR(40) NOT NULL,
	from_version VARCHAR(50) NOT NULL,
	to_version   VARCHAR(50) NOT NULL,
	created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_drills_created ON drills(created_at);
CREATE TABLE IF NOT EXISTS drill_observations (
	id               VARCHAR(36) PRIMARY KEY,
	run_id           VARCHAR(36) NOT NULL REFERENCES drills(run_id),
	moment           VARCHAR(20) NOT NULL,
	observed_at      TIMESTAMPTZ NOT NULL,
	version          VARCHAR(50) NOT NULL,
	day              VARCHAR(10) NULL,
	day_count        INTEGER NULL,
	customers        INTEGER NULL,
	savings          VARCHAR(40) NULL,
	lending          VARCHAR(40) NULL,
	previous_version VARCHAR(50) NULL,
	downtime_ns      BIGINT NULL,
	intact           BOOLEAN NULL,
	created_at       TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_drill_observations_run_moment ON drill_observations(run_id, moment);
CREATE INDEX IF NOT EXISTS idx_drill_observations_created ON drill_observations(created_at);
`},
}

// Store keeps drills in a database shared with the workflow store.
type Store struct{ db *sql.DB }

// New brings the drill tables to the current version and returns the store.
func New(ctx context.Context, d *sql.DB) (*Store, error) {
	if _, err := db.ApplyTo(ctx, d, MigrationsTable, Migrations); err != nil {
		return nil, fmt.Errorf("drills: %w", err)
	}
	return &Store{db: d}, nil
}

// Create records a drill as it starts.
func (s *Store) Create(ctx context.Context, d Drill) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO drills (run_id, environment, from_version, to_version, created_at) VALUES ($1, $2, $3, $4, $5)`,
		d.RunID, d.Environment, d.From, d.To, d.CreatedAt.UTC())
	return err
}

// Observe records what the demo looked like at a moment of the drill,
// replacing an earlier observation of the same moment (a hop re-run).
func (s *Store) Observe(ctx context.Context, runID string, o Observation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM drill_observations WHERE run_id = $1 AND moment = $2`, runID, string(o.Moment)); err != nil {
		return err
	}
	var day, savings, lending, previous sql.NullString
	var dayCount, customers sql.NullInt64
	var downtime sql.NullInt64
	var intact sql.NullBool
	if p := o.Position; p != nil {
		day = sql.NullString{String: p.Day, Valid: true}
		dayCount = sql.NullInt64{Int64: int64(p.DayCount), Valid: true}
		customers = sql.NullInt64{Int64: int64(p.Customers), Valid: true}
		savings = sql.NullString{String: p.Savings, Valid: true}
		lending = sql.NullString{String: p.Lending, Valid: true}
	}
	if r := o.Restart; r != nil {
		previous = sql.NullString{String: r.PreviousVersion, Valid: true}
		if r.DowntimeKnown {
			downtime = sql.NullInt64{Int64: int64(r.Downtime), Valid: true}
		}
		if r.Intact != nil {
			intact = sql.NullBool{Bool: *r.Intact, Valid: true}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO drill_observations
		(id, run_id, moment, observed_at, version, day, day_count, customers, savings, lending, previous_version, downtime_ns, intact, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		uuid.New().String(), runID, string(o.Moment), o.ObservedAt.UTC(), o.Version, day, dayCount, customers, savings, lending, previous, downtime, intact, time.Now().UTC())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Get is the drill for a workflow run with its observations; nil when there
// is none.
func (s *Store) Get(ctx context.Context, runID string) (*Drill, error) {
	list, err := s.query(ctx, `SELECT run_id, environment, from_version, to_version, created_at FROM drills WHERE run_id = $1`, runID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// List is the most recent drills, newest first, with their observations.
func (s *Store) List(ctx context.Context, limit int) ([]Drill, error) {
	return s.query(ctx, `SELECT run_id, environment, from_version, to_version, created_at FROM drills ORDER BY created_at DESC LIMIT $1`, limit)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Drill, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Drill
	for rows.Next() {
		var d Drill
		if err := rows.Scan(&d.RunID, &d.Environment, &d.From, &d.To, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Observations, err = s.observations(ctx, out[i].RunID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) observations(ctx context.Context, runID string) ([]Observation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT moment, observed_at, version, day, day_count, customers, savings, lending, previous_version, downtime_ns, intact
		FROM drill_observations WHERE run_id = $1`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var o Observation
		var moment string
		var day, savings, lending, previous sql.NullString
		var dayCount, customers, downtime sql.NullInt64
		var intact sql.NullBool
		if err := rows.Scan(&moment, &o.ObservedAt, &o.Version, &day, &dayCount, &customers, &savings, &lending, &previous, &downtime, &intact); err != nil {
			return nil, err
		}
		o.Moment = Moment(moment)
		if day.Valid {
			o.Position = &Position{Day: day.String, DayCount: int(dayCount.Int64), Customers: int(customers.Int64), Savings: savings.String, Lending: lending.String}
		}
		if previous.Valid {
			o.Restart = &Restart{PreviousVersion: previous.String, Downtime: time.Duration(downtime.Int64), DowntimeKnown: downtime.Valid}
			if intact.Valid {
				b := intact.Bool
				o.Restart.Intact = &b
			}
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortByMoment(out)
	return out, nil
}

func sortByMoment(obs []Observation) {
	for i := 1; i < len(obs); i++ {
		for j := i; j > 0 && obs[j].Moment.order() < obs[j-1].Moment.order(); j-- {
			obs[j], obs[j-1] = obs[j-1], obs[j]
		}
	}
}

// Reading is the demo as its about.json gives it at one moment: what the
// drill reads before and after each hop. About is false when the release
// offers no about.json (gobank before it had one): Version then came from the
// page and nothing else is known.
type Reading struct {
	Version   string
	About     bool
	Running   bool
	DayLength time.Duration // zero is flat out
	DayEndsIn time.Duration // zero when flat out or stopped
	Position  *Position
	Restart   *Restart

	// The rates a performance run reads (gobank v0.12 onwards; zero
	// before): customers added per second, live while a batch add runs
	// and else the last batch's, and account days per 12h at the pass's
	// whole-day rate, with the last day it is made of.
	Adding            bool
	CustomersPerSec   float64
	AccountDaysPer12h int64
	LastDayDuration   time.Duration
	LastDayAccounts   int
}

// Observation is the reading recorded against a moment of the drill.
func (r Reading) Observation(m Moment, at time.Time) Observation {
	return Observation{Moment: m, ObservedAt: at, Version: r.Version, Position: r.Position, Restart: r.Restart}
}
