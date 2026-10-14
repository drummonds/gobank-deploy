package flows

import (
	"context"
	"strings"
	"testing"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
)

// openapiDoc is the demo's API document as a scan reads it: what each
// route needs, whether it takes path parameters or a body.
const openapiDoc = `{"openapi":"3.1.0","paths":{
 "/about":{"get":{"security":[{"staff_session":[]},{"bearer":[]}]}},
 "/about.json":{"get":{"security":[]}},
 "/movements/{id}":{"get":{"parameters":[{"name":"id","in":"path"}],"security":[{"staff_session":[]},{"bearer":[]}]}},
 "/customers/search":{"post":{"security":[{"staff_session":[]},{"bearer":[]}]}},
 "/settings":{"get":{"security":[{"staff_session":[]},{"bearer":[]}]},"post":{"security":[{"staff_session":[]},{"bearer":[]}]}},
 "/v1/screen/home":{"get":{"security":[{"mb_session":[]},{"bearer":[]}]}},
 "/v1/login":{"post":{"requestBody":{},"security":[]}}
}}`

// A scan loads every route the demo's API document lists that it can
// drive — a GET without path parameters as the operator or as a
// customer by what the route needs, the customer search by its post —
// at each rung of the ladder, records the rest as not scanned with why,
// and keeps the run as a scenario run on the environment as it stands:
// no server made or removed.
func TestScanLoadsEveryRouteItCanDrive(t *testing.T) {
	h := newScenarioHarness(t)
	h.ops.servers["preprod"] = &deploy.Server{Name: "gobank-preprod", IP: "10.0.0.9", Type: "ccx33", MemoryGB: 32}
	h.prob.pages = map[string]string{"/v1/openapi.json": openapiDoc, "/customers/search": `<a href="/customers/abc-123">Ann Smith</a>`}
	h.con.demo("http://preprod/").customers = 1234

	rec, err := h.flow.Scan(context.Background(), deploy.Environment{Name: "preprod"}, "Smith", &h.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ops.ups) != 0 || h.ops.downs != 0 {
		t.Errorf("ups %d downs %d; a scan makes and removes nothing", len(h.ops.ups), h.ops.downs)
	}
	if rec.Config.Name != "scan" || rec.Environment != "preprod" || rec.Scale != "" || rec.ServerType != "ccx33" || rec.Version != "v0.49.0" || rec.Config.Customers != 1234 || rec.Config.Search != "Smith" {
		t.Errorf("run = %+v", rec)
	}

	// Five routes driven at three rungs, Samples each.
	for _, suffix := range []string{"/about", "/about.json", "/customers/search", "/settings", "/v1/screen/home"} {
		if n := h.prob.count(suffix); n != 60 {
			t.Errorf("%s fetched %d times; want 20 at each of 1, 4 and 16", suffix, n)
		}
	}
	// As the operator for the staff and open routes, as a customer for
	// the customer route, logged in once on the app password.
	if tok := h.prob.tokens["http://preprod/about"]; tok != h.ops.token {
		t.Errorf("/about fetched with %q; want the operator token", tok)
	}
	if tok := h.prob.tokens["http://preprod/v1/screen/home"]; tok != "tok-cust-001" {
		t.Errorf("/v1/screen/home fetched with %q; want the customer's", tok)
	}
	if len(h.prob.logins) != 1 || h.prob.logins[0] != "cust-001:"+h.ops.appPassword {
		t.Errorf("logins = %v; want one as cust-001 on the app password", h.prob.logins)
	}

	var scanned, skipped []string
	for _, p := range rec.Probes {
		if p.Skipped != "" {
			skipped = append(skipped, p.Method+" "+p.Path+": "+p.Skipped)
		} else if p.Concurrency == 1 {
			scanned = append(scanned, p.Method+" "+p.Path)
		}
	}
	if len(rec.Probes) != 15+3 || strings.Join(scanned, ", ") != "GET /about, GET /about.json, POST /customers/search, GET /settings, GET /v1/screen/home" {
		t.Errorf("scanned = %v (%d probes)", scanned, len(rec.Probes))
	}
	if strings.Join(skipped, "; ") != "GET /movements/{id}: needs a path parameter; POST /settings: a post that is not the customer search; POST /v1/login: a post that is not the customer search" {
		t.Errorf("skipped = %v", skipped)
	}

	runs, _ := h.runs.ListRuns(context.Background(), 1)
	if len(runs) != 1 || runs[0].WorkflowType != WorkflowScan || runs[0].Key != "preprod 2026-09-27" || runs[0].Status != wf.StatusCompleted {
		t.Fatalf("run = %+v", runs)
	}
	if stored, _ := h.store.Get(context.Background(), rec.RunID); stored == nil || len(stored.Probes) != 18 {
		t.Errorf("stored = %+v", stored)
	}
	if page := scenario.Page(*rec); !strings.Contains(page, "# 2026-09-27 scan at preprod") || !strings.Contains(page, "Not scanned: `GET /movements/{id}` (needs a path parameter)") {
		t.Errorf("page:\n%s", page)
	}
}

// Without a serving demo there is nothing to scan.
func TestScanNeedsAServingDemo(t *testing.T) {
	h := newScenarioHarness(t)
	if _, err := h.flow.Scan(context.Background(), deploy.Environment{Name: "nowhere"}, "", &h.out); err == nil || !strings.Contains(err.Error(), "not serving") {
		t.Fatalf("err = %v, want not serving", err)
	}
}
