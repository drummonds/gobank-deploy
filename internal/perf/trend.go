package perf

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Series is one scale's points of a route's trend, oldest version first.
type Series struct {
	Scale  string
	Points []Point
}

// Point is a route's figures in the run that stands for its scale at a
// version.
type Point struct {
	Version string
	RunID   string
	Count   int64
	P95     time.Duration
}

// Trend is the route's p95 across gobank's versions: the versions any run
// measured it at, oldest first by version number, and a series per scale,
// the smaller box first. Where a scale was run more than once at a
// version the latest run stands. Nil when no run measured the route.
func Trend(runs []Run, pattern string) (versions []string, series []Series) {
	type key struct{ scale, version string }
	latest := map[key]Run{}
	memory := map[string]float64{}
	for _, r := range runs {
		if _, ok := find(r.Routes, pattern); !ok {
			continue
		}
		k := key{r.Scale, r.Version}
		if have, ok := latest[k]; !ok || r.CreatedAt.After(have.CreatedAt) {
			latest[k] = r
		}
		memory[r.Scale] = r.MemoryGB
	}
	if len(latest) == 0 {
		return nil, nil
	}
	for k := range latest {
		if !slices.Contains(versions, k.version) {
			versions = append(versions, k.version)
		}
	}
	slices.SortFunc(versions, compareVersions)
	var scales []string
	for s := range memory {
		scales = append(scales, s)
	}
	slices.SortFunc(scales, func(a, b string) int { return cmp.Or(cmp.Compare(memory[a], memory[b]), cmp.Compare(a, b)) })
	for _, s := range scales {
		sr := Series{Scale: s}
		for _, v := range versions {
			if r, ok := latest[key{s, v}]; ok {
				rt, _ := find(r.Routes, pattern)
				sr.Points = append(sr.Points, Point{Version: v, RunID: r.RunID, Count: rt.Count, P95: rt.P95})
			}
		}
		series = append(series, sr)
	}
	return versions, series
}

// Patterns is every route any run measured, the busiest across the runs
// first.
func Patterns(runs []Run) []string {
	total := map[string]int64{}
	for _, r := range runs {
		for _, rt := range r.Routes {
			total[rt.Pattern] += rt.Count
		}
	}
	var out []string
	for p := range total {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Or(cmp.Compare(total[b], total[a]), cmp.Compare(a, b)) })
	return out
}

func find(routes []Route, pattern string) (Route, bool) {
	for _, rt := range routes {
		if rt.Pattern == pattern {
			return rt, true
		}
	}
	return Route{}, false
}

// compareVersions orders gobank's tags by number, v0.9.0 before v0.10.0;
// a tag that is not vX.Y.Z sorts as text after the numbered ones.
func compareVersions(a, b string) int {
	na, oka := numbers(a)
	nb, okb := numbers(b)
	switch {
	case oka && okb:
		return cmp.Or(slices.Compare(na, nb), cmp.Compare(a, b))
	case oka:
		return -1
	case okb:
		return 1
	}
	return cmp.Compare(a, b)
}

func numbers(v string) ([]int, bool) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
