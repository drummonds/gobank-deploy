package perf

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "git.bytestone.uk/hum3/go-postgres"
)

func open(t *testing.T) *Store {
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

// A performance run is recorded as it starts, with where it ran, and
// filled in stage by stage: the add span's figures, then the days span's.
// Reading it back gives the whole row; the list is newest first.
func TestRunIsRecordedThenFilledInByStage(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	first := Run{RunID: "r1", Environment: "perf", Scale: "large", ServerType: "cx53", MemoryGB: 32, Version: "v0.12.0", CreatedAt: t0}
	if err := s.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, Run{RunID: "r2", Environment: "perf", Scale: "small", ServerType: "cx23", MemoryGB: 4, Version: "v0.12.0", CreatedAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "r1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if *got != first {
		t.Errorf("fresh run = %+v, want %+v", *got, first)
	}

	first.AddSpan, first.Customers, first.CustomersPerSec = 10*time.Minute, 48000, 80.5
	if err := s.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.DaysSpan, first.Days, first.AccountDaysPer12h, first.LastDay, first.LastDayAccounts = 10*time.Minute, 7, 9_000_000, 85*time.Second, 96000
	if err := s.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, "r1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if *got != first {
		t.Errorf("filled run = %+v, want %+v", *got, first)
	}

	list, err := s.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].RunID != "r2" || list[1].RunID != "r1" {
		t.Errorf("list = %+v, want r2 then r1", list)
	}
	if unknown, err := s.Get(ctx, "nope"); err != nil || unknown != nil {
		t.Errorf("unknown run = %v, %v; want nil", unknown, err)
	}
}
