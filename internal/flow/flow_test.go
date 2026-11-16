package flow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/runner"
)

func TestParseShapes(t *testing.T) {
	one := "name: a\nsteps:\n  - {method: get, path: /x}\n"
	list := "- name: a\n  steps: [{method: GET, path: /x}]\n- name: b\n  steps: [{method: GET, path: /y}]\n"
	wrapped := `{"flows": [{"name": "a", "steps": [{"method": "GET", "path": "/x"}]}]}`
	for in, want := range map[string]int{one: 1, list: 2, wrapped: 1} {
		flows, err := Parse([]byte(in))
		if err != nil || len(flows) != want {
			t.Fatalf("Parse(%q) = %d flows, %v", in, len(flows), err)
		}
		if flows[0].Steps[0].Method != "GET" {
			t.Errorf("method not upper-cased: %q", flows[0].Steps[0].Method)
		}
	}
}

func TestParseErrorsHelpTheAgent(t *testing.T) {
	cases := map[string]string{
		"name: a\nsteps: [{method: GET, path: http://evil.example/}]":         "path must start with /",
		"name: a\nsteps: [{method: GET, path: /x, expect: {status: teapot}}]": "expect.status",
		"name: a\nsteps: [{method: GET, path: /x, expct: {status: 200}}]":     "unknown field",
		"name: a\nsteps: [{method: GET, path: /x, save: {id: id}}]":           "JSON path",
		"steps: [{method: GET, path: /x}]":                                    "name is required",
	}
	for in, want := range cases {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %v, want it to mention %q", in, err, want)
		}
	}
}

func TestLookup(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"a": {"b": [10, {"c": "x"}]}, "odd key": 1, "items": [1, 2, 3], "length": 7}`), &doc)
	cases := map[string]any{
		"$.a.b[0]":       10.0,
		"$.a.b[1].c":     "x",
		`$["odd key"]`:   1.0,
		"$.items.length": 3.0,
		"$.length":       7.0, // a real key wins over .length
		"a.b[0]":         10.0,
	}
	for path, want := range cases {
		got, ok := lookup(doc, path)
		if !ok || got != want {
			t.Errorf("lookup(%s) = %v, %v; want %v", path, got, ok, want)
		}
	}
	for _, path := range []string{"$.a.zzz", "$.a.b[9]", "$.items[0].x"} {
		if _, ok := lookup(doc, path); ok {
			t.Errorf("lookup(%s) should not be found", path)
		}
	}
}

func TestAssertJSONKeepsTypes(t *testing.T) {
	body := map[string]any{"age": "36", "n": 2.0, "tags": []any{}}
	if msg := assertJSON(body, "$.age", 36); msg == "" {
		t.Error(`"36" must not equal 36`)
	}
	if msg := assertJSON(body, "$.n", 2); msg != "" {
		t.Errorf("YAML 2 should equal JSON 2.0: %s", msg)
	}
	if msg := assertJSON(body, "$.tags", "<nonempty>"); msg == "" {
		t.Error("an empty array is not <nonempty>")
	}
	if msg := assertJSON(body, "$.missing", "<absent>"); msg != "" {
		t.Error(msg)
	}
}

func TestStatusMatcher(t *testing.T) {
	m, _ := statusMatcher([]any{201, "4xx"})
	for code, want := range map[int]bool{201: true, 200: false, 404: true, 500: false} {
		if m(code) != want {
			t.Errorf("status %d: got %v", code, !want)
		}
	}
}

// A tiny stateful API: POST /notes creates, GET /notes/{id} reads.
func notesServer() *httptest.Server {
	var mu sync.Mutex
	notes := map[string]map[string]any{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/notes":
			var n map[string]any
			_ = json.NewDecoder(r.Body).Decode(&n)
			id := len(notes) + 1
			n["id"] = id
			notes[strings.TrimSpace(strings.Repeat(" ", 0))+itoa(id)] = n
			w.Header().Set("Location", "/notes/"+itoa(id))
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(n)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/notes/"):
			n, ok := notes[strings.TrimPrefix(r.URL.Path, "/notes/")]
			if !ok {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(n)
		case r.URL.Path == "/boom":
			w.WriteHeader(500)
			_, _ = w.Write([]byte("Traceback (most recent call last):\n  File \"app.py\", line 3"))
		default:
			w.WriteHeader(404)
		}
	}))
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestRunSavesAndReusesValues(t *testing.T) {
	srv := notesServer()
	defer srv.Close()
	client, err := runner.NewClient(srv.URL, runner.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	flows, err := Parse([]byte(`
flows:
  - name: a note can be read back
    steps:
      - method: POST
        path: /notes
        json: {text: "hi {{unique}}", n: 3}
        expect: {status: 201, headers: {Location: /notes/}, json: {$.n: 3}}
        save: {id: $.id, loc: "header:Location"}
      - method: GET
        path: "{{loc}}"
        expect: {status: 200, json: {$.id: "{{id}}", $.n: 3}}
      - method: GET
        path: /notes/{{id}}?x={{id}}
        query: {y: "{{id}}"}
        expect: {status: 200}
  - name: an unknown variable stops the flow
    steps:
      - {method: GET, path: "/notes/{{nope}}"}
      - {method: GET, path: /never}
  - name: a crash fails a step without assertions
    steps:
      - {method: GET, path: /boom}
  - name: a step may expect a 5xx
    steps:
      - {method: GET, path: /boom, expect: {status: 500}}
`))
	if err != nil {
		t.Fatal(err)
	}
	results := (&Runner{Client: client}).Run(context.Background(), flows)
	if !results[0].Passed {
		var b strings.Builder
		WriteText(&b, results[:1], false)
		t.Fatalf("flow with saved values failed:\n%s", b.String())
	}
	if got := results[0].Steps[2].Path; got != "/notes/1?x=1&y=1" {
		t.Errorf("path with variables = %q", got)
	}
	if r := results[1]; r.Passed || len(r.Steps) != 1 || !strings.Contains(r.Steps[0].Failures[0], "unknown variable {{nope}}") {
		t.Errorf("unknown variable: %+v", r)
	}
	if r := results[2]; r.Passed || len(r.Steps[0].Findings) != 2 {
		t.Errorf("crash: want server-error and error-leak findings, got %+v", r.Steps[0].Findings)
	}
	if r := results[3]; !r.Passed {
		t.Errorf("an expected 5xx should pass: %+v", r.Steps[0])
	}
}
