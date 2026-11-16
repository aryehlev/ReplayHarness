package discover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// MapFile is where the discovery agent writes the API map, relative to the
// project root. ScanRepo uses it when it exists.
const MapFile = ".replayharness/api.yaml"

// An API map describes a service the way a coding agent found it in the code.
// It is the fallback for services without an OpenAPI spec, and it adds what a
// spec leaves out: where each handler is and which rules it enforces.
//
//	name: Orders service
//	auth_headers: [Authorization]
//	notes: [Every /api route needs "Authorization: Bearer <token>" (middleware/auth.go:12)]
//	endpoints:
//	  - method: POST
//	    path: /api/orders/{id}/items
//	    handler: internal/http/orders.go:57
//	    auth: true
//	    params:
//	      - {name: id, in: path, example: 42}
//	      - {name: dry_run, in: query, type: boolean}
//	    body:
//	      example: {item_id: 7, qty: 2}
//	      required: [item_id, qty]
//	      fields: {qty: {min: 1, max: 100}}
//	    responses: {"201": the updated order, "400": qty out of range, "404": unknown order or item}
//	    rules:
//	      - only the order's owner may add items (orders.go:63)
type apiMap struct {
	Name        string        `json:"name"`
	AuthHeaders []string      `json:"auth_headers"`
	Notes       []string      `json:"notes"`
	Endpoints   []mapEndpoint `json:"endpoints"`
}

type mapEndpoint struct {
	Method    string         `json:"method"`
	Path      string         `json:"path"`
	Handler   string         `json:"handler"`
	Auth      bool           `json:"auth"`
	Params    []mapParam     `json:"params"`
	Body      *mapBody       `json:"body"`
	Responses map[string]any `json:"responses"`
	Rules     []string       `json:"rules"`
}

type mapField struct {
	Type      string   `json:"type"`
	Format    string   `json:"format"`
	Min       *float64 `json:"min"`
	Max       *float64 `json:"max"`
	MinLength *int     `json:"min_length"`
	MaxLength *int     `json:"max_length"`
	Pattern   string   `json:"pattern"`
	Enum      []any    `json:"enum"`
	Nullable  bool     `json:"nullable"`
}

type mapParam struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Example  any    `json:"example"`
	mapField
}

type mapBody struct {
	ContentType string              `json:"content_type"`
	Example     any                 `json:"example"`
	Required    []string            `json:"required"`
	Fields      map[string]mapField `json:"fields"`
}

func isAPIMap(doc map[string]any) bool {
	_, ok := doc["endpoints"].([]any)
	return ok
}

func parseAPIMap(doc map[string]any, origin string) (*model.API, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var m apiMap
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: invalid API map: %w", origin, err)
	}
	api := &model.API{Title: m.Name, Sources: []string{origin}, AuthHeaders: m.AuthHeaders, Notes: m.Notes}
	for i, me := range m.Endpoints {
		ep, err := me.endpoint(origin)
		if err != nil {
			return nil, fmt.Errorf("%s: endpoint %d (%s %s): %w", origin, i+1, me.Method, me.Path, err)
		}
		if api.Endpoint(ep.Key()) != nil {
			return nil, fmt.Errorf("%s: %s is listed twice", origin, ep.Key())
		}
		api.Endpoints = append(api.Endpoints, ep)
	}
	return api, nil
}

var mapMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}

