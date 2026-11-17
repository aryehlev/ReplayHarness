package protocols

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator"
)

//go:embed schema.graphql
var schemaSDL string

// AdminToken is the token deleteProduct is supposed to require.
const AdminToken = "demo-admin-token"

type catalog struct {
	schema   *ast.Schema
	mu       sync.Mutex
	products []map[string]any
	reviews  int
}

func newCatalog() *catalog {
	c := &catalog{schema: gqlparser.MustLoadSchema(&ast.Source{Name: "schema.graphql", Input: schemaSDL})}
	for i, p := range []struct{ name, cat string }{{"Notebook", "paper"}, {"Fountain pen", "pens"}, {"Ink", "pens"}} {
		c.products = append(c.products, map[string]any{"id": i + 1, "name": p.name, "category": p.cat, "priceCents": 500 * (i + 1), "reviews": []any{}})
	}
	return c
}

// ServeHTTP is a small GraphQL server: it parses and validates with
// gqlparser and runs the resolvers below. It answers POST with a JSON body,
// and GET with ?query=.
func (c *catalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	switch r.Method {
	case http.MethodPost:
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, gqlErrors("invalid JSON body: "+err.Error()))
			return
		}
	case http.MethodGet:
		// BUG: GET runs mutations too, so a link or an <img> can change data.
		req.Query, req.OperationName = r.URL.Query().Get("query"), r.URL.Query().Get("operationName")
		if v := r.URL.Query().Get("variables"); v != "" {
			_ = json.Unmarshal([]byte(v), &req.Variables)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, c.execute(r, req.Query, req.Variables, req.OperationName))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func gqlErrors(msgs ...string) map[string]any {
	var list []any
	for _, m := range msgs {
		list = append(list, map[string]any{"message": m})
	}
	return map[string]any{"errors": list}
}

func (c *catalog) execute(r *http.Request, query string, variables map[string]any, opName string) (resp map[string]any) {
	if strings.TrimSpace(query) == "" {
		return gqlErrors("no query")
	}
	doc, errs := gqlparser.LoadQuery(c.schema, query)
	if len(errs) > 0 {
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, e.Message)
		}
		return gqlErrors(msgs...)
	}
	op := doc.Operations.ForName(opName)
	if op == nil && opName == "" && len(doc.Operations) == 1 {
		op = doc.Operations[0]
	}
	if op == nil {
		return gqlErrors("unknown operation " + opName)
	}
	vars, err := validator.VariableValues(c.schema, op, variables)
	if err != nil {
		return gqlErrors(err.Error())
	}
	root := "Query"
	if op.Operation == ast.Mutation {
		root = "Mutation"
	}
	defer func() {
		// Like gqlgen, a panicking resolver becomes "internal system error"
		// with a 200 status.
		if p := recover(); p != nil {
			resp = map[string]any{"data": nil, "errors": []any{map[string]any{"message": "internal system error", "path": []any{}}}}
		}
	}()
	e := &executor{c: c, r: r, vars: vars, frags: doc.Fragments}
	return map[string]any{"data": e.selectionSet(op.SelectionSet, nil, root)}
}

type executor struct {
	c     *catalog
	r     *http.Request
	vars  map[string]any
	frags ast.FragmentDefinitionList
}

func (e *executor) selectionSet(set ast.SelectionSet, parent map[string]any, typ string) map[string]any {
	out := map[string]any{}
	for _, sel := range set {
		switch f := sel.(type) {
		case *ast.Field:
			if f.Name == "__typename" {
				out[f.Alias] = typ
				continue
			}
			var v any
			if parent == nil {
				v = e.root(typ, f.Name, f.ArgumentMap(e.vars))
			} else {
				v = parent[f.Name]
			}
			out[f.Alias] = e.complete(v, f.SelectionSet, f.Definition.Type.Name())
		case *ast.InlineFragment:
			for k, v := range e.selectionSet(f.SelectionSet, parent, typ) {
				out[k] = v
			}
		case *ast.FragmentSpread:
			if fd := e.frags.ForName(f.Name); fd != nil {
				for k, v := range e.selectionSet(fd.SelectionSet, parent, typ) {
					out[k] = v
				}
			}
		}
	}
	return out
}

func (e *executor) complete(v any, set ast.SelectionSet, typ string) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = e.complete(item, set, typ)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = e.complete(item, set, typ)
		}
		return out
	case map[string]any:
		return e.selectionSet(set, t, typ)
	}
	return v
}

func (e *executor) root(typ, field string, args map[string]any) any {
	c := e.c
	c.mu.Lock()
	defer c.mu.Unlock()
	switch typ + "." + field {
	case "Query.product":
		id := toInt(args["id"])
		if id > len(c.products) {
			return nil
		}
		// BUG: id 0 or below indexes before the start of the slice and panics.
		return c.products[id-1]
	case "Query.products":
		limit := 10
		if v, ok := args["limit"]; ok && v != nil {
			limit = toInt(v)
		}
		var out []map[string]any
		for _, p := range c.products {
			if cat, _ := args["category"].(string); cat == "" || p["category"] == cat {
				out = append(out, p)
			}
		}
		if limit < len(out) {
			out = out[:limit] // BUG: a negative limit panics here.
		}
		return out
	case "Mutation.addReview":
		id := toInt(args["productId"])
		if id < 1 || id > len(c.products) {
			panic(fmt.Sprintf("no product %d", id))
		}
		// BUG: the schema says rating is 1 to 5, but it is never checked.
		c.reviews++
		rev := map[string]any{"id": c.reviews, "rating": toInt(args["rating"]), "text": args["text"]}
		p := c.products[id-1]
		p["reviews"] = append(p["reviews"].([]any), rev)
		return rev
	case "Mutation.deleteProduct":
		// BUG: the admin token is never checked.
		id := toInt(args["id"])
		for i, p := range c.products {
			if p["id"] == id {
				c.products = append(c.products[:i], c.products[i+1:]...)
				return true
			}
		}
		return false
	}
	return nil
}

func toInt(v any) int {
	switch t := v.(type) {
	case int64:
		return int(t)
	case int:
		return t
	case float64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}
