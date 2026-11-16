// Package discover finds the endpoints of a service: from an OpenAPI or
// Swagger document, from the routes in a source tree, from a running server,
// or from recorded traffic.
package discover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

var specMethods = []string{"get", "post", "put", "patch", "delete", "head", "options"}

// LoadSpecFile parses an OpenAPI 3.x or Swagger 2.0 file (JSON or YAML).
func LoadSpecFile(path string) (*model.API, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSpec(data, path)
}

// ParseSpec parses an OpenAPI 3.x or Swagger 2.0 document.
func ParseSpec(data []byte, origin string) (*model.API, error) {
	doc, err := decodeDoc(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	if !isSpec(doc) {
		if isAPIMap(doc) {
			return parseAPIMap(doc, origin)
		}
		return nil, fmt.Errorf("%s: not an OpenAPI or Swagger document, or an API map (a document with endpoints: [...])", origin)
	}
	p := &specParser{root: doc, cache: map[string]*model.Schema{}, visiting: map[string]bool{}}
	_, p.swagger2 = doc["swagger"]
	return p.parse(origin), nil
}

func decodeDoc(data []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(data)
	var doc any
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return nil, err
		}
	} else if err := yaml.Unmarshal(trimmed, &doc); err != nil {
		return nil, err
	}
	m, ok := normalize(doc).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document is not an object")
	}
	return m, nil
}

// normalize turns YAML's map[any]any (used when keys are not all strings,
// e.g. response codes) into map[string]any.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalize(val)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
		return t
	}
	return v
}

func isSpec(doc map[string]any) bool {
	_, oas := doc["openapi"]
	_, sw := doc["swagger"]
	return oas || sw
}

type specParser struct {
	root     map[string]any
	swagger2 bool
	cache    map[string]*model.Schema
	visiting map[string]bool
}

func (p *specParser) parse(origin string) *model.API {
	api := &model.API{Sources: []string{origin}}
	if info, ok := p.root["info"].(map[string]any); ok {
		api.Title = str(info["title"])
	}
	api.AuthHeaders = p.authHeaders()
	base := p.basePath()
	globalSecured := hasSecurity(p.root["security"])
	consumes := strs(p.root["consumes"])

	paths, _ := p.root["paths"].(map[string]any)
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, path := range keys {
		item, ok := p.resolve(paths[path])
		if !ok {
			continue
		}
		shared := p.params(item["parameters"])
		for _, method := range specMethods {
			op, ok := p.resolve(item[method])
			if !ok {
				continue
			}
			ep := &model.Endpoint{Method: strings.ToUpper(method), Path: base + path, Source: origin}
			secured := globalSecured
			if sec, ok := op["security"]; ok {
				secured = hasSecurity(sec)
			}
			ep.Secured = secured

			var form *model.Schema
			for _, prm := range mergeParams(shared, p.params(op["parameters"])) {
				switch prm.In {
				case "body":
					ep.Body, ep.BodyType, ep.BodyRequired = prm.Schema, "application/json", prm.Required
				case "formData":
					if form == nil {
						form = &model.Schema{Type: "object", Properties: map[string]*model.Schema{}}
					}
					form.Properties[prm.Name] = prm.Schema
					if prm.Required {
						form.Required = append(form.Required, prm.Name)
					}
				case "path", "query", "header":
					ep.Params = append(ep.Params, prm)
				}
			}
			if form != nil {
				ep.Body, ep.BodyType = form, "application/x-www-form-urlencoded"
				for _, c := range append(strs(op["consumes"]), consumes...) {
					if strings.Contains(c, "multipart") {
						ep.BodyType = c
					}
				}
			}
			if !p.swagger2 {
				p.requestBody(ep, op["requestBody"])
			}
			ep.Responses = p.responses(op["responses"])
			api.Endpoints = append(api.Endpoints, ep)
		}
	}
	return api
}

func (p *specParser) requestBody(ep *model.Endpoint, node any) {
	rb, ok := p.resolve(node)
	if !ok {
		return
	}
	ep.BodyRequired = boolean(rb["required"])
	content, ok := rb["content"].(map[string]any)
	if !ok || len(content) == 0 {
		return
	}
	ct := pickContentType(content)
	ep.BodyType = ct
	cm, ok := content[ct].(map[string]any)
	if !ok {
		return
	}
	ep.Body = p.schema(cm["schema"])
	example := cm["example"]
	if example == nil {
		if exs, ok := cm["examples"].(map[string]any); ok {
			names := make([]string, 0, len(exs))
			for n := range exs {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if ex, ok := p.resolve(exs[n]); ok && ex["value"] != nil {
					example = ex["value"]
					break
				}
			}
		}
	}
	if example != nil {
		cp := model.Schema{}
		if ep.Body != nil {
			cp = *ep.Body
		}
		cp.Example = example
		ep.Body = &cp
	}
}

