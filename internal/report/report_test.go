package report

import (
	"strings"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func TestCurlRedactsAndQuotes(t *testing.T) {
	o := Options{Target: "http://localhost:8080/", Headers: map[string]string{"Authorization": "Bearer s3cret", "X-Tenant": "a"},
		AuthHeaders: map[string]bool{"Authorization": true}}
	sc := model.Scenario{Request: model.Request{Method: "POST", Path: "/users", Query: "q=1",
		Headers: map[string]string{"Content-Type": "application/json"}, Body: `{"name":"O'Brien"}`}}
	got := Curl(o, sc)
	if strings.Contains(got, "s3cret") {
		t.Fatalf("credentials leaked: %s", got)
	}
	for _, want := range []string{`-X POST`, `'http://localhost:8080/users?q=1'`, `'Authorization: <redacted>'`, `'X-Tenant: a'`, `--data-raw '{"name":"O'\''Brien"}'`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	sc.StripAuth = true
	if strings.Contains(Curl(o, sc), "Authorization") {
		t.Error("a no-credentials scenario should not show the Authorization header")
	}
}

func TestBuildGroupsFindings(t *testing.T) {
	f := model.Finding{Check: "server-error", Severity: model.SevError, Message: "500 on wrong-type input"}
	results := []model.Result{
		{Scenario: model.Scenario{Endpoint: "POST /a", Name: "one"}, Observed: []model.Observed{{Status: 500}}, Findings: []model.Finding{f}},
		{Scenario: model.Scenario{Endpoint: "POST /a", Name: "two"}, Observed: []model.Observed{{Status: 500}}, Findings: []model.Finding{f}},
		{Scenario: model.Scenario{Endpoint: "GET /b", Name: "three"}, Observed: []model.Observed{{Status: 200}}},
	}
	rep := Build(results, nil, Options{Target: "http://localhost"})
	if len(rep.Findings) != 1 || rep.Findings[0].Count != 2 || rep.Findings[0].Example != "one" {
		t.Fatalf("findings = %+v", rep.Findings)
	}
	if rep.Summary.Errors != 1 || rep.Summary.Endpoints != 2 || !rep.Failed("error") || rep.Failed("never") {
		t.Fatalf("summary = %+v", rep.Summary)
	}
}
