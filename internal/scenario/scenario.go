// Package scenario records gobank-deploy's scenario runs — a named
// configuration driven through timed steps on a fresh environment — and
// renders each as a page of the journal, the notebook of runs kept in
// this repository's journal/ folder.
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is what a scenario asks for.
type Config struct {
	Name      string        // the scenario, e.g. seven-years, or scan
	Customers int           // added flat out before the years run; for a scan, on the books
	Years     int           // bank years run flat out
	DayLength time.Duration // wall clock per bank day, set after the years
	Search    string        // the name the customer search is probed with
	Ladder    []int         // the concurrencies each route is probed at
	Samples   int           // calls per route per rung
}

// Figure is one labelled value a step measured.
type Figure struct {
	Label string
	Value string
}

// Step is one timed step of a run with what it measured. Duration is
// the step's wall clock; a figure says which clock it is on, the bank's
// calendar (bank days, from, to) or the wall (last day), and a rate is a
// bank quantity per wall-clock unit over this duration, so the figures
// on a page multiply out.
type Step struct {
	Name     string
	Started  time.Time
	Duration time.Duration
	Figures  []Figure
}

// Probe is one route measured from outside at one concurrency, or a
// route a scan could not drive, with why.
type Probe struct {
	Route       string // what the route is, e.g. "customer search by name"
	Method      string // GET or POST
	Path        string // the path probed, e.g. "/customers/search"
	Concurrency int
	N           int
	P50, P95    time.Duration
	PerSecond   float64 // requests completed a second over the measurement
	Skipped     string  // why the route was not driven; "" when it was
}

// Run is one scenario run: where it ran, what was asked, and what each
// step measured. It is a workflow instance, RunID that instance's id.
type Run struct {
	RunID       string
	Environment string
	Scale       string  // small, large, ... as asked for
	ServerType  string  // the provider's type the scale became, e.g. ccx33
	MemoryGB    float64 // the server's RAM, shared with PostgreSQL
	Version     string  // the gobank release run
	CreatedAt   time.Time
	Config      Config
	Steps       []Step
	Probes      []Probe
}

// where is the scale the run made its environment at, or the
// environment itself for a run on one as it stands (a scan).
func where(r Run) string {
	if r.Scale != "" {
		return r.Scale
	}
	return r.Environment
}

// Title names the run: its date, scenario and scale.
func Title(r Run) string {
	return fmt.Sprintf("%s %s at %s", r.CreatedAt.UTC().Format("2006-01-02"), r.Config.Name, where(r))
}

// Filename is the run's page in the journal folder.
func Filename(r Run) string {
	return fmt.Sprintf("%s-%s-%s.md", r.CreatedAt.UTC().Format("2006-01-02"), r.Config.Name, where(r))
}

// ladder writes the rungs as "1, 4, 16".
func ladder(l []int) string {
	var parts []string
	for _, c := range l {
		parts = append(parts, fmt.Sprint(c))
	}
	return strings.Join(parts, ", ")
}

