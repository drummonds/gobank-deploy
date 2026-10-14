package scenario

import (
	"strings"
	"testing"
	"time"
)

// sample is one seven-years run as the workflow records it.
var sample = Run{
	RunID: "run-1", Environment: "seven-years-large", Scale: "large",
	ServerType: "ccx33", MemoryGB: 32, Version: "v0.49.0",
	CreatedAt: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC),
	Config:    Config{Name: "seven-years", Customers: 1000, Years: 7, DayLength: 24 * time.Hour, Search: "Smith"},
	Steps: []Step{
		{Name: "add", Started: time.Date(2026, 10, 15, 9, 1, 0, 0, time.UTC), Duration: 2 * time.Minute,
			Figures: []Figure{{"customers", "1,000"}, {"customers/s", "8.3"}}},
		{Name: "years", Started: time.Date(2026, 10, 15, 9, 3, 0, 0, time.UTC), Duration: 3*time.Hour + 12*time.Minute,
			Figures: []Figure{{"days", "2,556"}, {"account days / 12h", "13,367,814"}}},
	},
	Probes: []Probe{
		{Route: "customer search by name", Path: "/customers?q=Smith", Concurrency: 1, N: 20, P50: 120 * time.Millisecond, P95: 310 * time.Millisecond},
		{Route: "customer search by name", Path: "/customers?q=Smith", Concurrency: 4, N: 20, P50: 150 * time.Millisecond, P95: 420 * time.Millisecond},
	},
}

// A run's page is named by its date, scenario and scale.
func TestFilenameIsDateScenarioScale(t *testing.T) {
	if got := Filename(sample); got != "2026-10-15-seven-years-large.md" {
		t.Errorf("Filename = %q", got)
	}
}

// The page states where the run was, its configuration, each step with
// its duration and figures, each probe's percentiles, links the workflow
// run, and ends with an empty Notes section for the operator's hand.
func TestPageHasTheRunItsTablesAndRoomForNotes(t *testing.T) {
	p := Page(sample)
	for _, want := range []string{
		"# 2026-10-15 seven-years at large",
		"ccx33", "32 GB", "v0.49.0",
		"| Customers | 1,000 |",
		"| Years | 7 |",
		"| Day length | 24h |",
		"| Search | Smith |",
		"| add | 09:01:00 | 2m | customers 1,000; customers/s 8.3 |",
		"| years | 09:03:00 | 3h12m | days 2,556; account days / 12h 13,367,814 |",
		"| customer search by name | `/customers?q=Smith` | 1 | 20 | 120ms | 310ms |",
		"| customer search by name | `/customers?q=Smith` | 4 | 20 | 150ms | 420ms |",
		"/workflows/run-1",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("page lacks %q:\n%s", want, p)
		}
	}
	if !strings.HasSuffix(strings.TrimRight(p, "\n"), "## Notes") {
		t.Errorf("the page should end with an empty Notes section:\n%s", p)
	}
}

// The index lists runs newest first: a run's line goes above the entries
// already there, replaces its own earlier line on a second write, and
// goes at the end of an index with no entries yet.
func TestUpdateIndexInsertsNewestFirstOnce(t *testing.T) {
	older := "- [2026-10-01 seven-years at small](2026-10-01-seven-years-small.md) — gobank v0.48.0, 1,000 customers, 7 years"
	index := "# Journal\n\nRuns newest first; notes between them.\n\n" + older + "\n"
	line := "- [2026-10-15 seven-years at large](2026-10-15-seven-years-large.md) — gobank v0.49.0, 1,000 customers, 7 years"

	out, err := UpdateIndex(index, sample)
	if err != nil {
		t.Fatal(err)
	}
	if i, j := strings.Index(out, line), strings.Index(out, older); i < 0 || j < 0 || j < i {
		t.Errorf("the new line should sit above the older entry:\n%s", out)
	}
	if !strings.HasPrefix(out, "# Journal\n\nRuns newest first; notes between them.\n\n") {
		t.Errorf("the prose above the entries should be kept:\n%s", out)
	}

	again, err := UpdateIndex(out, sample)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(again, "2026-10-15-seven-years-large.md") != 1 || again != out {
		t.Errorf("a second write should replace the run's line, not add one:\n%s", again)
	}

	fresh, err := UpdateIndex("# Journal\n\nRuns newest first.\n", sample)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(fresh, "Runs newest first.\n\n"+line+"\n") {
		t.Errorf("an index without entries gets the line at the end:\n%s", fresh)
	}
}