func (p *specParser) responses(node any) map[string]*model.ResponseSpec {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]*model.ResponseSpec{}
	for code, v := range m {
		r, ok := p.resolve(v)
		if !ok {
			r = map[string]any{}
		}
		rs := &model.ResponseSpec{}
		if p.swagger2 {
			if rs.Schema = p.schema(r["schema"]); rs.Schema != nil {
				rs.ContentType = "application/json"
			}
		} else if content, ok := r["content"].(map[string]any); ok && len(content) > 0 {
			rs.ContentType = pickContentType(content)
			if cm, ok := content[rs.ContentType].(map[string]any); ok {
				rs.Schema = p.schema(cm["schema"])
			}
		}
		out[strings.ToUpper(code)] = rs
	}
	return out
}

func (p *specParser) params(node any) []model.Param {
	list, _ := node.([]any)
	var out []model.Param
	for _, item := range list {
		m, ok := p.resolve(item)
		if !ok {
			continue
		}
		in, name := str(m["in"]), str(m["name"])
		if name == "" || in == "" {
			continue
		}
		prm := model.Param{Name: name, In: in, Required: boolean(m["required"]) || in == "path", Example: m["example"]}
		switch {
		case in == "body":
			prm.Schema = p.schema(m["schema"])
		case p.swagger2:
			prm.Schema = p.schema(m) // Swagger 2 keeps type, format and enum on the parameter itself
		default:
			prm.Schema = p.schema(m["schema"])
			if prm.Schema == nil {
				if content, ok := m["content"].(map[string]any); ok && len(content) > 0 {
					if cm, ok := content[pickContentType(content)].(map[string]any); ok {
						prm.Schema = p.schema(cm["schema"])
					}
				}
			}
		}
		out = append(out, prm)
	}
	return out
}

func mergeParams(shared, own []model.Param) []model.Param {
	out := append([]model.Param(nil), shared...)
	for _, o := range own {
		replaced := false
		for i, s := range out {
			if s.In == o.In && s.Name == o.Name {
				out[i], replaced = o, true
			}
		}
		if !replaced {
			out = append(out, o)
		}
	}
	return out
}

// schema converts a JSON Schema node, following $refs. Recursive schemas are
// cut off with an empty object where they loop.
func (p *specParser) schema(node any) *model.Schema {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := m["$ref"].(string); ok {
		if s, ok := p.cache[ref]; ok {
			return s
		}
		if p.visiting[ref] {
			return &model.Schema{Type: "object"}
		}
		p.visiting[ref] = true
		s := p.schema(p.lookup(ref))
		delete(p.visiting, ref)
		if s == nil {
			s = &model.Schema{}
		}
		p.cache[ref] = s
		return s
	}

	s := &model.Schema{}
	if all, ok := m["allOf"].([]any); ok {
		for _, sub := range all {
			merge(s, p.schema(sub))
		}
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		alts, ok := m[key].([]any)
		if !ok {
			continue
		}
		picked := false
		for _, a := range alts {
			sub := p.schema(a)
			if sub == nil {
				continue
			}
			if sub.Type == "null" {
				s.Nullable = true
				continue
			}
			if !picked {
				merge(s, sub)
				picked = true
			}
		}
	}

	switch t := m["type"].(type) {
	case string:
		s.Type = t
	case []any:
		for _, v := range t {
			if v == "null" {
				s.Nullable = true
			} else if s.Type == "" {
				s.Type = str(v)
			}
		}
	}
	if f := str(m["format"]); f != "" {
		s.Format = f
	}
	if props, ok := m["properties"].(map[string]any); ok {
		if s.Properties == nil {
			s.Properties = map[string]*model.Schema{}
		}
		for name, sub := range props {
			if ps := p.schema(sub); ps != nil {
				s.Properties[name] = ps
			}
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if rs, ok := r.(string); ok {
				s.Required = appendUnique(s.Required, rs)
			}
		}
	}
	if it, ok := m["items"]; ok {
		s.Items = p.schema(it)
	}
	if e, ok := m["enum"].([]any); ok {
		s.Enum = e
	}
	if v := num(m["minimum"]); v != nil {
		s.Minimum = v
	}
	if v := num(m["maximum"]); v != nil {
		s.Maximum = v
	}
	// OpenAPI 3.0 uses booleans for exclusive bounds, 3.1 uses numbers.
	if v := num(m["exclusiveMinimum"]); v != nil {
		s.Minimum, s.ExclusiveMinimum = v, true
	} else if boolean(m["exclusiveMinimum"]) {
		s.ExclusiveMinimum = true
	}
	if v := num(m["exclusiveMaximum"]); v != nil {
		s.Maximum, s.ExclusiveMaximum = v, true
	} else if boolean(m["exclusiveMaximum"]) {
		s.ExclusiveMaximum = true
	}
	if v := intp(m["minLength"]); v != nil {
		s.MinLength = v
	}
	if v := intp(m["maxLength"]); v != nil {
		s.MaxLength = v
	}
	if v := intp(m["minItems"]); v != nil {
		s.MinItems = v
	}
	if v := intp(m["maxItems"]); v != nil {
		s.MaxItems = v
	}
	if pat := str(m["pattern"]); pat != "" {
		s.Pattern = pat
	}
	if boolean(m["nullable"]) || boolean(m["x-nullable"]) {
		s.Nullable = true
	}
	if ex, ok := m["example"]; ok && ex != nil {
		s.Example = ex
	} else if exs, ok := m["examples"].([]any); ok && len(exs) > 0 {
		s.Example = exs[0]
	}
	if d, ok := m["default"]; ok && d != nil {
		s.Default = d
	}
	if s.Type == "" {
		switch {
		case s.Properties != nil:
			s.Type = "object"
		case s.Items != nil:
			s.Type = "array"
		}
	}
	return s
}