func (me mapEndpoint) endpoint(origin string) (*model.Endpoint, error) {
	method := strings.ToUpper(strings.TrimSpace(me.Method))
	if !mapMethods[method] {
		return nil, fmt.Errorf("method must be one of GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
	}
	if !strings.HasPrefix(me.Path, "/") {
		return nil, fmt.Errorf("path must start with /")
	}
	path, _ := normalizePath(me.Path)
	ep := &model.Endpoint{Method: method, Path: path, Secured: me.Auth, Source: origin, Code: me.Handler, Rules: me.Rules}

	seen := map[string]bool{}
	for _, p := range me.Params {
		if p.Name == "" {
			return nil, fmt.Errorf("a param has no name")
		}
		switch p.In {
		case "path", "query", "header":
		case "":
			p.In = "query"
		default:
			return nil, fmt.Errorf("param %s: in must be path, query or header", p.Name)
		}
		s := p.mapField.schema()
		if s.Type == "" && p.Example != nil {
			inferred := InferSchema(p.Example)
			inferred.Format = ""
			p.mapField.apply(inferred)
			s = inferred
		}
		required := p.Required || p.In == "path"
		ep.Params = append(ep.Params, model.Param{Name: p.Name, In: p.In, Required: required, Schema: s, Example: p.Example})
		seen[p.In+" "+p.Name] = true
	}
	for _, name := range PathParamNames(path) {
		if !seen["path "+name] {
			ep.Params = append(ep.Params, model.Param{Name: name, In: "path", Required: true})
		}
	}
	for _, p := range ep.Params {
		if p.In == "path" && !strings.Contains(path, "{"+p.Name+"}") {
			return nil, fmt.Errorf("path param %s is not in the path", p.Name)
		}
	}

	if b := me.Body; b != nil {
		ep.BodyType = b.ContentType
		if ep.BodyType == "" {
			ep.BodyType = "application/json"
		}
		ep.BodyRequired = true
		s := &model.Schema{Type: "object", Properties: map[string]*model.Schema{}}
		if b.Example != nil {
			s = InferSchema(b.Example)
			s.Example = b.Example
		}
		clearRequired(s)
		for _, name := range b.Required {
			parent, field, err := fieldParent(s, name)
			if err != nil {
				return nil, fmt.Errorf("body.required %s: %w", name, err)
			}
			parent.Required = append(parent.Required, field)
		}
		names := make([]string, 0, len(b.Fields))
		for name := range b.Fields {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			target, err := fieldSchema(s, name)
			if err != nil {
				return nil, fmt.Errorf("body.fields %s: %w", name, err)
			}
			b.Fields[name].apply(target)
		}
		ep.Body = s
	}

	if len(me.Responses) > 0 {
		ep.Responses = map[string]*model.ResponseSpec{}
		for code, v := range me.Responses {
			desc, _ := v.(string)
			ep.Responses[strings.ToUpper(code)] = &model.ResponseSpec{Description: desc}
		}
	}
	return ep, nil
}

func (f mapField) schema() *model.Schema {
	s := &model.Schema{}
	f.apply(s)
	return s
}

func (f mapField) apply(s *model.Schema) {
	if f.Type != "" {
		s.Type = f.Type
	}
	if f.Format != "" {
		s.Format = f.Format
	}
	if f.Min != nil {
		s.Minimum = f.Min
	}
	if f.Max != nil {
		s.Maximum = f.Max
	}
	if f.MinLength != nil {
		s.MinLength = f.MinLength
	}
	if f.MaxLength != nil {
		s.MaxLength = f.MaxLength
	}
	if f.Pattern != "" {
		s.Pattern = f.Pattern
	}
	if len(f.Enum) > 0 {
		s.Enum = f.Enum
	}
	if f.Nullable {
		s.Nullable = true
	}
}

// fieldSchema finds a field by a dotted name such as "address.city" or
// "items[].qty", creating it if the example left it out.
func fieldSchema(root *model.Schema, name string) (*model.Schema, error) {
	parent, field, err := fieldParent(root, name)
	if err != nil {
		return nil, err
	}
	if parent.Properties == nil {
		parent.Properties = map[string]*model.Schema{}
	}
	s := parent.Properties[field]
	if s == nil {
		s = &model.Schema{}
		parent.Properties[field] = s
	}
	return s, nil
}

func fieldParent(root *model.Schema, name string) (*model.Schema, string, error) {
	parts := strings.Split(name, ".")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		arr := strings.HasSuffix(part, "[]")
		part = strings.TrimSuffix(part, "[]")
		next := cur.Properties[part]
		if next == nil {
			return nil, "", fmt.Errorf("%s is not in the example body", part)
		}
		if arr {
			if next.Items == nil {
				return nil, "", fmt.Errorf("%s is not an array in the example body", part)
			}
			next = next.Items
		}
		cur = next
	}
	if cur.Type != "" && cur.Type != "object" {
		return nil, "", fmt.Errorf("its parent is a %s, not an object", cur.Type)
	}
	return cur, parts[len(parts)-1], nil
}

// clearRequired drops the "every field is required" guess InferSchema makes
// from an example; only the fields the map lists are required.
func clearRequired(s *model.Schema) {
	if s == nil {
		return
	}
	s.Required = nil
	for _, p := range s.Properties {
		clearRequired(p)
	}
	clearRequired(s.Items)
}

// findMapFile returns the API map in a project, if there is one.
func findMapFile(root string) string {
	for _, name := range []string{MapFile, ".replayharness/api.yml", ".replayharness/api.json"} {
		p := filepath.Join(root, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// mergeMap adds what the API map knows to endpoints found in specs, and adds
// the endpoints only the map has. It returns the endpoints that came from the
// map alone.
func mergeMap(api, m *model.API) map[*model.Endpoint]bool {
	fromMap := map[*model.Endpoint]bool{}
	for _, me := range m.Endpoints {
		var match *model.Endpoint
		for _, ep := range api.Endpoints {
			if ep.Method == me.Method && pathsMatch(ep.Path, me.Path) {
				match = ep
				break
			}
		}
		if match == nil {
			api.Endpoints = append(api.Endpoints, me)
			fromMap[me] = true
			continue
		}
		if me.Code != "" {
			match.Code = me.Code
		}
		match.Rules = append(match.Rules, me.Rules...)
		match.Secured = match.Secured || me.Secured
		if match.Body == nil && me.Body != nil {
			match.Body, match.BodyType, match.BodyRequired = me.Body, me.BodyType, me.BodyRequired
		}
		if len(match.Params) == 0 {
			match.Params = me.Params
		}
	}
	for _, h := range m.AuthHeaders {
		api.AuthHeaders = appendUnique(api.AuthHeaders, h)
	}
	api.Notes = append(api.Notes, m.Notes...)
	return fromMap
}
