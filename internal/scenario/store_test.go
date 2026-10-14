package scenario

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "git.bytestone.uk/hum3/go-postgres"
)

func newStore(t *testing.T) *Store {
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
	return s
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A run is kept whole — where it ran, its configuration, every step with
// its figures and every probe — filled in step by step, and listed
// newest first.
func TestStoreKeepsARunWithItsStepsAndProbes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Create(ctx, sample); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "run-1")
	if err != nil || got == nil {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if asJSON(t, *got) != asJSON(t, sample) {
		t.Errorf("round trip changed the run:\n got %s\nwant %s", asJSON(t, *got), asJSON(t, sample))
	}

	later := sample
	later.Steps = append(append([]Step{}, sample.Steps...), Step{Name: "wallclock", Started: time.Date(2026, 10, 15, 12, 15, 0, 0, time.UTC), Duration: 40 * time.Second, Figures: []Figure{{"last day", "34s"}}})
	if err := s.Update(ctx, later); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "run-1")
	if len(got.Steps) != 3 || got.Steps[2].Name != "wallclock" || got.Steps[2].Figures[0].Value != "34s" {
		t.Errorf("after Update steps = %+v", got.Steps)
	}

	earlier := sample
	earlier.RunID, earlier.CreatedAt = "run-0", sample.CreatedAt.Add(-24*time.Hour)
	if err := s.Create(ctx, earlier); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, 10)
	if err != nil || len(list) != 2 || list[0].RunID != "run-1" || list[1].RunID != "run-0" {
		t.Errorf("List = %+v, %v; want run-1 then run-0", list, err)
	}

	if missing, err := s.Get(ctx, "nope"); err != nil || missing != nil {
		t.Errorf("Get of an unknown run = %v, %v; want nil, nil", missing, err)
	}
}
