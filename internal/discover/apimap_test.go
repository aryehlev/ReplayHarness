package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIMap(t *testing.T) {
	api, err := ParseSpec([]byte(`
name: Orders
auth_headers: [X-Api-Key]
endpoints:
  - method: post
    path: /orders/:id/items
    handler: orders.go:57
    auth: true
    params:
      - {name: dry_run, in: query, example: true}
    body:
      example: {item_id: 7, qty: 2, address: {city: Paris, zip: "75001"}, lines: [{sku: a, n: 1}]}
      required: [item_id, address.city, "lines[].sku"]
      fields: {qty: {min: 1, max: 100}, note: {type: string, max_length: 20}}
    responses: {"201": created, "4xx": bad input}
    rules: [only the owner may add items]
`), "api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ep := api.Endpoint("POST /orders/{id}/items")
	if ep == nil {
		t.Fatalf("endpoints: %+v", api.Endpoints)
	}
	if ep.Code != "orders.go:57" || !ep.Secured || ep.Inferred || len(ep.Rules) != 1 || api.AuthHeaders[0] != "X-Api-Key" {
		t.Errorf("endpoint = %+v", ep)
	}
	if len(ep.ParamsIn("path")) != 1 || ep.ParamsIn("query")[0].Schema.Type != "boolean" {
		t.Errorf("params = %+v", ep.Params)
	}
	b := ep.Body
	if strings.Join(b.Required, ",") != "item_id" || strings.Join(b.Properties["address"].Required, ",") != "city" ||
		strings.Join(b.Properties["lines"].Items.Required, ",") != "sku" {
		t.Errorf("required: %v / %v", b.Required, b.Properties["address"].Required)
	}
	if q := b.Properties["qty"]; q.Type != "integer" || *q.Minimum != 1 || *q.Maximum != 100 {
		t.Errorf("qty = %+v", q)
	}
	if n := b.Properties["note"]; n == nil || n.Type != "string" || *n.MaxLength != 20 {
		t.Errorf("a field missing from the example should be added: %+v", n)
	}
	if ep.Responses["4XX"] == nil || ep.Responses["201"].Description != "created" {
		t.Errorf("responses = %+v", ep.Responses)
	}
}

func TestAPIMapErrorsHelpTheAgent(t *testing.T) {
	cases := map[string]string{
		"endpoints: [{method: GET, path: /x, handlr: a.go:1}]":                                    "unknown field",
		"endpoints: [{method: FETCH, path: /x}]":                                                  "method must be",
		"endpoints: [{method: GET, path: x}]":                                                     "path must start with /",
		"endpoints: [{method: GET, path: /x, params: [{name: id, in: path}]}]":                    "not in the path",
		"endpoints: [{method: POST, path: /x, body: {example: {a: 1}, required: [b.c]}}]":         "b is not in the example body",
		"endpoints: [{method: GET, path: /x}, {method: GET, path: /x}]":                           "listed twice",
		"endpoints: [{method: GET, path: /x, params: [{name: q, in: cookie}]}]":                   "path, query or header",
		"endpoints: [{method: POST, path: /x, body: {example: {a: 1}, fields: {a.b: {min: 1}}}}]": "not an object",
	}
	for in, want := range cases {
		if _, err := ParseSpec([]byte(in), "api.yaml"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", in, err, want)
		}
	}
}

func TestScanRepoUsesTheMap(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main
func routes() {
	mux.HandleFunc("GET /users/{id}", getUser)
	mux.HandleFunc("POST /users", createUser)
	mux.HandleFunc("GET /internal/debug", debug)
}`)
	writeFile(t, filepath.Join(dir, MapFile), `
name: Users
endpoints:
  - {method: GET, path: "/users/{id}", handler: "users.go:10", rules: [404 for unknown ids]}
  - {method: POST, path: /users, handler: "users.go:30", body: {example: {name: a}, required: [name]}}
  - {method: GET, path: /reports, handler: "reports.go:5"}
`)
	api, err := ScanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if api.Title != "Users" || len(api.Endpoints) != 4 {
		t.Fatalf("title %q, endpoints %d", api.Title, len(api.Endpoints))
	}
	if ep := api.Endpoint("GET /users/{id}"); ep.Code != "users.go:10" || ep.Inferred {
		t.Errorf("the map's handler location wins: %+v", ep)
	}
	// /internal/debug is in the code but not the map; /reports was located by
	// the agent even though no pattern found it, so it is not drift.
	if len(api.Drift) != 1 || api.Drift[0].Path != "/internal/debug" {
		t.Errorf("drift = %+v", api.Drift)
	}

	os.WriteFile(filepath.Join(dir, MapFile), []byte("endpoints: [{method: GET}]"), 0o644)
	if _, err := ScanRepo(dir); err == nil || !strings.Contains(err.Error(), "path must start with /") {
		t.Errorf("an invalid map must be reported, got %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
