package generate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func intp(i int) *int           { return &i }
func floatp(f float64) *float64 { return &f }

func usersAPI() *model.API {
	body := &model.Schema{Type: "object", Required: []string{"name", "email"}, Properties: map[string]*model.Schema{
		"name":  {Type: "string", MinLength: intp(1), MaxLength: intp(10)},
		"email": {Type: "string", Format: "email"},
		"age":   {Type: "integer", Minimum: floatp(0), Maximum: floatp(120)},
		"role":  {Type: "string", Enum: []any{"admin", "member"}},
		"address": {Type: "object", Required: []string{"city"}, Properties: map[string]*model.Schema{
			"city": {Type: "string"},
		}},
	}}
	return &model.API{Endpoints: []*model.Endpoint{
		{Method: "POST", Path: "/users", Body: body, BodyType: "application/json", BodyRequired: true,
			Responses: map[string]*model.ResponseSpec{"201": {}}},
		{Method: "GET", Path: "/users/{id}", Secured: true, Params: []model.Param{
			{Name: "id", In: "path", Required: true, Schema: &model.Schema{Type: "integer"}},
			{Name: "fields", In: "query", Schema: &model.Schema{Type: "string"}},
		}},
	}}
}

func find(t *testing.T, scs []model.Scenario, name string) model.Scenario {
	t.Helper()
	for _, s := range scs {
		if s.Name == name {
			return s
		}
	}
	var names []string
	for _, s := range scs {
		names = append(names, s.Name)
	}
	t.Fatalf("no scenario named %q; have:\n%s", name, strings.Join(names, "\n"))
	return model.Scenario{}
}

func body(t *testing.T, s model.Scenario) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s.Request.Body), &m); err != nil {
		t.Fatalf("%s: body %q is not a JSON object: %v", s.Name, s.Request.Body, err)
	}
	return m
}

func TestSuiteBodyProbes(t *testing.T) {
	scs := Suite(usersAPI(), DefaultOptions()).Scenarios

	valid := body(t, find(t, scs, "valid request"))
	if valid["email"] != "user@example.com" || valid["role"] != "admin" {
		t.Errorf("valid body = %v", valid)
	}

	missing := find(t, scs, "body.email omitted (required)")
	if _, ok := body(t, missing)["email"]; ok || missing.Expect != model.ExpectReject || !missing.Strict {
		t.Errorf("missing-required scenario is wrong: %+v", missing)
	}
	nested := find(t, scs, "body.address.city omitted (required)")
	if addr := body(t, nested)["address"].(map[string]any); addr["city"] != nil {
		t.Errorf("nested field was not removed: %v", addr)
	}

	over := find(t, scs, "body.name is 11 chars (above maxLength 10)")
	if n := len(body(t, over)["name"].(string)); n != 11 || over.Expect != model.ExpectReject {
		t.Errorf("above-max scenario: len %d expect %s", n, over.Expect)
	}
	if at := find(t, scs, "body.age = 120 (maximum)"); at.Expect != model.ExpectSuccess {
		t.Errorf("a value at the maximum should be expected to succeed")
	}
	if s := find(t, scs, "body.age = 121 (above maximum 120)"); body(t, s)["age"] != 121.0 {
		t.Errorf("above-max value = %v", body(t, s)["age"])
	}
	find(t, scs, `body.role = "not-a-valid-option" (not in enum)`)
	find(t, scs, `body.role = "ADMIN" (enum value in a different case)`)
	find(t, scs, `body.email = "not-an-email" (not a valid email)`)
	find(t, scs, `body.age = "abc" (string, expected integer)`)
	find(t, scs, "body.name = NUL and control characters")
}

func TestSuiteSkipsUnboundedProbesWhenBounded(t *testing.T) {
	for _, s := range Suite(usersAPI(), DefaultOptions()).Scenarios {
		if strings.HasPrefix(s.Name, "body.age = 9223372036854775807") {
			t.Fatalf("int64 probes should be skipped when the schema has a maximum: %q", s.Name)
		}
	}
}

