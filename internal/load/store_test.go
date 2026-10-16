package load

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	_ "git.bytestone.uk/hum3/go-postgres"
)

// A load test is recorded as it starts, with where it ran, then filled in:
// the customers on the books, then the climb. Reading it back gives the
// whole record; the list is newest first.
func TestLoadRunIsRecordedThenFilledIn(t *testing.T) {
	d, err := sql.Open("pglike", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	s, err := NewStore(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	run := Run{RunID: "r1", Environment: "load1", Scale: "large", ServerType: "ccx33", MemoryGB: 32, Version: "v0.56.0", CreatedAt: t0}
	if err := s.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, Run{RunID: "r0", Environment: "load0", Scale: "small", CreatedAt: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "r1")
	if err != nil || got == nil || !reflect.DeepEqual(*got, run) {
		t.Fatalf("fresh = %+v %v; want %+v", got, err, run)
	}
	run.Customers = 10_000
	run.Result = Result{Budget: 2 * time.Second, Capacity: 50, Breach: "100 users: login p95 2.4s over 2s",
		Rungs: []Rung{{Users: 50, Journeys: 500, Screens: []Screen{{Name: "login", N: 500, P50: time.Second, P95: 1900 * time.Millisecond, Max: 3 * time.Second}},
			Journey: Screen{Name: "journey", N: 500, P95: 3 * time.Second}, Elapsed: time.Minute, PerSecond: 8.3}}}
	if err := s.Update(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, "r1")
	if err != nil || got == nil || !reflect.DeepEqual(*got, run) {
		t.Errorf("filled = %+v %v\nwant %+v", got, err, run)
	}
	list, err := s.List(ctx, 10)
	if err != nil || len(list) != 2 || list[0].RunID != "r1" || list[1].RunID != "r0" {
		t.Errorf("list = %+v %v", list, err)
	}
	if none, err := s.Get(ctx, "nope"); none != nil || err != nil {
		t.Errorf("unknown = %v %v", none, err)
	}
}
