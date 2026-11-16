package check

import (
	"strings"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func checks(fs []model.Finding) map[string]model.Severity {
	out := map[string]model.Severity{}
	for _, f := range fs {
		out[f.Check] = f.Severity
	}
	return out
}

var userSchema = &model.Schema{Type: "object", Required: []string{"id", "name"}, Properties: map[string]*model.Schema{
	"id":   {Type: "integer"},
	"name": {Type: "string"},
	"role": {Type: "string", Enum: []any{"admin", "member"}},
	"tags": {Type: "array", Items: &model.Schema{Type: "string"}},
}}

func TestValidate(t *testing.T) {
	ok := map[string]any{"id": 1.0, "name": "a", "role": "admin", "tags": []any{"x"}}
	if errs := Validate(userSchema, ok); len(errs) != 0 {
		t.Fatalf("valid value rejected: %v", errs)
	}
	bad := map[string]any{"id": 1.5, "role": "owner", "tags": []any{3.0}}
	errs := strings.Join(Validate(userSchema, bad), "\n")
	for _, want := range []string{"$.name is missing", "$.id is a number, expected an integer", "$.role is not one of", "$.tags[0] is a number, expected a string"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
}

func ep() *model.Endpoint {
	return &model.Endpoint{Method: "POST", Path: "/users", Responses: map[string]*model.ResponseSpec{
		"201": {ContentType: "application/json", Schema: userSchema},
		"400": {},
	}}
}

func obs(status int, body string) []model.Observed {
	return []model.Observed{{Status: status, Body: body, Headers: map[string]string{"Content-Type": "application/json"}}}
}

func TestServerErrorAndLeak(t *testing.T) {
	sc := model.Scenario{Category: model.CatWrongType, Expect: model.ExpectReject}
	got := checks(Evaluate(sc, ep(), obs(500, "panic: boom\n\ngoroutine 7 [running]:\nmain.main()"), nil, Options{}))
	if got["server-error"] != model.SevError || got["error-leak"] != model.SevWarning {
		t.Fatalf("got %v", got)
	}
}

func TestDroppedConnection(t *testing.T) {
	sc := model.Scenario{Category: model.CatQuery, Expect: model.ExpectNoCrash}
	got := checks(Evaluate(sc, ep(), []model.Observed{{Error: "EOF"}}, nil, Options{}))
	if got["connection"] != model.SevError {
		t.Fatalf("got %v", got)
	}
	got = checks(Evaluate(sc, ep(), []model.Observed{{Error: "Client.Timeout exceeded", Timeout: true}}, nil, Options{}))
	if got["timeout"] != model.SevError {
		t.Fatalf("got %v", got)
	}
}

func TestAcceptedInvalid(t *testing.T) {
	strict := model.Scenario{Category: model.CatMissing, Expect: model.ExpectReject, Strict: true}
	good := `{"id": 1, "name": "a"}`
	if got := checks(Evaluate(strict, ep(), obs(201, good), nil, Options{})); got["accepted-invalid"] != model.SevError {
		t.Errorf("strict: got %v", got)
	}
	loose := model.Scenario{Category: model.CatBoundary, Expect: model.ExpectReject}
	if got := checks(Evaluate(loose, ep(), obs(201, good), nil, Options{})); got["accepted-invalid"] != model.SevWarning {
		t.Errorf("loose: got %v", got)
	}
	if got := Evaluate(loose, ep(), obs(400, `{}`), nil, Options{}); len(got) != 0 {
		t.Errorf("a 400 for invalid input is correct, got %v", got)
	}
}

func TestAuthBypass(t *testing.T) {
	sc := model.Scenario{Category: model.CatAuth, Expect: model.ExpectReject, Strict: true}
	if got := checks(Evaluate(sc, ep(), obs(201, `{"id":1,"name":"a"}`), nil, Options{})); got["auth-bypass"] != model.SevError {
		t.Fatalf("got %v", got)
	}
}

func TestContract(t *testing.T) {
	sc := model.Scenario{Category: model.CatHappy, Expect: model.ExpectSuccess}
	if got := checks(Evaluate(sc, ep(), obs(201, `{"id":"1","name":"a"}`), nil, Options{})); got["schema-mismatch"] != model.SevError {
		t.Errorf("schema mismatch not found: %v", got)
	}
	if got := checks(Evaluate(sc, ep(), obs(202, `{}`), nil, Options{})); got["undocumented-status"] != model.SevWarning {
		t.Errorf("undocumented status not found: %v", got)
	}
	html := []model.Observed{{Status: 201, Body: `{"id":1,"name":"a"}`, Headers: map[string]string{"Content-Type": "text/html"}}}
	if got := checks(Evaluate(sc, ep(), html, nil, Options{})); got["content-type"] != model.SevWarning {
		t.Errorf("content-type mismatch not found: %v", got)
	}
	if got := checks(Evaluate(sc, ep(), obs(404, `{}`), nil, Options{})); got["rejected-valid"] != model.SevInfo {
		t.Errorf("404 on a valid request should be a note: %v", got)
	}
}

func TestConsistencyAndBaseline(t *testing.T) {
	sc := model.Scenario{Category: model.CatConsistency, Expect: model.ExpectNoCrash}
	two := []model.Observed{{Status: 200, Body: `{"a":1}`}, {Status: 200, Body: `{"a":"x"}`}}
	if got := checks(Evaluate(sc, nil, two, nil, Options{})); got["inconsistent"] != model.SevWarning {
		t.Errorf("got %v", got)
	}
	base := model.Observed{Status: 200, Body: `{"a":1}`}
	if got := checks(Evaluate(sc, nil, obs(400, `{}`), &base, Options{})); got["divergence"] != model.SevError {
		t.Errorf("status divergence: got %v", got)
	}
	if got := checks(Evaluate(sc, nil, obs(200, `{"a":1,"b":2}`), &base, Options{})); got["divergence"] != model.SevWarning {
		t.Errorf("shape divergence: got %v", got)
	}
}

func TestReplayRegression(t *testing.T) {
	sc := model.Scenario{Category: model.CatReplay, Expect: model.ExpectRecorded,
		Recorded: &model.RecordedResponse{Status: 200, Body: `{"id":1,"name":"a","at":"t1"}`}}
	if got := checks(Evaluate(sc, nil, obs(500, "oops"), nil, Options{})); got["regression"] != model.SevError || got["server-error"] != "" {
		t.Errorf("status change: got %v", got)
	}
	if got := checks(Evaluate(sc, nil, obs(200, `{"id":1,"name":"a"}`), nil, Options{})); got["regression"] != model.SevWarning {
		t.Errorf("removed field: got %v", got)
	}
	same := obs(200, `{"id":2,"name":"b","at":"t2"}`)
	if got := Evaluate(sc, nil, same, nil, Options{}); len(got) != 0 {
		t.Errorf("same shape should pass without --exact: %v", got)
	}
	exact := Options{Exact: true, Ignore: map[string]bool{"id": true, "at": true}}
	if got := checks(Evaluate(sc, nil, same, nil, exact)); got["regression"] != model.SevWarning {
		t.Errorf("--exact should catch the changed name: %v", got)
	}
}
