// Package scenario records gobank-deploy's scenario runs — a named
// configuration driven through timed steps on a fresh environment — and
// renders each as a page of the journal, the notebook of runs kept in
// this repository's journal/ folder.
package scenario

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is what a scenario asks for.
type Config struct {
	Name      string        // the scenario, e.g. seven-years
	Customers int           // added flat out before the years run
	Years     int           // simulated years run flat out
	DayLength time.Duration // the day length set after the years: wall clock
	Search    string        // the name the customer search is probed with
}

// Figure is one labelled value a step measured.
type Figure struct {
	Label string
	Value string
}

// Step is one timed step of a run with what it measured.
type Step struct {
	Name     string
	Started  time.Time
	Duration time.Duration
	Figures  []Figure
}

// Probe is one route measured from outside at one concurrency.
type Probe struct {
	Route       string // what the route is, e.g. "customer search by name"
	Path        string // the path probed, e.g. "/customers?q=Smith"
	Concurrency int
	N           int
	P50, P95    time.Duration
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

// Title names the run: its date, scenario and scale.
func Title(r Run) string {
	return fmt.Sprintf("%s %s at %s", r.CreatedAt.UTC().Format("2006-01-02"), r.Config.Name, r.Scale)
}

// Filename is the run's page in the journal folder.
func Filename(r Run) string {
	return fmt.Sprintf("%s-%s-%s.md", r.CreatedAt.UTC().Format("2006-01-02"), r.Config.Name, r.Scale)
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
	fmt.Fprintf(&b, "| Search | %s |\n\n", r.Config.Search)

	b.WriteString("| Step | Started | Duration | Figures |\n|---|---|---|---|\n")
	for _, s := range r.Steps {
		var figs []string
		for _, f := range s.Figures {
			figs = append(figs, f.Label+" "+f.Value)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", s.Name, s.Started.UTC().Format("15:04:05"), Dur(s.Duration), strings.Join(figs, "; "))
	}
	b.WriteString("\n")

	b.WriteString("| Route | Path | Concurrency | n | p50 | p95 |\n|---|---|---|---|---|---|\n")
	for _, p := range r.Probes {
		fmt.Fprintf(&b, "| %s | `%s` | %d | %d | %s | %s |\n", p.Route, p.Path, p.Concurrency, p.N, Dur(p.P50), Dur(p.P95))
	}
	b.WriteString("\n## Notes\n")
	return b.String()
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