func TestSuitePathQueryAuthAndMethods(t *testing.T) {
	scs := Suite(usersAPI(), DefaultOptions()).Scenarios
	if s := find(t, scs, `path id = "abc" (not a number)`); s.Request.Path != "/users/abc" || s.Expect != model.ExpectReject {
		t.Errorf("path probe = %+v", s.Request)
	}
	if s := find(t, scs, "path id = encoded slash"); s.Request.Path != "/users/a%2Fb" {
		t.Errorf("path values must be escaped, got %q", s.Request.Path)
	}
	if s := find(t, scs, "query fields given twice"); s.Request.Query != "fields=test&fields=test2" {
		t.Errorf("duplicate query = %q", s.Request.Query)
	}
	if s := find(t, scs, "no credentials"); !s.StripAuth || !s.Strict {
		t.Errorf("auth probe = %+v", s)
	}
	if s := find(t, scs, "DELETE (not defined for this path)"); s.Request.Method != "DELETE" || s.Request.Body != "" {
		t.Errorf("method probe = %+v", s.Request)
	}
	for _, s := range scs {
		if s.Category == model.CatMethod && s.Endpoint == "GET /users/{id}" && s.Request.Method == "GET" {
			t.Error("GET is defined for /users/{id} and should not be probed as undefined")
		}
	}
	find(t, scs, "same GET sent twice")
}

func TestSuiteIDsAreUniqueAndStable(t *testing.T) {
	a := Suite(usersAPI(), DefaultOptions()).Scenarios
	b := Suite(usersAPI(), DefaultOptions()).Scenarios
	if len(a) != len(b) {
		t.Fatalf("not deterministic: %d vs %d scenarios", len(a), len(b))
	}
	seen := map[string]bool{}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("scenario %d has unstable ID", i)
		}
		if seen[a[i].ID] {
			t.Fatalf("duplicate ID %s", a[i].ID)
		}
		seen[a[i].ID] = true
	}
}

func TestCategoryFilters(t *testing.T) {
	opts := DefaultOptions()
	opts.Only = map[string]bool{model.CatMissing: true}
	for _, s := range Suite(usersAPI(), opts).Scenarios {
		if s.Category != model.CatMissing {
			t.Fatalf("--only leaked category %s", s.Category)
		}
	}
}

func TestCapKeepsEveryCategory(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxPerEndpoint = 12
	scs := Suite(usersAPI(), opts).Scenarios
	cats := map[string]bool{}
	for _, s := range scs {
		if s.Endpoint == "POST /users" {
			cats[s.Category] = true
		}
	}
	for _, c := range []string{model.CatHappy, model.CatMissing, model.CatWrongType, model.CatBodyShape, model.CatMethod} {
		if !cats[c] {
			t.Errorf("cap dropped category %s entirely (have %v)", c, cats)
		}
	}
}

func TestInferredEndpointsOnlyFlagCrashes(t *testing.T) {
	api := &model.API{Endpoints: []*model.Endpoint{{Method: "POST", Path: "/things", BodyType: "application/json", Inferred: true}}}
	for _, s := range Suite(api, DefaultOptions()).Scenarios {
		if s.Expect == model.ExpectReject && s.Strict {
			t.Errorf("guessed rules must not be strict: %s", s.Name)
		}
		if s.Category == model.CatHappy && s.Expect != model.ExpectNoCrash {
			t.Errorf("the baseline of a route found in code should only have to not crash")
		}
	}
}

func TestFormBodies(t *testing.T) {
	api := &model.API{Endpoints: []*model.Endpoint{{Method: "POST", Path: "/login", BodyType: "application/x-www-form-urlencoded",
		Body: &model.Schema{Type: "object", Required: []string{"user"}, Properties: map[string]*model.Schema{"user": {Type: "string", MaxLength: intp(5)}}}}}}
	scs := Suite(api, DefaultOptions()).Scenarios
	if s := find(t, scs, "valid request"); s.Request.Body != "user=test" {
		t.Errorf("form body = %q", s.Request.Body)
	}
	if s := find(t, scs, "body.user omitted (required)"); s.Request.Body != "" {
		t.Errorf("form body without user = %q", s.Request.Body)
	}
}
