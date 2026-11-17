package discover

import (
	"strings"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func TestAPIMapOtherProtocols(t *testing.T) {
	api, err := ParseSpec([]byte(`
endpoints:
  - grpc: users.v1.Users/GetUser
    handler: users.go:40
    body: {example: {id: 7}, required: [id]}
  - grpc: users.v1.Users/Watch
    stream: server
  - graphql: query user
    path: /api/graphql
    args: {id: ID!}
    rules: [a deleted user is null]
  - graphql: mutation rename
    body: {example: {id: 1, name: x}}
  - websocket: /ws/chat
    body: {example: {type: join}}
`), "api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	get := api.Endpoint("GRPC /users.v1.Users/GetUser")
	if get == nil || get.Protocol != model.ProtoGRPC || get.Code != "users.go:40" || !get.Body.IsRequired("id") {
		t.Fatalf("gRPC entry: %+v", get)
	}
	if w := api.Endpoint("GRPC /users.v1.Users/Watch"); w == nil || w.Stream != "server" {
		t.Errorf("stream entry: %+v", w)
	}
	user := api.Endpoint("QUERY user")
	if user == nil || user.Path != "/api/graphql" || user.GraphQL.ArgTypes["id"] != "ID!" || !user.Body.IsRequired("id") || len(user.Rules) != 1 {
		t.Fatalf("GraphQL entry: %+v", user)
	}
	rename := api.Endpoint("MUTATION rename")
	if rename == nil || rename.Path != "/api/graphql" || rename.GraphQL.ArgTypes["id"] != "Int" || rename.GraphQL.ArgTypes["name"] != "String" {
		t.Errorf("arg types inferred from the example, path from the other entry: %+v %+v", rename, rename.GraphQL)
	}
	ws := api.Endpoint("WS /ws/chat")
	if ws == nil || ws.Protocol != model.ProtoWebSocket {
		t.Errorf("WebSocket entry: %+v (%v)", ws, keys(api))
	}
}

func TestAPIMapProtocolErrors(t *testing.T) {
	for in, want := range map[string]string{
		"endpoints: [{grpc: a.B/C, method: GET, path: /x}]":    "exactly one of",
		"endpoints: [{grpc: a.B/C, path: /x}]":                 "leave out path",
		"endpoints: [{grpc: a.B/C, stream: sideways}]":         "client, server or bidi",
		"endpoints: [{graphql: fetch user}]":                   `"query <field>"`,
		"endpoints: [{websocket: ws/chat}]":                    "starting with /",
		"endpoints: [{method: GET, path: /x, args: {a: Int}}]": "args are for graphql",
		"endpoints: [{method: GET, path: /x, stream: server}]": "stream is for grpc",
		"endpoints: [{method: GET, path: /x, rules: [a: b]}]":  "quote a value",
	} {
		if _, err := ParseSpec([]byte(in), "api.yaml"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", in, err, want)
		}
	}
}

func TestMapMergesIntoProtoEndpoint(t *testing.T) {
	fromProto := &model.API{Endpoints: []*model.Endpoint{{
		Protocol: model.ProtoGRPC, Method: "GRPC", Path: "/users.v1.Users/GetUser", Source: "users.proto:9",
		Body: &model.Schema{Type: "object", Properties: map[string]*model.Schema{"id": {Type: "integer", Format: "int64"}, "name": {Type: "string"}}},
	}}}
	m, err := ParseSpec([]byte(`
endpoints:
  - grpc: /users.v1.Users/GetUser
    handler: users.go:40
    body: {required: [id], fields: {name: {max_length: 10}}}
`), "api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	mergeMap(fromProto, m)
	ep := fromProto.Endpoint("GRPC /users.v1.Users/GetUser")
	if len(fromProto.Endpoints) != 1 || ep.Code != "users.go:40" || ep.Source != "users.proto:9" {
		t.Fatalf("merged: %+v", fromProto.Endpoints)
	}
	if !ep.Body.IsRequired("id") || ep.Body.Properties["id"].Format != "int64" || *ep.Body.Properties["name"].MaxLength != 10 {
		t.Errorf("schema: %+v", ep.Body)
	}
}

func keys(api *model.API) []string {
	var out []string
	for _, ep := range api.Endpoints {
		out = append(out, ep.Key())
	}
	return out
}
