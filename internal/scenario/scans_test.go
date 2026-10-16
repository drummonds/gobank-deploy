package scenario

import (
	"testing"
	"time"
)

func msec(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// scanOn is a scan with the dashboard and the search driven up the
// ladder, and one route not scanned.
func scanOn(day int, version string, dashP95, searchP95 int, skipped string) Run {
	r := Run{RunID: "scan-" + version, Environment: "prod", Version: version, CreatedAt: time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC),
		Config: Config{Name: "scan", Ladder: []int{1, 4}, Samples: 10, Customers: 3000}}
	for _, c := range []int{1, 4} {
		r.Probes = append(r.Probes,
			Probe{Route: "GET /", Method: "GET", Path: "/", Concurrency: c, N: 10, P50: msec(dashP95 / 2), P95: msec(dashP95 * c / 4), PerSecond: float64(100 / c)},
			Probe{Route: "POST /customers/search", Method: "POST", Path: "/customers/search", Concurrency: c, N: 10, P50: msec(searchP95 / 2), P95: msec(searchP95 * c / 4), PerSecond: float64(50 / c)})
	}
	if skipped != "" {
		r.Probes = append(r.Probes, Probe{Route: "GET /about", Method: "GET", Path: "/about", Skipped: skipped})
	}
	return r
}

func TestRoutesAreEachRouteAtItsTopRungWorstFirst(t *testing.T) {
	routes := Routes(scanOn(1, "v1", 100, 400, "needs a path parameter"))
	if len(routes) != 3 {
		t.Fatalf("routes: %+v", routes)
	}
	if r := routes[0]; r.Path != "/customers/search" || r.Concurrency != 4 || r.P95 != msec(400) || r.PerSecond != 12 {
		t.Errorf("worst first: %+v", r)
	}
	if r := routes[1]; r.Path != "/" || r.P95 != msec(100) {
		t.Errorf("then the dashboard: %+v", r)
	}
	if r := routes[2]; r.Path != "/about" || r.Skipped != "needs a path parameter" {
		t.Errorf("not scanned last: %+v", r)
	}
	if got := Worst(scanOn(1, "v1", 100, 400, ""), 1); len(got) != 1 || got[0].Path != "/customers/search" {
		t.Errorf("worst 1: %+v", got)
	}
}

func TestCompareSaysHowEachRouteMovedSinceThePreviousScan(t *testing.T) {
	prev := scanOn(1, "v1", 100, 400, "needs a path parameter")
	now := scanOn(2, "v2", 100, 600, "")
	now.Probes = append(now.Probes, Probe{Route: "GET /new", Method: "GET", Path: "/new", Concurrency: 4, N: 10, P95: msec(50), PerSecond: 20})
	got := Compare(now, &prev)
	want := map[string]Change{"/customers/search": Worse, "/": Same, "/new": First}
	for _, c := range got {
		if c.Change != want[c.Path] {
			t.Errorf("%s: %s, want %s (%+v)", c.Path, c.Change, want[c.Path], c)
		}
		if c.Path == "/customers/search" && (c.Previous == nil || c.Previous.P95 != msec(400)) {
			t.Errorf("previous figure kept: %+v", c.Previous)
		}
	}
	if len(got) != 3 {
		t.Errorf("one row per route of the latest scan: %+v", got)
	}
	for _, c := range Compare(now, nil) {
		if c.Change != First {
			t.Errorf("no previous scan: everything is new, got %+v", c)
		}
	}
}

func TestCriticalIsFailedNewlyUnscannedOrOverBudget(t *testing.T) {
	prev := scanOn(1, "v1", 100, 400, "")
	prev.Probes = append(prev.Probes, Probe{Route: "GET /about", Method: "GET", Path: "/about", Concurrency: 4, N: 10, P95: msec(30), PerSecond: 40})
	now := scanOn(2, "v2", 100, 600, "failed: 500 Internal Server Error")
	now.Probes = append(now.Probes, Probe{Route: "GET /gone", Method: "GET", Path: "/gone", Skipped: "needs a path parameter"})
	prev.Probes = append(prev.Probes, Probe{Route: "GET /gone", Method: "GET", Path: "/gone", Concurrency: 4, N: 10, P95: msec(30), PerSecond: 40})

	got := Critical(now, &prev, msec(500))
	why := map[string]string{}
	for _, c := range got {
		why[c.Path] = c.Why
	}
	if len(got) != 3 {
		t.Errorf("three concerns: %+v", got)
	}
	if why["/about"] != "failed: 500 Internal Server Error" {
		t.Errorf("failed: %q", why["/about"])
	}
	if why["/gone"] != "not scanned (needs a path parameter), scanned on 2026-10-01" {
		t.Errorf("newly unscanned: %q", why["/gone"])
	}
	if why["/customers/search"] != "p95 600ms at 4 in flight, over the 500ms budget" {
		t.Errorf("over budget: %q", why["/customers/search"])
	}
	if _, ok := why["/"]; ok {
		t.Errorf("the dashboard is fine")
	}
}

func TestTrendIsEachRouteAcrossTheScansOldestFirst(t *testing.T) {
	newest := []Run{scanOn(3, "v3", 120, 300, "x"), scanOn(2, "v2", 100, 600, "x"), scanOn(1, "v1", 100, 400, "x")}
	got := Trend(newest)
	byPath := map[string]RouteTrend{}
	for _, tr := range got {
		byPath[tr.Path] = tr
	}
	search := byPath["/customers/search"]
	if len(search.Points) != 3 || search.Points[0].Version != "v1" || search.Points[2].Version != "v3" {
		t.Fatalf("points oldest first: %+v", search.Points)
	}
	if p := search.Points[1]; p.P95 != msec(600) || p.PerSecond != 12 || p.Date != "2026-10-02" {
		t.Errorf("point: %+v", p)
	}
	if about := byPath["/about"]; len(about.Points) != 3 || about.Points[0].Skipped != "x" {
		t.Errorf("not scanned is a point too: %+v", about)
	}
	if got[0].Path != "/customers/search" {
		t.Errorf("worst on the latest scan first: %+v", got[0])
	}
}

func TestScansAreTheScanRunsOnly(t *testing.T) {
	runs := []Run{{Config: Config{Name: "seven-years"}}, scanOn(1, "v1", 1, 1, "")}
	if got := Scans(runs); len(got) != 1 || !IsScan(got[0]) {
		t.Errorf("scans: %+v", got)
	}
}
