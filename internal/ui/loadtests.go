package ui

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/load"
	"git.bytestone.uk/hum3/gobank-deploy/internal/perf"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
)

// loadTest starts a load test: a new environment named on the form,
// created at its scale, loaded from a loader beside it and both removed
// by the load workflow.
func (s *Server) loadTest(w http.ResponseWriter, r *http.Request) {
	if why := s.upUnavailable(); why != "" {
		s.fail(w, r, http.StatusForbidden, "load test not available here: "+why)
		return
	}
	name := r.FormValue("name")
	if !envName.MatchString(name) {
		s.fail(w, r, http.StatusBadRequest, fmt.Sprintf("environment name %q: use lower-case letters, digits and hyphens", name))
		return
	}
	if _, exists := s.environment(r.Context(), name); exists {
		s.fail(w, r, http.StatusConflict, name+": already an environment; a load test wants a fresh one")
		return
	}
	scale := r.FormValue("scale")
	if scale == "" {
		scale = "small"
	}
	env := deploy.Environment{Name: name}
	run := func(ctx context.Context, out io.Writer) error { return s.op.Load(ctx, env, scale, out) }
	if err := s.start(env, "load", run); err != nil {
		s.fail(w, r, http.StatusConflict, fmt.Sprintf("%s: %v", env.Name, err))
		return
	}
	s.done(w, r)
}

// loadView is a load test as the pages show it.
type loadView struct {
	load.Run
	Date, Box, Customers string
	Screens              []string // the journey's screens, the rungs' columns
	Rungs                []rungView
}

// rungView is a rung of the climb as the run page shows it.
type rungView struct {
	Users, Failed      int
	Journeys           string
	P95                []string // a screen's p95, in Screens order
	Journey, PerSecond string
	Breach             string
}

func viewLoad(r load.Run) loadView {
	v := loadView{Run: r, Date: r.CreatedAt.Local().Format("Mon 2 Jan 2006 15:04"), Box: fmt.Sprintf("%s, %g GB", r.ServerType, r.MemoryGB),
		Customers: groupInt(int64(r.Customers)), Screens: load.Customer.Screens()}
	for _, rg := range r.Rungs {
		rv := rungView{Users: rg.Users, Journeys: groupInt(int64(rg.Journeys)), Failed: rg.Failed, Journey: latency(rg.Journey.P95),
			PerSecond: strconv.FormatFloat(rg.PerSecond, 'f', 1, 64), Breach: rg.Breach(r.Budget)}
		for _, name := range v.Screens {
			p95 := ""
			for _, sc := range rg.Screens {
				if sc.Name == name {
					p95 = latency(sc.P95)
				}
			}
			rv.P95 = append(rv.P95, p95)
		}
		v.Rungs = append(v.Rungs, rv)
	}
	return v
}

// loadTests is the load tests page: every test, and the capacity against
// gobank's version, a line per scale.
func (s *Server) loadTests(w http.ResponseWriter, r *http.Request) {
	list, err := s.op.Loads(r.Context(), 200)
	var views []loadView
	for _, l := range list {
		views = append(views, viewLoad(l))
	}
	versions, scales, at := capacityTrend(list)
	var legend []legendView
	var lines []chartSeries
	for i, sc := range scales {
		legend = append(legend, legendView{Label: fmt.Sprintf("%s (%s)", sc.Scale, viewLoad(sc).Box), Colour: seriesColours[i%len(seriesColours)]})
		var c chartSeries
		for _, v := range versions {
			if run, ok := at[sc.Scale+" "+v]; ok {
				c.Points = append(c.Points, chartPoint{X: v, Y: float64(run.Capacity), Title: fmt.Sprintf("%s · %s: %d users", sc.Scale, v, run.Capacity)})
			}
		}
		lines = append(lines, c)
	}
	var rows []routesRow
	for _, v := range versions {
		row := routesRow{Version: v}
		for _, sc := range scales {
			cell := ""
			if run, ok := at[sc.Scale+" "+v]; ok {
				cell = strconv.Itoa(run.Capacity)
			}
			row.Cells = append(row.Cells, cell)
		}
		rows = append(rows, row)
	}
	chart := lineChart(versions, lines, "capacity in users by gobank version, a line per scale", func(y float64) string { return strconv.FormatFloat(y, 'f', -1, 64) }, 1)
	s.render(w, "load", map[string]any{"loads": views, "legend": legend, "rows": rows, "chart": chart, "error": err})
}

// capacityTrend is the load tests by version, oldest first by number, and
// scale, the smaller box first; where a scale was tested more than once
// at a version the latest stands. at is keyed "<scale> <version>"; each
// of scales is a run that stands for its scale's box.
func capacityTrend(runs []load.Run) (versions []string, scales []load.Run, at map[string]load.Run) {
	at = map[string]load.Run{}
	box := map[string]load.Run{}
	for _, r := range runs {
		if r.Rungs == nil && r.Capacity == 0 && r.Breach == "" {
			continue // not yet loaded
		}
		k := r.Scale + " " + r.Version
		if have, ok := at[k]; !ok || r.CreatedAt.After(have.CreatedAt) {
			at[k] = r
		}
		if !slices.Contains(versions, r.Version) {
			versions = append(versions, r.Version)
		}
		box[r.Scale] = r
	}
	slices.SortFunc(versions, perf.CompareVersions)
	for _, r := range box {
		scales = append(scales, r)
	}
	slices.SortFunc(scales, func(a, b load.Run) int {
		return cmp.Or(cmp.Compare(a.MemoryGB, b.MemoryGB), cmp.Compare(a.Scale, b.Scale))
	})
	return versions, scales, at
}

// latency writes a p95 to the precision the budget is read at: whole
// milliseconds under a second, tenths of a second under a minute.
func latency(d time.Duration) string {
	if d < time.Minute && d >= time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return scenario.Dur(d)
}