// merge copies src into dst without sharing maps, so cached schemas stay untouched.
func merge(dst, src *model.Schema) {
	if src == nil {
		return
	}
	if dst.Type == "" {
		dst.Type = src.Type
	}
	if dst.Format == "" {
		dst.Format = src.Format
	}
	if len(src.Properties) > 0 {
		if dst.Properties == nil {
			dst.Properties = map[string]*model.Schema{}
		}
		for k, v := range src.Properties {
			dst.Properties[k] = v
		}
	}
	for _, r := range src.Required {
		dst.Required = appendUnique(dst.Required, r)
	}
	if dst.Items == nil {
		dst.Items = src.Items
	}
	if dst.Enum == nil {
		dst.Enum = src.Enum
	}
	if dst.Minimum == nil {
		dst.Minimum, dst.ExclusiveMinimum = src.Minimum, src.ExclusiveMinimum
	}
	if dst.Maximum == nil {
		dst.Maximum, dst.ExclusiveMaximum = src.Maximum, src.ExclusiveMaximum
	}
	if dst.MinLength == nil {
		dst.MinLength = src.MinLength
	}
	if dst.MaxLength == nil {
		dst.MaxLength = src.MaxLength
	}
	if dst.MinItems == nil {
		dst.MinItems = src.MinItems
	}
	if dst.MaxItems == nil {
		dst.MaxItems = src.MaxItems
	}
	if dst.Pattern == "" {
		dst.Pattern = src.Pattern
	}
	dst.Nullable = dst.Nullable || src.Nullable
	if dst.Example == nil {
		dst.Example = src.Example
	}
	if dst.Default == nil {
		dst.Default = src.Default
	}
}

func (p *specParser) resolve(node any) (map[string]any, bool) {
	for i := 0; i < 16; i++ {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		ref, isRef := m["$ref"].(string)
		if !isRef {
			return m, true
		}
		node = p.lookup(ref)
	}
	return nil, false
}

// lookup resolves a local JSON pointer such as #/components/schemas/User.
// External references are not supported.
func (p *specParser) lookup(ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = p.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		if unescaped, err := url.PathUnescape(part); err == nil {
			part = unescaped
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func (p *specParser) basePath() string {
	if p.swagger2 {
		return strings.TrimRight(str(p.root["basePath"]), "/")
	}
	servers, _ := p.root["servers"].([]any)
	if len(servers) == 0 {
		return ""
	}
	s, _ := servers[0].(map[string]any)
	raw := str(s["url"])
	if vars, ok := s["variables"].(map[string]any); ok {
		for name, v := range vars {
			if vm, ok := v.(map[string]any); ok {
				raw = strings.ReplaceAll(raw, "{"+name+"}", str(vm["default"]))
			}
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// authHeaders lists the header names of apiKey security schemes.
func (p *specParser) authHeaders() []string {
	var schemes map[string]any
	if p.swagger2 {
		schemes, _ = p.root["securityDefinitions"].(map[string]any)
	} else if comps, ok := p.root["components"].(map[string]any); ok {
		schemes, _ = comps["securitySchemes"].(map[string]any)
	}
	var out []string
	for _, v := range schemes {
		m, ok := p.resolve(v)
		if ok && str(m["type"]) == "apiKey" && str(m["in"]) == "header" && str(m["name"]) != "" {
			out = appendUnique(out, str(m["name"]))
		}
	}
	sort.Strings(out)
	return out
}

// hasSecurity reports whether a security requirement list demands credentials.
// An empty list, or one that contains {}, means credentials are optional.
func hasSecurity(node any) bool {
	list, ok := node.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && len(m) == 0 {
			return false
		}
	}
	return true
}

func pickContentType(content map[string]any) string {
	if _, ok := content["application/json"]; ok {
		return "application/json"
	}
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.Contains(k, "json") {
			return k
		}
	}
	for _, k := range keys {
		if k == "application/x-www-form-urlencoded" {
			return k
		}
	}
	return keys[0]
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func boolean(v any) bool {
	b, _ := v.(bool)
	return b
}

func num(v any) *float64 {
	var f float64
	switch t := v.(type) {
	case int:
		f = float64(t)
	case int64:
		f = float64(t)
	case uint64:
		f = float64(t)
	case float64:
		f = t
	case json.Number:
		var err error
		if f, err = t.Float64(); err != nil {
			return nil
		}
	default:
		return nil
	}
	return &f
}

func intp(v any) *int {
	f := num(v)
	if f == nil {
		return nil
	}
	i := int(*f)
	return &i
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}
