package drills

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "git.bytestone.uk/hum3/go-postgres"
	"git.bytestone.uk/hum3/gobank-workflow/sqlstore"
)

func open(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	d, err := sql.Open("pglike", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := New(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return d, s
}

var at = time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)

func intact(b bool) *bool { return &b }

func TestDrillIsKeptWithItsObservationsInMomentOrder(t *testing.T) {
	_, s := open(t)
	ctx := context.Background()
	d := Drill{RunID: "run-1", Environment: "prod", From: "v0.7.0", To: "v0.8.0", CreatedAt: at}
	if err := s.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	upgraded := Observation{Moment: Upgraded, ObservedAt: at.Add(2 * time.Minute), Version: "v0.8.0",
		Position: &Position{Day: "2026-03-01", DayCount: 59, Customers: 1200, Savings: "£1,000.00", Lending: "£500.00"},
		Restart:  &Restart{PreviousVersion: "v0.7.0", Downtime: 42 * time.Second, DowntimeKnown: true, Intact: intact(true)}}
	before := Observation{Moment: Before, ObservedAt: at.Add(time.Minute), Version: "v0.7.0",
		Position: &Position{Day: "2026-03-01", DayCount: 59, Customers: 1200, Savings: "£1,000.00", Lending: "£500.00"},
		Restart:  &Restart{PreviousVersion: "", DowntimeKnown: false}}
	for _, o := range []Observation{upgraded, before} { // written out of order
		if err := s.Observe(ctx, d.RunID, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, d.RunID)
	if err != nil || got == nil {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if got.Environment != "prod" || got.From != "v0.7.0" || got.To != "v0.8.0" || !got.CreatedAt.Equal(at) {
		t.Errorf("drill = %+v", got)
	}
	if len(got.Observations) != 2 || got.Observations[0].Moment != Before || got.Observations[1].Moment != Upgraded {
		t.Fatalf("observations = %+v", got.Observations)
	}
	u := got.Observations[1]
	if u.Version != "v0.8.0" || *u.Position != *upgraded.Position || u.Restart.PreviousVersion != "v0.7.0" || u.Restart.Downtime != 42*time.Second || !u.Restart.DowntimeKnown || u.Restart.Intact == nil || !*u.Restart.Intact {
		t.Errorf("upgraded = %+v position %+v restart %+v", u, u.Position, u.Restart)
	}
	b := got.Observations[0]
	if b.Restart == nil || b.Restart.DowntimeKnown || b.Restart.Intact != nil {
		t.Errorf("before's restart row (first start, nothing before it) = %+v", b.Restart)
	}
}

// A hop that failed is re-run by the next attempt; its observation
// replaces the one before rather than adding a second.
func TestAMomentObservedAgainReplacesTheObservation(t *testing.T) {
	_, s := open(t)
	ctx := context.Background()
	s.Create(ctx, Drill{RunID: "run-1", Environment: "prod", From: "v1", To: "v2", CreatedAt: at})
	s.Observe(ctx, "run-1", Observation{Moment: Upgraded, ObservedAt: at, Version: "v1"})
	s.Observe(ctx, "run-1", Observation{Moment: Upgraded, ObservedAt: at.Add(time.Hour), Version: "v2"})
	got, _ := s.Get(ctx, "run-1")
	if len(got.Observations) != 1 || got.Observations[0].Version != "v2" {
		t.Errorf("observations = %+v", got.Observations)
	}
}

// A release without about.json gives only its version: the position and
// restart row are absent, not zero.
func TestAnObservationWithoutPositionOrRestartRoundTrips(t *testing.T) {
	_, s := open(t)
	ctx := context.Background()
	s.Create(ctx, Drill{RunID: "run-1", Environment: "prod", From: "v1", To: "v2", CreatedAt: at})
	s.Observe(ctx, "run-1", Observation{Moment: Before, ObservedAt: at, Version: "v1"})
	got, _ := s.Get(ctx, "run-1")
	if o := got.Observations[0]; o.Position != nil || o.Restart != nil || o.Version != "v1" {
		t.Errorf("observation = %+v", o)
	}
}

func TestListIsNewestFirstAndGetOfAnUnknownDrillIsNil(t *testing.T) {
	_, s := open(t)
	ctx := context.Background()
	s.Create(ctx, Drill{RunID: "old", Environment: "prod", From: "v1", To: "v2", CreatedAt: at})
	s.Create(ctx, Drill{RunID: "new", Environment: "preprod", From: "v2", To: "v3", CreatedAt: at.Add(time.Hour)})
	s.Observe(ctx, "new", Observation{Moment: Before, ObservedAt: at.Add(time.Hour), Version: "v2"})
	list, err := s.List(ctx, 10)
	if err != nil || len(list) != 2 || list[0].RunID != "new" || list[1].RunID != "old" || len(list[0].Observations) != 1 {
		t.Errorf("list = %+v, %v", list, err)
	}
	if got, err := s.Get(ctx, "nope"); got != nil || err != nil {
		t.Errorf("Get unknown = %v, %v", got, err)
	}
}

// The drill tables and gobank-workflow's share one file; each component's
// migrations are recorded in its own table, so neither skips the other's
// versions, and opening either twice applies nothing more.
func TestSharesADatabaseWithTheWorkflowStore(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	if _, err := sqlstore.New(d); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, d); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"drills", "drill_observations", "workflow_runs", "workflow_steps", "schema_migrations", MigrationsTable} {
		if _, err := d.Exec("SELECT 1 FROM " + table + " WHERE 1=0"); err != nil {
			t.Errorf("%s: %v", table, err)
		}
	}
}
