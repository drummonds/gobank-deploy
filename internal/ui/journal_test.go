package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
)

func (f *fakeOperator) Scenarios(context.Context, int) ([]scenario.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scenarios, nil
}

// The journal sits under About: the About page describes the scenario
// workflow and links the journal, which shows every scenario run with its
// workflow state and the page and index line to copy into the
// repository's journal folder.
func TestJournalUnderAboutShowsEachRunAsItsPage(t *testing.T) {
	ts, op := newTestServer(t)
	_, about := get(t, ts, "/about")
	if !strings.Contains(about, `href="/about/journal"`) || !strings.Contains(about, "scenario-workflow.svg") {
		t.Errorf("About should describe the scenario workflow and link the journal:\n%s", about)
	}
	code, body := get(t, ts, "/about/journal")
	if code != http.StatusOK || !strings.Contains(body, "No scenario runs yet") {
		t.Errorf("empty journal: %d\n%s", code, body)
	}

	op.mu.Lock()
	op.scenarios = []scenario.Run{{RunID: "run-1", Environment: "seven-years-large", Scale: "large", ServerType: "ccx33", MemoryGB: 32, Version: "v0.49.0",
		CreatedAt: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC),
		Config:    scenario.Config{Name: "seven-years", Customers: 1000, Years: 7, DayLength: 24 * time.Hour, Search: "Smith"},
		Steps:     []scenario.Step{{Name: "add", Started: time.Date(2026, 10, 15, 9, 1, 0, 0, time.UTC), Duration: 2 * time.Minute, Figures: []scenario.Figure{{Label: "customers", Value: "1,000"}}}},
		Probes:    []scenario.Probe{{Route: "dashboard", Path: "/", Concurrency: 1, N: 20, P50: 80 * time.Millisecond, P95: 90 * time.Millisecond}}}}
	op.mu.Unlock()
	_, body = get(t, ts, "/about/journal")
	for _, want := range []string{
		"2026-10-15 seven-years at large",
		"journal/2026-10-15-seven-years-large.md",
		"| add | 09:01:00 | 2m | customers 1,000 |",
		"| dashboard | `/` | 1 | 20 | 80ms | 90ms |",
		"## Notes",
		`href="/workflows/run-1"`,
		"- [2026-10-15 seven-years at large](2026-10-15-seven-years-large.md)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("journal lacks %q:\n%s", want, body)
		}
	}
}
