// Package e2e runs the whole pipeline against the deliberately buggy example
// API and checks that every planted bug is found and the clean endpoint stays
// clean.
package e2e

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryehlev/ReplayHarness/examples/buggyapi"
	"github.com/aryehlev/ReplayHarness/internal/check"
	"github.com/aryehlev/ReplayHarness/internal/discover"
	"github.com/aryehlev/ReplayHarness/internal/generate"
	"github.com/aryehlev/ReplayHarness/internal/model"
	"github.com/aryehlev/ReplayHarness/internal/replay"
	"github.com/aryehlev/ReplayHarness/internal/report"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

func TestBuggyAPI(t *testing.T) {
	srv := httptest.NewUnstartedServer(buggyapi.Handler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the demo panics on purpose
	srv.Start()
	defer srv.Close()

	api, err := discover.ScanRepo("../../examples/buggyapi")
	if err != nil {
		t.Fatal(err)
	}
	suite := generate.Suite(api, generate.DefaultOptions())
	results, err := runner.Run(context.Background(), runner.Config{
		Target: srv.URL, Concurrency: 8, Timeout: 5 * time.Second, Check: check.Options{Slow: 2 * time.Second},
	}, suite)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.Build(results, api, report.Options{Target: srv.URL})

	has := func(endpoint, chk, msgPart string) bool {
		for _, g := range rep.Findings {
			if g.Endpoint == endpoint && g.Check == chk && strings.Contains(g.Message, msgPart) {
				return true
			}
		}
		return false
	}
	want := []struct{ endpoint, check, msg, why string }{
		{"POST /users", "server-error", "wrong-type", "a non-numeric age panics"},
		{"POST /users", "server-error", "null", "a null age panics"},
		{"POST /users", "accepted-invalid", "missing a required field", "email is required but never checked"},
		{"POST /users", "accepted-invalid", "boundary", "name maxLength is not enforced"},
		{"POST /users", "accepted-invalid", "badly formatted", "email format is not checked"},
		{"POST /users", "error-leak", "Go stack trace", "panics are sent to the client"},
		{"GET /users/{id}", "server-error", "path parameter", "id 0 indexes out of range"},
		{"GET /users/{id}", "schema-mismatch", "$.age", "age is sent as a string"},
		{"DELETE /users/{id}", "auth-bypass", "", "the bearer token is never checked"},
		{"GET /search", "connection", "", "short queries panic and drop the connection"},
		{"GET /search", "accepted-invalid", "boundary", "limit is not range-checked"},
	}
	for _, w := range want {
		if !has(w.endpoint, w.check, w.msg) {
			t.Errorf("missed %s on %s (%s)", w.check, w.endpoint, w.why)
		}
	}

	for _, g := range rep.Findings {
		if g.Endpoint == "GET /health" && g.Severity != model.SevInfo {
			t.Errorf("false positive on the correct endpoint: %+v", g)
		}
	}
	drift := false
	for _, d := range rep.Drift {
		if d.Kind == "undocumented" && d.Path == "/admin/stats" {
			drift = true
		}
		if d.Path == "/openapi.yaml" {
			t.Errorf("the route serving the spec should not count as drift")
		}
	}
	if !drift {
		t.Errorf("missed the undocumented /admin/stats route; drift = %+v", rep.Drift)
	}
	if !rep.Failed("error") {
		t.Error("the run should fail")
	}
}

func TestServerSpecDiscovery(t *testing.T) {
	srv := httptest.NewServer(buggyapi.Handler())
	defer srv.Close()
	api, found, err := discover.FetchSpec(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(found, "/openapi.yaml") || len(api.Endpoints) != 5 {
		t.Fatalf("found %s with %d endpoints", found, len(api.Endpoints))
	}
}

// TestAgentFlows runs the replays an agent would write for the demo API: the
// ones that describe planted bugs must fail, the rest must pass.
func TestAgentFlows(t *testing.T) {
	srv := httptest.NewUnstartedServer(buggyapi.Handler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	defer srv.Close()

	replays, err := replay.Load("../../examples/buggyapi/replays.yaml")
	if err != nil {
		t.Fatal(err)
	}
	client, err := runner.NewClient(srv.URL, runner.Config{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	results := (&replay.Runner{Client: client}).Run(context.Background(), replays)
	pass := map[string]bool{
		"the service reports healthy": true,
		"search honors the limit":     true,
		"malformed JSON is rejected":  true,
	}
	for _, r := range results {
		if r.Passed != pass[r.Name] {
			var b strings.Builder
			replay.WriteText(&b, []replay.Result{r}, false)
			t.Errorf("replay %q: passed=%v, want %v\n%s", r.Name, r.Passed, pass[r.Name], b.String())
		}
	}
	if len(results) != 10 {
		t.Errorf("ran %d replays, want 10", len(results))
	}
}

// TestNoSpec checks a project without any OpenAPI spec: only the code and the
// API map the discovery agent wrote.
func TestNoSpec(t *testing.T) {
	dir := t.TempDir()
	code, err := os.ReadFile("../../examples/buggyapi/buggyapi.go")
	if err != nil {
		t.Fatal(err)
	}
	apiMap, err := os.ReadFile("../../examples/buggyapi/api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "buggyapi.go"), code, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".replayharness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, discover.MapFile), apiMap, 0o644); err != nil {
		t.Fatal(err)
	}
	api, err := discover.ScanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.Endpoints) != 6 || len(api.Drift) != 0 {
		t.Fatalf("endpoints %d, drift %+v", len(api.Endpoints), api.Drift)
	}

	srv := httptest.NewUnstartedServer(buggyapi.Handler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	defer srv.Close()
	cfg := runner.Config{Target: srv.URL, Concurrency: 8, Timeout: 5 * time.Second, Check: check.Options{Slow: 2 * time.Second}}
	results, err := runner.Run(context.Background(), cfg, generate.Suite(api, generate.DefaultOptions()))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range results {
		for _, f := range r.Findings {
			found[r.Scenario.Endpoint+" "+f.Check] = true
		}
	}
	for _, want := range []string{
		"POST /users server-error", "POST /users accepted-invalid", "GET /users/{id} server-error",
		"GET /search connection", "DELETE /users/{id} auth-bypass",
	} {
		if !found[want] {
			t.Errorf("missing %s", want)
		}
	}
	if found["GET /health server-error"] {
		t.Error("false positive on /health")
	}
}
