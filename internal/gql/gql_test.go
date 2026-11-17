package gql

import (
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

const sdl = `
type Query {
  user(id: ID!): User
  users(limit: Int = 10, role: Role): [User!]!
}
type User { id: ID! name: String! role: Role friends(first: Int!): [User!]! address: Address }
type Address { city: String street: String }
enum Role { ADMIN MEMBER }
input NewUser { name: String! role: Role = MEMBER }
`

func TestParseSDLAndEndpoints(t *testing.T) {
	s, notes := ParseSDL([]*ast.Source{
		{Name: "schema.graphql", Input: sdl},
		{Name: "more.graphql", Input: "extend type Query { me: User }\ntype Mutation { createUser(input: NewUser!): User! }"},
		{Name: "ops.graphql", Input: "query Q { me { id } }"}, // an operation document: skipped quietly
	})
	if len(notes) != 0 {
		t.Errorf("notes: %v", notes)
	}
	if !s.HasRoots() {
		t.Fatal("no roots")
	}
	eps := s.Endpoints("/query")
	keys := map[string]bool{}
	for _, ep := range eps {
		keys[ep.Key()] = true
		if ep.Path != "/query" {
			t.Errorf("%s path %s", ep.Key(), ep.Path)
		}
	}
	for _, k := range []string{"QUERY user", "QUERY users", "QUERY me", "MUTATION createUser"} {
		if !keys[k] {
			t.Errorf("missing %s in %v", k, keys)
		}
	}
	user := eps[0]
	if !user.Body.IsRequired("id") || user.Source != "schema.graphql:3" {
		t.Errorf("user: required %v, source %s", user.Body.Required, user.Source)
	}
	// friends needs an argument, so the selection leaves it out; address is one level down.
	if got := user.GraphQL.Selection; got != "{ id name role address { city street } }" {
		t.Errorf("selection = %q", got)
	}
	if q := Query(user.GraphQL); q != "query ReplayHarness($id: ID!) { user(id: $id) { id name role address { city street } } }" {
		t.Errorf("query = %q", q)
	}
	users := eps[1]
	if users.Body.IsRequired("limit") || len(users.Body.Properties["role"].Enum) != 2 || !users.Body.Properties["role"].Nullable {
		t.Errorf("users variables: %+v", users.Body)
	}
	create := eps[3]
	in := create.Body.Properties["input"]
	if in.Type != "object" || !in.IsRequired("name") || in.IsRequired("role") {
		t.Errorf("input object: %+v", in)
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct {
		body           string
		ok, isInternal bool
		n              int
	}{
		{`{"data":{"user":null}}`, true, false, 0},
		{`{"data":null,"errors":[{"message":"user not found"}]}`, true, false, 1},
		{`{"data":null,"errors":[{"message":"internal system error"}]}`, true, true, 1},
		{`{"errors":[{"message":"boom","extensions":{"code":"INTERNAL_SERVER_ERROR"}}]}`, true, true, 1},
		{`{"errors":[{"message":"x","extensions":{"exception":{"stacktrace":["at f (a.js:1:2)"]}}}]}`, true, true, 1},
		{`{"errors":[{"message":"runtime error: index out of range [-1]"}]}`, true, true, 1},
		{`{"ok":true}`, false, false, 0},
		{`not json`, false, false, 0},
	} {
		msgs, internal, ok := Errors(c.body)
		if ok != c.ok || (len(internal) > 0) != c.isInternal || len(msgs) != c.n {
			t.Errorf("Errors(%s) = %v %v %v", c.body, msgs, internal, ok)
		}
	}
}

func TestFromIntrospection(t *testing.T) {
	body := `{"data":{"__schema":{"queryType":{"name":"Query"},"mutationType":null,"subscriptionType":null,"types":[
	  {"kind":"OBJECT","name":"Query","fields":[{"name":"user","args":[{"name":"id","defaultValue":null,"type":{"kind":"NON_NULL","name":null,"ofType":{"kind":"SCALAR","name":"ID","ofType":null}}}],
	    "type":{"kind":"OBJECT","name":"User","ofType":null}}]},
	  {"kind":"OBJECT","name":"User","fields":[{"name":"id","args":[],"type":{"kind":"NON_NULL","name":null,"ofType":{"kind":"SCALAR","name":"ID","ofType":null}}},
	    {"name":"tags","args":[],"type":{"kind":"LIST","name":null,"ofType":{"kind":"SCALAR","name":"String","ofType":null}}}]},
	  {"kind":"SCALAR","name":"ID"},{"kind":"SCALAR","name":"String"}]}}}`
	s, err := FromIntrospection([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	eps := s.Endpoints("/graphql")
	if len(eps) != 1 || eps[0].GraphQL.ArgTypes["id"] != "ID!" || eps[0].GraphQL.Selection != "{ id tags }" || eps[0].GraphQL.Returns != "User" {
		t.Fatalf("endpoints: %+v %+v", eps, eps[0].GraphQL)
	}
	if _, err := FromIntrospection([]byte(`{"errors":[{"message":"introspection is disabled"}]}`)); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled introspection: %v", err)
	}
}
