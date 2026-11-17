package generate

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/gql"
	"github.com/aryehlev/ReplayHarness/internal/model"
)

// grpcShape probes the whole message: an empty one.
func (g *gen) grpcShape() {
	hasRequired := g.ep.Body != nil && len(g.ep.Body.Required) > 0
	expect := model.ExpectNoCrash
	if hasRequired {
		expect = model.ExpectReject
	}
	g.add(variant{category: model.CatBodyShape, name: "empty message {}", expect: expect, strict: hasRequired && !g.ep.Inferred,
		setBody: true, body: map[string]any{}})
}

// graphqlShape probes the request around the variables: bodies that are not
// GraphQL requests, queries that do not parse or name unknown fields, a
// batch, and a mutation sent with GET.
func (g *gen) graphqlShape() {
	op := g.ep.GraphQL
	raw := func(cat, name, body string, expect model.Expect, headers map[string]string) {
		b := body
		g.add(variant{category: cat, name: name, expect: expect, rawBody: &b, headers: headers})
	}
	vars := g.base.body
	query := gql.Query(op)
	envelope := gql.Envelope(query, vars, "")
	raw(model.CatBodyShape, "{} (no query)", "{}", model.ExpectReject, nil)
	raw(model.CatBodyShape, "query is a number", `{"query": 42}`, model.ExpectReject, nil)
	raw(model.CatBodyShape, "malformed JSON (truncated)", `{"query": "{`, model.ExpectReject, nil)
	raw(model.CatBodyShape, "query that does not parse", gql.Envelope(op.Operation+" { "+op.Field+" ", vars, ""), model.ExpectReject, nil)
	raw(model.CatBodyShape, "unknown field", gql.Envelope(op.Operation+" { replayharnessUnknownField }", nil, ""), model.ExpectReject, nil)
	raw(model.CatBodyShape, "unknown operationName", gql.Envelope(query, vars, "ReplayHarnessMissing"), model.ExpectReject, nil)
	raw(model.CatBodyShape, "{ __typename }", gql.Envelope("{ __typename }", nil, ""), model.ExpectSuccess, nil)
	raw(model.CatBodyShape, "batch of two operations", "["+envelope+","+envelope+"]", model.ExpectNoCrash, nil)
	raw(model.CatBodyShape, "query sent as text/plain", envelope, model.ExpectNoCrash, map[string]string{"Content-Type": "text/plain"})
	if op.Operation == "mutation" {
		b, _ := json.Marshal(vars)
		q := url.Values{"query": {query}, "variables": {string(b)}}.Encode()
		g.add(variant{category: model.CatMethod, name: "mutation sent with GET (must not run)", expect: model.ExpectReject, method: "GET", noBody: true, rawQuery: &q})
	}
}

// wsShape probes the messages: malformed JSON, an empty message, a message of
// a kind the server does not know, and a large one.
func (g *gen) wsShape() {
	raw := func(name, msg string) {
		m := msg
		g.add(variant{category: model.CatBodyShape, name: name, expect: model.ExpectNoCrash, rawBody: &m})
	}
	raw("malformed JSON message", `{"type": "unterminated`)
	raw("empty message", "")
	raw("message that is not JSON", "hello")
	raw(`message of an unknown kind`, `{"type":"replayharness-unknown","replayharness":true}`)
	raw(fmt.Sprintf("%s message", byteSize(g.opts.LargeBody)), strings.Repeat("a", g.opts.LargeBody))
}
