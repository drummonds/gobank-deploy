package ui

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/perf"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
)

// seriesColours are the categorical slots a scale's line takes, in order:
// the smallest box the first.
var seriesColours = []string{"#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"}

// perfRouteView is one route of a perf run as its page shows it.
type perfRouteView struct {
	Pattern, Count, P95 string
}

func viewPerfRoutes(routes []perf.Route) []perfRouteView {
	var out []perfRouteView
	for _, rt := range routes {
		out = append(out, perfRouteView{Pattern: rt.Pattern, Count: groupInt(rt.Count), P95: scenario.Dur(rt.P95)})
	}
	return out
}

// legendView is a scale's line: its name, its box and its colour.
type legendView struct{ Label, Colour string }

// routesRow is a version's row of the table: a cell per scale.
type routesRow struct {
	Version string
	Cells   []string
}

// perfRoutes is the routes page: one route's p95 across gobank's
// versions, a line per scale, and the same as a table.
func (s *Server) perfRoutes(w http.ResponseWriter, r *http.Request) {
	list, err := s.op.Perfs(r.Context(), 200)
	patterns := perf.Patterns(list)
	route := r.URL.Query().Get("route")
	if route == "" && len(patterns) > 0 {
		route = patterns[0]
	}
	versions, series := perf.Trend(list, route)
	box := map[string]string{}
	for _, p := range list {
		box[p.Scale] = fmt.Sprintf("%s (%s, %g GB)", p.Scale, p.ServerType, p.MemoryGB)
	}
	var legend []legendView
	for i, sr := range series {
		legend = append(legend, legendView{Label: box[sr.Scale], Colour: seriesColours[i%len(seriesColours)]})
	}
	var rows []routesRow
	for _, v := range versions {
		row := routesRow{Version: v}
		for _, sr := range series {
			cell := ""
			for _, p := range sr.Points {
				if p.Version == v {
					cell = scenario.Dur(p.P95) + " · " + groupInt(p.Count)
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		rows = append(rows, row)
	}
	s.render(w, "routes", map[string]any{"patterns": patterns, "route": route, "legend": legend, "rows": rows,
		"chart": trendChart(versions, series), "error": err})
}

// trendChart draws a route's p95 by version, a line per scale in the
// legend's colours, as inline SVG: the versions evenly along the x axis,
// the p95 up from zero, each point titled with its figures for hover.
func trendChart(versions []string, series []perf.Series) template.HTML {
	if len(versions) == 0 {
		return ""
	}
	const width, height, left, right, top, bottom = 720.0, 300.0, 64.0, 24.0, 16.0, 40.0
	var top95 time.Duration
	for _, sr := range series {
		for _, p := range sr.Points {
			top95 = max(top95, p.P95)
		}
	}
	step := niceStep(top95 / 4)
	ceiling := step
	for ceiling < top95 {
		ceiling += step
	}
	at := map[string]int{}
	for i, v := range versions {
		at[v] = i
	}
	x := func(v string) float64 {
		if len(versions) == 1 {
			return left + (width-left-right)/2
		}
		return left + float64(at[v])*(width-left-right)/float64(len(versions)-1)
	}
	y := func(d time.Duration) float64 {
		return height - bottom - float64(d)/float64(ceiling)*(height-top-bottom)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="trend" viewBox="0 0 %g %g" role="img" aria-label="p95 by gobank version, a line per scale">`, width, height)
	for t := time.Duration(0); t <= ceiling; t += step {
		fmt.Fprintf(&b, `<line class="grid" x1="%g" x2="%g" y1="%.1f" y2="%.1f"/><text class="tick" x="%g" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`,
			left, width-right, y(t), y(t), left-8, y(t), scenario.Dur(t))
	}
	every := int(math.Ceil(float64(len(versions)) / 10))
	for i, v := range versions {
		if i%every == 0 {
			fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%g" text-anchor="middle">%s</text>`, x(v), height-bottom+20, template.HTMLEscapeString(v))
		}
	}
	for i, sr := range series {
		colour := seriesColours[i%len(seriesColours)]
		var pts []string
		for _, p := range sr.Points {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(p.Version), y(p.P95)))
		}
		fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" points="%s"/>`, colour, strings.Join(pts, " "))
		for _, p := range sr.Points {
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="#fff" stroke-width="2"><title>%s · %s: p95 %s over %s requests</title></circle>`,
				x(p.Version), y(p.P95), colour, template.HTMLEscapeString(sr.Scale), template.HTMLEscapeString(p.Version), scenario.Dur(p.P95), groupInt(p.Count))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// niceStep is the round step — 1, 2 or 5 of a power of ten nanoseconds —
// at or above d, so the axis reads in whole milliseconds and the like.
func niceStep(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Millisecond
	}
	pow := time.Duration(math.Pow(10, math.Floor(math.Log10(float64(d)))))
	for _, m := range []time.Duration{1, 2, 5, 10} {
		if m*pow >= d {
			return m * pow
		}
	}
	return 10 * pow
}