// Page renders the run as its journal page: where it ran, the
// configuration, the steps, the probes, and an empty Notes section for
// the operator's hand. The runner writes a page once and never again.
func Page(r Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", Title(r))
	fmt.Fprintf(&b, "gobank %s on %s (%s, %g GB), environment `%s`; the workflow run is `/workflows/%s` on the host that drove it.\n\n",
		r.Version, r.Scale, r.ServerType, r.MemoryGB, r.Environment, r.RunID)

	b.WriteString("| Configuration | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Customers | %s |\n", Comma(int64(r.Config.Customers)))
	fmt.Fprintf(&b, "| Years | %d |\n", r.Config.Years)
	fmt.Fprintf(&b, "| Day length | %s |\n", Dur(r.Config.DayLength))
	fmt.Fprintf(&b, "| Search | %s |\n", r.Config.Search)
	fmt.Fprintf(&b, "| Ladder | %s |\n", ladder(r.Config.Ladder))
	fmt.Fprintf(&b, "| Samples | %d |\n\n", r.Config.Samples)

	b.WriteString("| Step | Started | Wall clock | Figures |\n|---|---|---|---|\n")
	for _, s := range r.Steps {
		var figs []string
		for _, f := range s.Figures {
			figs = append(figs, f.Label+" "+f.Value)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", s.Name, s.Started.UTC().Format("15:04:05"), Dur(s.Duration), strings.Join(figs, "; "))
	}
	b.WriteString("\n")

	b.WriteString("| Route | Method | Path | Concurrency | n | p50 | p95 | per s |\n|---|---|---|---|---|---|---|---|\n")
	var skipped []string
	for _, p := range r.Probes {
		if p.Skipped != "" {
			skipped = append(skipped, fmt.Sprintf("`%s %s` (%s)", p.Method, p.Path, p.Skipped))
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` | %d | %d | %s | %s | %.1f |\n", p.Route, p.Method, p.Path, p.Concurrency, p.N, Dur(p.P50), Dur(p.P95), p.PerSecond)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "\nNot scanned: %s.\n", strings.Join(skipped, "; "))
	}
	b.WriteString("\n## Notes\n")
	return b.String()
}

// Measured is the scan as gobank's API pages read it, api/measured.json
// in gobank's checkout: where and when, a row per route and rung, and
// the routes not scanned with why.
type Measured struct {
	Date        string          `json:"date"`
	Version     string          `json:"version"` // gobank's
	Environment string          `json:"environment"`
	Server      string          `json:"server"`
	MemoryGB    float64         `json:"memory_gb"`
	Customers   int             `json:"customers"`
	Ladder      []int           `json:"ladder"`
	Samples     int             `json:"samples"`
	Routes      []MeasuredRoute `json:"routes"`
	NotScanned  []NotScanned    `json:"not_scanned"`
}

// MeasuredRoute is one route's rungs.
type MeasuredRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Rungs  []Rung `json:"rungs"`
}

// Rung is one route at one concurrency.
type Rung struct {
	Concurrency int     `json:"concurrency"`
	N           int     `json:"n"`
	P50ms       float64 `json:"p50_ms"`
	P95ms       float64 `json:"p95_ms"`
	PerSecond   float64 `json:"per_second"`
}

// NotScanned is a route the scan could not drive.
type NotScanned struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Why    string `json:"why"`
}

// MeasuredOf is the run's probes as the measured document.
func MeasuredOf(r Run) Measured {
	m := Measured{Date: r.CreatedAt.UTC().Format("2006-01-02"), Version: r.Version, Environment: r.Environment, Server: r.ServerType, MemoryGB: r.MemoryGB,
		Customers: r.Config.Customers, Ladder: r.Config.Ladder, Samples: r.Config.Samples, Routes: []MeasuredRoute{}, NotScanned: []NotScanned{}}
	for _, p := range r.Probes {
		if p.Skipped != "" {
			m.NotScanned = append(m.NotScanned, NotScanned{Method: p.Method, Path: p.Path, Why: p.Skipped})
			continue
		}
		rung := Rung{Concurrency: p.Concurrency, N: p.N, P50ms: ms(p.P50), P95ms: ms(p.P95), PerSecond: p.PerSecond}
		if n := len(m.Routes); n > 0 && m.Routes[n-1].Method == p.Method && m.Routes[n-1].Path == p.Path {
			m.Routes[n-1].Rungs = append(m.Routes[n-1].Rungs, rung)
			continue
		}
		m.Routes = append(m.Routes, MeasuredRoute{Method: p.Method, Path: p.Path, Rungs: []Rung{rung}})
	}
	return m
}

// MeasuredJSON is the measured document as written into gobank.
func MeasuredJSON(r Run) ([]byte, error) {
	b, err := json.MarshalIndent(MeasuredOf(r), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ms is a latency in milliseconds to a tenth.
func ms(d time.Duration) float64 {
	return float64(d.Round(100*time.Microsecond)) / float64(time.Millisecond)
}

// IndexLine is the run's line in the journal's index.
func IndexLine(r Run) string {
	return fmt.Sprintf("- [%s](%s) — gobank %s, %s customers, %d years", Title(r), Filename(r), r.Version, Comma(int64(r.Config.Customers)), r.Config.Years)
}

// UpdateIndex puts the run's line into the journal's index, newest first:
// above the first entry there, in place of its own earlier line, or at
// the end of an index with no entries yet.
func UpdateIndex(index string, r Run) (string, error) {
	if strings.TrimSpace(index) == "" {
		return "", errors.New("journal index is empty: not a journal")
	}
	line, name := IndexLine(r), "("+Filename(r)+")"
	lines := strings.Split(index, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "- [") && strings.Contains(l, name) {
			lines[i] = line
			return strings.Join(lines, "\n"), nil
		}
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "- [") {
			lines = append(lines[:i], append([]string{line}, lines[i:]...)...)
			return strings.Join(lines, "\n"), nil
		}
	}
	return strings.TrimRight(index, "\n") + "\n\n" + line + "\n", nil
}

// Comma formats n with thousands separators.
func Comma(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// Dur formats a duration the way a person reads it: whole seconds above
// a second with the empty minutes and seconds dropped (24h, 3h12m,
// 1m25s), milliseconds below.
func Dur(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
		if strings.HasSuffix(s, "h0m") {
			s = strings.TrimSuffix(s, "0m")
		}
	}
	return s
}
