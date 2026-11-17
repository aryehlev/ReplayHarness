package replay

import (
	"net/url"
	"strings"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func TestProtocolStepErrors(t *testing.T) {
	for in, want := range map[string]string{
		"{grpc: a.B/C, graphql: '{ x }'}":                  "one of grpc, graphql or websocket",
		"{grpc: a.B/C, path: /x}":                          "a gRPC step takes",
		"{grpc: a.B/C, message: {}, messages: [{}]}":       "message or messages, not both",
		"{method: GET, path: /x, message: {a: 1}}":         "message and messages are for gRPC",
		"{method: GET, path: /x, expect: {code: OK}}":      "expect.code is for gRPC",
		"{graphql: '{ x }', json: {a: 1}}":                 "leave out json and body",
		"{graphql: '{ x }', method: PUT}":                  "POST (the default) or GET",
		"{websocket: ws/chat}":                             "starting with /",
		"{websocket: /ws, method: GET}":                    "a WebSocket step takes",
		"{websocket: /ws, receive: -1}":                    "cannot be negative",
		"{method: GET, path: /x, send: [hi]}":              "are for WebSocket steps",
		"{grpc: a.B/C, expect: {code: TEAPOT}}":            "expect.code",
		"{method: GET, path: /x, expect: {close: 1000}}":   "expect.close is for WebSocket",
		"{graphql: '{ x }', variables: {a: 1}, query: {}}": "",
	} {
		_, err := Parse([]byte("name: a\nsteps: [" + in + "]"))
		if want == "" {
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", in, err, want)
		}
	}
	good := `
name: ok
steps:
  - {grpc: users.v1.Users/GetUser, message: {id: 1}, expect: {code: NOT_FOUND}}
  - {grpc: /users.v1.Users/Upload, messages: [{chunk: a}, {chunk: b}]}
  - {graphql: "{ me { id } }", variables: {x: 1}, expect: {status: 200}}
  - {method: GET, graphql: "{ me { id } }"}
  - {websocket: /ws, send: [{type: ping}, raw text], receive: 2, wait_ms: 300, expect: {close: 1000}}
`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
}

func TestBuildProtocolRequests(t *testing.T) {
	rs, err := Parse([]byte(`
name: build
steps:
  - {grpc: users.v1.Users.GetUser, message: {id: "{{id}}"}}
  - {grpc: users.v1.Users/Upload, messages: [{n: 1}, {n: 2}]}
  - {graphql: "query ($id: ID!) { user(id: $id) { name } }", variables: {id: "{{id}}"}, operation_name: Q}
  - {method: GET, graphql: "{ user(id: {{id}}) { name } }"}
  - {websocket: "/rooms/{{id}}", query: {v: 2}, send: [{hello: "{{id}}"}], receive: 1}
`))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"id": 42}
	build := func(i int) model.Request {
		req, err := BuildRequest(rs[0].Steps[i], vars)
		if err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		return req
	}
	if r := build(0); r.Protocol != model.ProtoGRPC || r.Path != "/users.v1.Users/GetUser" || r.Body != `{"id":42}` {
		t.Errorf("gRPC: %+v", r)
	}
	if r := build(1); r.Stream != "client" || r.Body != `[{"n":1},{"n":2}]` {
		t.Errorf("client stream: %+v", r)
	}
	if r := build(2); r.Method != "POST" || r.Path != "/graphql" || r.Headers["Content-Type"] != "application/json" ||
		!strings.Contains(r.Body, `"variables":{"id":42}`) || !strings.Contains(r.Body, `"operationName":"Q"`) {
		t.Errorf("GraphQL POST: %+v", r)
	}
	r := build(3)
	q, _ := url.ParseQuery(r.Query)
	if r.Method != "GET" || r.Body != "" || q.Get("query") != "{ user(id: 42) { name } }" {
		t.Errorf("GraphQL GET: %+v", r)
	}
	if r := build(4); r.Protocol != model.ProtoWebSocket || r.Path != "/rooms/42" || r.Query != "v=2" || r.Body != `[{"hello":42}]` || r.Receive != 1 {
		t.Errorf("WebSocket: %+v", r)
	}
}
