package flows

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
)

// WorkflowScan is the scan workflow's type.
const WorkflowScan wf.WorkflowType = "scan"

const scanSrc = "https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/scan.go"

// ScanDefinition describes the workflow for the registry and the pages.
var ScanDefinition = wf.Definition{
	Type:        WorkflowScan,
	Description: "A full performance scan of an environment as it stands: every route the demo's API document lists that the scan can drive — a GET without path parameters as the operator or as a customer by what the route needs, the customer search by its post — loaded at each rung of the concurrency ladder, the rest listed as not scanned with why. A scenario run kept in the store, written as a page of the journal, and the measured document for gobank's API pages. No server is made or removed.",
	InstanceKey: "environment date",
	Steps: []wf.Step{
		{Name: "survey", Description: "the environment must be serving; record where the scan is: server type, RAM, gobank version, customers on the books", Source: scanSrc},
		{Name: "scan", Description: "read /v1/openapi.json; load every route the scan can drive at each rung, Samples calls each; a route that fails or cannot be driven is recorded as not scanned", Source: scanSrc},
	},
}

func init() { wf.Definitions = append(wf.Definitions, ScanDefinition) }

// customerID is the customer the scan signs in as for the customer
// routes: the first one on the books.
const customerID = "cust-001"

// Scan drives the scan on env's demo as it stands and returns its
// record. search is the name the customer search is posted with. One
// instance per environment and date: a scan that failed resumes.
func (s *Scenario) Scan(ctx context.Context, env deploy.Environment, search string, out io.Writer) (*scenario.Run, error) {
	if search == "" {
		search = "Jones"
	}
	key := fmt.Sprintf("%s %s", env.Name, s.now().Format("2006-01-02"))
	run := &scenarioRun{s: s, env: env, key: key, out: out, wfType: WorkflowScan,
		cfg: scenario.Config{Name: "scan", Search: search, Ladder: s.ladder(), Samples: s.samples()}}
	stages := []wf.Stage{
		{Name: "survey", Run: run.survey},
		{Name: "scan", Run: run.scan},
	}
	if _, err := wf.NewRunner(s.Store, nil).RunPipeline(ctx, WorkflowScan, key, wf.BusinessDateFrom(s.now()), stages); err != nil {
		return nil, err
	}
	rec, err := run.record(ctx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%s completed but left no record", key)
	}
	return rec, nil
}

// survey records where the scan is: the environment must be serving.
func (r *scenarioRun) survey(ctx context.Context) error {
	fmt.Fprintf(r.out, "== survey %s\n", r.env.Name)
	st, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	rec, err := r.record(ctx)
	if err != nil {
		return err
	}
	cfg := r.cfg
	if rd.Position != nil {
		cfg.Customers = rd.Position.Customers
	}
	if rec == nil {
		if err := r.s.Scenarios.Create(ctx, scenario.Run{RunID: r.runID, Environment: r.env.Name,
			ServerType: st.Server.Type, MemoryGB: st.Server.MemoryGB, Version: rd.Version, CreatedAt: r.s.now(), Config: cfg}); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "%s serving %s on %s (%g GB), %d customers\n", r.env.Name, rd.Version, st.Server.Type, st.Server.MemoryGB, cfg.Customers)
	return nil
}

// openapi is the demo's API document as the scan reads it: for each
// path and method, what the route needs and takes.
type openapi struct {
	Paths map[string]map[string]operation `json:"paths"`
}

type operation struct {
	Parameters []struct {
		Name string `json:"name"`
		In   string `json:"in"`
	} `json:"parameters"`
	Security    []map[string][]string `json:"security"`
	RequestBody json.RawMessage       `json:"requestBody"`
}

// needs says whether the operation accepts the scheme ("mb_session",
// "staff_session", "bearer").
func (o operation) needs(scheme string) bool {
	for _, alt := range o.Security {
		if _, ok := alt[scheme]; ok {
			return true
		}
	}
	return false
}

// scan loads every route the document lists that the scan can drive, at
// each rung of the ladder, and records the rest as not scanned with why.
func (r *scenarioRun) scan(ctx context.Context) error {
	fmt.Fprintln(r.out, "== scan")
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
	}
	st, _, err := r.read(ctx)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(st.URL, "/")
	body, _, err := r.s.Prober.Fetch(ctx, base+"/v1/openapi.json", "")
	if err != nil {
		return fmt.Errorf("reading the API document: %w", err)
	}
	var doc openapi
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return fmt.Errorf("the API document: %w", err)
	}
	started := r.s.now()

	// As a customer, signed in once and only if a customer route asks.
	var customer fetcher
	asCustomer := func() (fetcher, string) {
		if customer != nil {
			return customer, ""
		}
		if st.AppPassword == "" {
			return nil, "no app password to sign a customer in with"
		}
		token, err := r.s.Prober.Login(ctx, base, customerID, st.AppPassword)
		if err != nil {
			return nil, err.Error()
		}
		customer = func(ctx context.Context, url string) (string, time.Duration, error) {
			return r.s.Prober.Fetch(ctx, url, token)
		}
		return customer, ""
	}

	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var probes []scenario.Probe
	scanned, skipped := 0, 0
	for _, path := range paths {
		methods := make([]string, 0, len(doc.Paths[path]))
		for m := range doc.Paths[path] {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		for _, m := range methods {
			op, method := doc.Paths[path][m], strings.ToUpper(m)
			route := method + " " + path
			skip := func(why string) {
				probes = append(probes, scenario.Probe{Route: route, Method: method, Path: path, Skipped: why})
				skipped++
				fmt.Fprintf(r.out, "%s: not scanned, %s\n", route, why)
			}
			var fetch fetcher
			switch {
			case strings.Contains(path, "{"):
				skip("needs a path parameter")
				continue
			case method == "POST" && path == "/customers/search":
				fetch = r.searching(st)
			case method == "POST":
				skip("a post that is not the customer search")
				continue
			case method != "GET":
				skip("not a method the scan drives")
				continue
			case op.needs("mb_session"):
				var why string
				if fetch, why = asCustomer(); why != "" {
					skip(why)
					continue
				}
			default:
				fetch = r.asOperator(st)
			}
			rungs, _, err := r.rungs(ctx, base, route, method, path, fetch)
			if err != nil {
				skip("failed: " + err.Error())
				continue
			}
			probes = append(probes, rungs...)
			scanned++
		}
	}
	rec.Probes = probes
	return r.step(ctx, rec, scenario.Step{Name: "scan", Started: started, Duration: r.s.now().Sub(started), Figures: []scenario.Figure{
		{Label: "routes scanned", Value: fmt.Sprint(scanned)},
		{Label: "not scanned", Value: fmt.Sprint(skipped)},
		{Label: "ladder", Value: fmt.Sprint(r.cfg.Ladder)},
		{Label: "samples", Value: fmt.Sprint(r.cfg.Samples)},
	}})
}
