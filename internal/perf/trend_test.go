package perf

import (
	"reflect"
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// A route's trend is its p95 across gobank's versions, oldest first by
// version number, with a series per scale so the same box is followed
// over releases and the boxes are compared; the smaller box comes first.
// Where a scale was run more than once at a version the latest run
// stands. A run that did not serve the route leaves no point.
func TestTrendIsARoutesP95ByVersionWithASeriesPerScale(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	route := func(n int64, p95 int) []Route {
		return []Route{{Pattern: "GET /v1/health", Count: n, P95: ms(p95)}, {Pattern: "/customers/", Count: 1, P95: ms(1)}}
	}
	runs := []Run{ // newest first, as List gives them
		{RunID: "r5", Scale: "large", MemoryGB: 32, Version: "v0.10.0", CreatedAt: t0.Add(5 * time.Hour), Routes: route(50, 7)},
		{RunID: "r4", Scale: "small", MemoryGB: 4, Version: "v0.10.0", CreatedAt: t0.Add(4 * time.Hour), Routes: route(40, 30)},
		{RunID: "r3", Scale: "small", MemoryGB: 4, Version: "v0.9.0", CreatedAt: t0.Add(3 * time.Hour), Routes: route(30, 25)},
		{RunID: "r2", Scale: "small", MemoryGB: 4, Version: "v0.9.0", CreatedAt: t0.Add(2 * time.Hour), Routes: route(20, 99)},
		{RunID: "r1", Scale: "large", MemoryGB: 32, Version: "v0.9.0", CreatedAt: t0.Add(time.Hour), Routes: route(10, 5)},
		{RunID: "r0", Scale: "large", MemoryGB: 32, Version: "v0.8.0", CreatedAt: t0}, // before gobank measured its routes
	}
	versions, series := Trend(runs, "GET /v1/health")
	if want := []string{"v0.9.0", "v0.10.0"}; !reflect.DeepEqual(versions, want) {
		t.Errorf("versions = %v, want %v", versions, want)
	}
	want := []Series{
		{Scale: "small", Points: []Point{{Version: "v0.9.0", RunID: "r3", Count: 30, P95: ms(25)}, {Version: "v0.10.0", RunID: "r4", Count: 40, P95: ms(30)}}},
		{Scale: "large", Points: []Point{{Version: "v0.9.0", RunID: "r1", Count: 10, P95: ms(5)}, {Version: "v0.10.0", RunID: "r5", Count: 50, P95: ms(7)}}},
	}
	if !reflect.DeepEqual(series, want) {
		t.Errorf("series = %+v\nwant %+v", series, want)
	}
	if versions, series := Trend(runs, "GET /nowhere"); versions != nil || series != nil {
		t.Errorf("a route never served has no trend: %v %v", versions, series)
	}
}

// The routes a trend can be drawn for are every route any run measured,
// the busiest across the runs first.
func TestPatternsAreEveryMeasuredRouteBusiestFirst(t *testing.T) {
	runs := []Run{
		{RunID: "r2", Routes: []Route{{Pattern: "/a", Count: 5}, {Pattern: "/b", Count: 1}}},
		{RunID: "r1", Routes: []Route{{Pattern: "/b", Count: 9}, {Pattern: "/c", Count: 2}}},
		{RunID: "r0"},
	}
	if got, want := Patterns(runs), []string{"/b", "/a", "/c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("patterns = %v, want %v", got, want)
	}
}
