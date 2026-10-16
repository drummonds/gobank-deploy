package scenario

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sample is one seven-years run as the workflow records it.
var sample = Run{
	RunID: "run-1", Environment: "seven-years-large", Scale: "large",
	ServerType: "ccx33", MemoryGB: 32, Version: "v0.49.0",
	CreatedAt: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC),
	Config:    Config{Name: "seven-years", Customers: 1000, Years: 7, DayLength: 24 * time.Hour, Search: "Smith", Ladder: []int{1, 4, 16}, Samples: 20},
	Steps: []Step{
		{Name: "add", Started: time.Date(2026, 10, 15, 9, 1, 0, 0, time.UTC), Duration: 2 * time.Minute,
			Figures: []Figure{{"added", "1,000"}, {"on the books", "1,000"}, {"customers/s", "8.3"}}},
		{Name: "years", Started: time.Date(2026, 10, 15, 9, 3, 0, 0, time.UTC), Duration: 3*time.Hour + 12*time.Minute,
			Figures: []Figure{{"bank days", "2,556"}, {"from", "2026-01-01"}, {"to", "2033-01-01"}, {"bank days/h", "799"}, {"account days/h", "1,597,500"}}},
	},
	Probes: []Probe{
		{Route: "customer search by name", Method: "POST", Path: "/customers/search", Concurrency: 1, N: 20, P50: 120 * time.Millisecond, P95: 310 * time.Millisecond, PerSecond: 8.3},
		{Route: "customer search by name", Method: "POST", Path: "/customers/search", Concurrency: 4, N: 20, P50: 150 * time.Millisecond, P95: 420 * time.Millisecond, PerSecond: 26.7},
		{Route: "GET /movements/{id}", Method: "GET", Path: "/movements/{id}", Skipped: "needs a path parameter"},
	},
}

// A run's page is named by its date, scenario and scale; a scan, which
// runs on an environment as it is, by the environment.
func TestFilenameIsDateScenarioScale(t *testing.T) {
	if got := Filename(sample); got != "2026-10-15-seven-years-large.md" {
		t.Errorf("Filename = %q", got)
	}
	scan := Run{Environment: "preprod", CreatedAt: sample.CreatedAt, Config: Config{Name: "scan"}}
	if got := Filename(scan); got != "2026-10-15-scan-preprod.md" {
		t.Errorf("scan Filename = %q", got)
	}
	if got := Title(scan); got != "2026-10-15 scan at preprod" {
		t.Errorf("scan Title = %q", got)
	}
}

// The page states where the run was, its configuration, each step with
// its wall clock and figures, each probe's percentiles and throughput at
// each rung of the ladder, the routes not scanned and why, links the
// workflow run, and ends with an empty Notes section for the operator's
// hand.
func TestPageHasTheRunItsTablesAndRoomForNotes(t *testing.T) {
	p := Page(sample)
	for _, want := range []string{
		"# 2026-10-15 seven-years at large",
		"ccx33", "32 GB", "v0.49.0",
		"| Customers | 1,000 |",
		"| Years | 7 |",
		"| Day length | 24h |",
		"| Search | Smith |",
		"| Ladder | 1, 4, 16 |",
		"| Samples | 20 |",
		"| Step | Started | Wall clock | Figures |",
		"| add | 09:01:00 | 2m | added 1,000; on the books 1,000; customers/s 8.3 |",
		"| years | 09:03:00 | 3h12m | bank days 2,556; from 2026-01-01; to 2033-01-01; bank days/h 799; account days/h 1,597,500 |",
		"| Route | Method | Path | Concurrency | n | p50 | p95 | per s |",
		"| customer search by name | POST | `/customers/search` | 1 | 20 | 120ms | 310ms | 8.3 |",
		"| customer search by name | POST | `/customers/search` | 4 | 20 | 150ms | 420ms | 26.7 |",
		"Not scanned: `GET /movements/{id}` (needs a path parameter)",
		"/workflows/run-1",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("page lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "| GET /movements/{id} |") {
		t.Errorf("a route not scanned is not a row of the table:\n%s", p)
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

// The measured document is the scan for gobank's API pages: where and
// when, a row per route and rung with the latency in milliseconds and
// the throughput, and the routes not scanned with why.
func TestMeasuredDocumentHasARowPerRouteAndRung(t *testing.T) {
	b, err := MeasuredJSON(sample)
	if err != nil {
		t.Fatal(err)
	}
	var m Measured
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Date != "2026-10-15" || m.Version != "v0.49.0" || m.Environment != "seven-years-large" || m.Server != "ccx33" || m.MemoryGB != 32 || m.Customers != 1000 || m.Samples != 20 {
		t.Errorf("header = %+v", m)
	}
	if len(m.Ladder) != 3 || m.Ladder[2] != 16 {
		t.Errorf("ladder = %v", m.Ladder)
	}
	if len(m.Routes) != 1 || m.Routes[0].Method != "POST" || m.Routes[0].Path != "/customers/search" || len(m.Routes[0].Rungs) != 2 {
		t.Fatalf("routes = %+v", m.Routes)
	}
	if r := m.Routes[0].Rungs[1]; r.Concurrency != 4 || r.N != 20 || r.P50ms != 150 || r.P95ms != 420 || r.PerSecond != 26.7 {
		t.Errorf("rung = %+v", r)
	}
	if len(m.NotScanned) != 1 || m.NotScanned[0].Path != "/movements/{id}" || m.NotScanned[0].Why != "needs a path parameter" {
		t.Errorf("not scanned = %+v", m.NotScanned)
	}
	if !strings.Contains(string(b), `"p95_ms": 420`) {
		t.Errorf("the document is indented JSON with snake_case keys:\n%s", b)
	}
}
