package scenario

import (
	"fmt"
	"sort"
	"time"
)

// IsScan says whether the run is a scan of an environment as it stands
// rather than a scenario on a fresh one.
func IsScan(r Run) bool { return r.Config.Name == "scan" }

// Scans is the scan runs among runs, in the order given.
func Scans(runs []Run) []Run {
	var out []Run
	for _, r := range runs {
		if IsScan(r) {
			out = append(out, r)
		}
	}
	return out
}

// RouteResult is one route of a scan at the top rung it reached: the
// concurrency, the latency and the throughput there; or, not scanned,
// why.
type RouteResult struct {
	Route, Method, Path string
	Concurrency         int
	N                   int
	P50, P95            time.Duration
	PerSecond           float64
	Skipped             string // why the route was not driven; "" when it was
}

// Scanned says whether the route was driven.
func (r RouteResult) Scanned() bool { return r.Skipped == "" }

// key names the route for matching across scans.
func (r RouteResult) key() string { return r.Method + " " + r.Path }

// Routes is each route of the scan at its top rung, the scanned routes
// first from the worst p95 down, then the routes not scanned in the
// order the scan listed them.
func Routes(r Run) []RouteResult {
	var out []RouteResult
	at := map[string]int{}
	for _, p := range r.Probes {
		res := RouteResult{Route: p.Route, Method: p.Method, Path: p.Path, Concurrency: p.Concurrency, N: p.N, P50: p.P50, P95: p.P95, PerSecond: p.PerSecond, Skipped: p.Skipped}
		if i, ok := at[res.key()]; ok {
			if p.Concurrency >= out[i].Concurrency {
				out[i] = res
			}
			continue
		}
		at[res.key()] = len(out)
		out = append(out, res)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Scanned() != b.Scanned() {
			return a.Scanned()
		}
		return a.P95 > b.P95
	})
	return out
}

// Worst is the n scanned routes with the highest p95 at the top rung.
func Worst(r Run, n int) []RouteResult {
	var out []RouteResult
	for _, res := range Routes(r) {
		if !res.Scanned() || len(out) == n {
			break
		}
		out = append(out, res)
	}
	return out
}

// Change is how a route's p95 at the top rung moved since the previous
// scan: beyond the band either way, within it, or with no earlier figure
// to move from. A route not scanned has no change.
type Change string

const (
	First  Change = "first"
	Better Change = "better"
	Same   Change = "same"
	Worse  Change = "worse"
)

// band is how far the p95 moves before the change is called.
const band = 0.10

// Compared is a route of the latest scan beside the previous scan's
// figure for it.
type Compared struct {
	RouteResult
	Previous *RouteResult // nil when the previous scan has no figure for the route
	Change   Change
}

// Compare is each route of the latest scan, worst first, with how it
// moved since previous; with no previous scan everything is new.
func Compare(latest Run, previous *Run) []Compared {
	before := map[string]RouteResult{}
	if previous != nil {
		for _, res := range Routes(*previous) {
			if res.Scanned() {
				before[res.key()] = res
			}
		}
	}
	var out []Compared
	for _, res := range Routes(latest) {
		c := Compared{RouteResult: res}
		if !res.Scanned() {
			out = append(out, c)
			continue
		}
		p, ok := before[res.key()]
		if !ok {
			c.Change = First
		} else {
			c.Previous = &p
			ratio := float64(res.P95)/float64(p.P95) - 1
			switch {
			case ratio > band:
				c.Change = Worse
			case ratio < -band:
				c.Change = Better
			default:
				c.Change = Same
			}
		}
		out = append(out, c)
	}
	return out
}

// Concern is a route of the latest scan that needs attention, and why.
type Concern struct {
	RouteResult
	Why string
}

// Critical is the routes of the latest scan that failed, that are not
// scanned now but were on the previous scan, or whose p95 at the top
// rung is over the budget; worst first as Routes orders them.
func Critical(latest Run, previous *Run, budget time.Duration) []Concern {
	before := map[string]RouteResult{}
	var when string
	if previous != nil {
		when = previous.CreatedAt.UTC().Format("2006-01-02")
		for _, res := range Routes(*previous) {
			if res.Scanned() {
				before[res.key()] = res
			}
		}
	}
	var out []Concern
	for _, res := range Routes(latest) {
		switch {
		case res.Scanned():
			if res.P95 > budget {
				out = append(out, Concern{res, fmt.Sprintf("p95 %s at %d in flight, over the %s budget", Dur(res.P95), res.Concurrency, Dur(budget))})
			}
		case len(res.Skipped) >= 7 && res.Skipped[:7] == "failed:":
			out = append(out, Concern{res, res.Skipped})
		default:
			if _, was := before[res.key()]; was {
				out = append(out, Concern{res, fmt.Sprintf("not scanned (%s), scanned on %s", res.Skipped, when)})
			}
		}
	}
	return out
}

// Point is one route on one scan.
type Point struct {
	RunID, Date, Version string
	Concurrency          int
	P95                  time.Duration
	PerSecond            float64
	Skipped              string
}

// RouteTrend is one route across the scans, oldest first.
type RouteTrend struct {
	Route, Method, Path string
	Points              []Point
}

// Trend is each route across the scans given newest first, its points
// oldest first and a point for a scan that did not drive it; the routes
// ordered as the latest scan ranks them, routes the latest scan does not
// list after.
func Trend(newestFirst []Run) []RouteTrend {
	if len(newestFirst) == 0 {
		return nil
	}
	var out []RouteTrend
	at := map[string]int{}
	for _, res := range Routes(newestFirst[0]) {
		at[res.key()] = len(out)
		out = append(out, RouteTrend{Route: res.Route, Method: res.Method, Path: res.Path})
	}
	for i := len(newestFirst) - 1; i >= 0; i-- {
		r := newestFirst[i]
		for _, res := range Routes(r) {
			j, ok := at[res.key()]
			if !ok {
				at[res.key()] = len(out)
				j = len(out)
				out = append(out, RouteTrend{Route: res.Route, Method: res.Method, Path: res.Path})
			}
			out[j].Points = append(out[j].Points, Point{RunID: r.RunID, Date: r.CreatedAt.UTC().Format("2006-01-02"), Version: r.Version,
				Concurrency: res.Concurrency, P95: res.P95, PerSecond: res.PerSecond, Skipped: res.Skipped})
		}
	}
	return out
}
