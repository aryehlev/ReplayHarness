// Package generate builds request scenarios from discovered endpoints: a valid
// request for each endpoint, then variants that break one thing at a time
// (a missing field, a wrong type, a value past a limit, a malformed body, an
// undefined method) together with what a well-behaved server should answer.
package generate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/discover"
	"github.com/aryehlev/ReplayHarness/internal/gql"
	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Options controls which scenarios are generated.
type Options struct {
	Only           map[string]bool // generate only these categories (empty means all)
	Skip           map[string]bool // never generate these categories
	MaxPerEndpoint int             // cap per endpoint; 0 means no cap
	LongString     int             // length of the long-string probes
	LargeBody      int             // size in bytes of the oversized-body probe
	NestingDepth   int             // depth of the deeply-nested-JSON probe
}

// DefaultOptions returns the options used by the CLI.
func DefaultOptions() Options {
	return Options{MaxPerEndpoint: 250, LongString: 10_000, LargeBody: 1 << 20, NestingDepth: 1000}
}

// Enabled reports whether a category is generated.
func (o Options) Enabled(cat string) bool {
	if len(o.Only) > 0 && !o.Only[cat] {
		return false
	}
	return !o.Skip[cat]
}

// Suite generates scenarios for every endpoint of the API.
func Suite(api *model.API, opts Options) *model.Suite {
	def := DefaultOptions()
	if opts.LongString <= 0 {
		opts.LongString = def.LongString
	}
	if opts.LargeBody <= 0 {
		opts.LargeBody = def.LargeBody
	}
	if opts.NestingDepth <= 0 {
		opts.NestingDepth = def.NestingDepth
	}
	methods := map[string]map[string]bool{}
	anyMethod := map[string]bool{}
	for _, ep := range api.Endpoints {
		if ep.Protocol != "" {
			continue
		}
		if methods[ep.Path] == nil {
			methods[ep.Path] = map[string]bool{}
		}
		methods[ep.Path][ep.Method] = true
		if ep.AnyMethod {
			anyMethod[ep.Path] = true
		}
	}
	probed := map[string]bool{}
	suite := &model.Suite{API: api}
	for _, ep := range api.Endpoints {
		g := &gen{ep: ep, opts: opts, seen: map[string]bool{}, pathMethods: methods[ep.Path]}
		if ep.Protocol == "" {
			g.probeMethods = !probed[ep.Path] && !anyMethod[ep.Path]
			probed[ep.Path] = true
		}
		suite.Scenarios = append(suite.Scenarios, g.generate()...)
	}
	return suite
}

// Replay turns recorded exchanges into scenarios that expect the recorded
// responses. keys holds the endpoint key of each exchange. Identical requests
// with the same recorded status are kept once.
func Replay(exs []discover.Exchange, keys []string) []model.Scenario {
	var out []model.Scenario
	seen := map[string]bool{}
	for i, ex := range exs {
		req := ex.Request
		req.Method = strings.ToUpper(req.Method)
		req.Headers = discover.SanitizeHeaders(toHeader(req.Headers), true)
		dedupe := fmt.Sprintf("%s %s?%s %s %d", req.Method, req.Path, req.Query, req.Body, ex.Response.Status)
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		rec := ex.Response
		name := fmt.Sprintf("recorded #%d: %s %s", i+1, req.Method, req.Path)
		if req.Query != "" {
			name += "?" + req.Query
		}
		if len(name) > 120 {
			name = name[:117] + "..."
		}
		key := req.Method + " " + req.Path
		if i < len(keys) {
			key = keys[i]
		}
		sc := model.Scenario{Endpoint: key, Category: model.CatReplay, Name: name, Request: req, Expect: model.ExpectRecorded, Recorded: &rec}
		sc.ID = scenarioID(sc)
		out = append(out, sc)
	}
	return out
}

type gen struct {
	ep           *model.Endpoint
	opts         Options
	pathMethods  map[string]bool
	probeMethods bool
	base         baseline
	out          []model.Scenario
	seen         map[string]bool
}

type baseline struct {
	pathValues map[string]string
	query      url.Values
	headers    map[string]string
	hasBody    bool
	body       any    // decoded JSON (or form fields as a map)
	rawBody    string // used when the recorded body is not JSON or a form
}

// variant describes how one scenario differs from the baseline request.
type variant struct {
	category   string
	name       string
	expect     model.Expect
	strict     bool
	method     string
	pathValues map[string]string
	query      url.Values
	rawQuery   *string
	headers    map[string]string // "" deletes the header
	setBody    bool
	body       any
	rawBody    *string
	noBody     bool
	stripAuth  bool
	repeat     int
}

func (g *gen) generate() []model.Scenario {
	g.buildBaseline()
	switch g.ep.Protocol {
	case model.ProtoGRPC:
		g.happy()
		g.auth()
		g.bodyFields()
		g.grpcShape()
	case model.ProtoGraphQL:
		if g.ep.GraphQL == nil || g.ep.GraphQL.Operation == "subscription" {
			return nil // subscriptions run over a WebSocket protocol of their own
		}
		g.happy()
		g.auth()
		g.bodyFields()
		g.graphqlShape()
	case model.ProtoWebSocket:
		g.happy()
		g.auth()
		g.bodyFields()
		g.pathParams()
		g.wsShape()
	default:
		g.happy()
		g.auth()
		g.bodyFields()
		g.bodyShape()
		g.pathParams()
		g.queryParams()
		g.headerProbes()
		g.wrongMethods()
		g.consistency()
	}
	return capScenarios(g.out, g.opts.MaxPerEndpoint)
}

func (g *gen) bodyType() string {
	if g.ep.BodyType != "" {
		return g.ep.BodyType
	}
	return "application/json"
}

func (g *gen) isJSON() bool { return strings.Contains(g.bodyType(), "json") }
func (g *gen) isForm() bool { return strings.Contains(g.bodyType(), "x-www-form-urlencoded") }

func (g *gen) expectsBody() bool {
	if g.ep.Body != nil || g.ep.BodyType != "" {
		return true
	}
	for _, s := range g.ep.Samples {
		if s.Body != "" {
			return true
		}
	}
	return false
}

// strictFor reports whether a broken rule is the spec's own rule (so breaking
// it is an error) rather than one guessed from code or traffic.
func (g *gen) strictFor(required bool) bool { return required && !g.ep.Inferred }

func (g *gen) buildBaseline() {
	b := baseline{pathValues: map[string]string{}, query: url.Values{}, headers: map[string]string{}}
	var sample *model.Sample
	if len(g.ep.Samples) > 0 {
		sample = &g.ep.Samples[0]
	}
	for _, p := range g.ep.Params {
		switch p.In {
		case "path":
			if sample != nil && sample.PathValues[p.Name] != "" {
				b.pathValues[p.Name] = sample.PathValues[p.Name]
			} else {
				b.pathValues[p.Name] = scalarString(validParam(p))
			}
		case "query":
			if sample == nil && p.Required {
				b.query.Set(p.Name, scalarString(validParam(p)))
			}
		case "header":
			if p.Required {
				b.headers[headerName(p.Name)] = scalarString(validParam(p))
			}
		}
	}
	if sample != nil {
		b.query, _ = url.ParseQuery(sample.Query)
	}
	if g.expectsBody() {
		b.hasBody = true
		switch {
		case sample != nil && sample.Body != "":
			var v any
			if g.isForm() {
				if f, err := url.ParseQuery(sample.Body); err == nil {
					m := map[string]any{}
					for k := range f {
						m[k] = f.Get(k)
					}
					v = m
				}
			} else if json.Unmarshal([]byte(sample.Body), &v) != nil {
				v = nil
			}
			if v != nil {
				b.body = v
			} else {
				b.rawBody = sample.Body
			}
		case g.ep.Body != nil && (g.ep.Body.Type != "" || len(g.ep.Body.Properties) > 0 || g.ep.Body.Example != nil):
			b.body = Valid(g.ep.Body, "")
		default:
			b.body = map[string]any{}
		}
	}
	g.base = b
}

func validParam(p model.Param) any {
	if p.Example != nil {
		return p.Example
	}
	if p.Schema == nil {
		if p.In == "path" {
			return "1"
		}
		return "test"
	}
	return Valid(p.Schema, p.Name)
}

func headerName(name string) string {
	return strings.TrimSpace(name)
}

func (g *gen) add(v variant) {
	if !g.opts.Enabled(v.category) {
		return
	}
	req := model.Request{Protocol: g.ep.Protocol, Method: g.ep.Method, Headers: map[string]string{}}
	switch g.ep.Protocol {
	case model.ProtoGraphQL:
		req.Method = "POST"
	case model.ProtoWebSocket:
		req.Method, req.WaitMS = "GET", 500
	}
	if v.method != "" {
		req.Method = v.method
	}
	pv := map[string]string{}
	for k, val := range g.base.pathValues {
		pv[k] = val
	}
	for k, val := range v.pathValues {
		pv[k] = val
	}
	req.Path = fillPath(g.ep.Path, pv)
	q := g.base.query
	if v.query != nil {
		q = v.query
	}
	req.Query = q.Encode()
	if v.rawQuery != nil {
		req.Query = *v.rawQuery
	}
	for k, val := range g.base.headers {
		req.Headers[k] = val
	}
	switch {
	case g.ep.Protocol == model.ProtoGRPC:
		// The message is JSON here and protobuf on the wire; there are no
		// Content-Type or body-shape variants.
		body := g.base.body
		if v.setBody {
			body = v.body
		}
		req.Body = g.encode(body)
	case g.ep.Protocol == model.ProtoWebSocket:
		var msgs []any
		switch {
		case v.rawBody != nil:
			msgs = []any{*v.rawBody}
		case v.setBody:
			msgs = []any{v.body}
		case g.base.hasBody && g.base.body != nil:
			msgs = []any{g.base.body}
		}
		if msgs == nil {
			msgs = []any{}
		}
		req.Body = g.encode(msgs)
	case !v.noBody && (g.base.hasBody || v.setBody || v.rawBody != nil):
		switch {
		case v.rawBody != nil:
			req.Body = *v.rawBody
		case v.setBody:
			req.Body = g.encode(v.body)
		case g.base.rawBody != "":
			req.Body = g.base.rawBody
		default:
			req.Body = g.encode(g.base.body)
		}
		if g.ep.Protocol == model.ProtoGraphQL && v.rawBody == nil {
			var vars any
			_ = json.Unmarshal([]byte(req.Body), &vars)
			req.Body = gql.Envelope(gql.Query(g.ep.GraphQL), vars, "")
		}
		req.Headers["Content-Type"] = g.bodyType()
	}
	for k, val := range v.headers {
		if val == "" {
			delete(req.Headers, k)
		} else {
			req.Headers[k] = val
		}
	}
	if len(req.Headers) == 0 {
		req.Headers = nil
	}
	if g.ep.Protocol == model.ProtoWebSocket && v.expect == model.ExpectReject && v.category != model.CatAuth {
		// A WebSocket server answers a bad message with a message of its own;
		// only a crash or an error close is a finding.
		v.expect, v.strict = model.ExpectNoCrash, false
	}
	sc := model.Scenario{
		Endpoint: g.ep.Key(), Category: v.category, Name: v.name, Request: req,
		Expect: v.expect, Strict: v.strict, Repeat: v.repeat, StripAuth: v.stripAuth,
	}
	sc.ID = scenarioID(sc)
	if g.seen[sc.ID] {
		return
	}
	g.seen[sc.ID] = true
	g.out = append(g.out, sc)
}

func (g *gen) encode(v any) string {
	if g.isForm() {
		m, ok := v.(map[string]any)
		if !ok {
			return scalarString(v)
		}
		vals := url.Values{}
		for k, val := range m {
			vals.Set(k, scalarString(val))
		}
		return vals.Encode()
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

var tmplParamRe = regexp.MustCompile(`\{([^}/]+)\}`)

func fillPath(tmpl string, values map[string]string) string {
	return tmplParamRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		v, ok := values[m[1:len(m)-1]]
		if !ok {
			v = "1"
		}
		return url.PathEscape(v)
	})
}

func (g *gen) happy() {
	if g.ep.Inferred && len(g.ep.Samples) == 0 {
		// Routes found in code: nothing says what a valid request looks like.
		name := "baseline request"
		if g.base.hasBody {
			name += " (body shape unknown, sends {})"
		}
		g.add(variant{category: model.CatHappy, name: name, expect: model.ExpectNoCrash})
	} else {
		g.add(variant{category: model.CatHappy, name: "valid request", expect: model.ExpectSuccess})
	}
	for i, s := range g.ep.Samples {
		if i == 0 {
			continue
		}
		q, body := s.Query, s.Body
		v := variant{category: model.CatHappy, name: fmt.Sprintf("recorded request #%d", i+1), expect: model.ExpectSuccess, pathValues: s.PathValues, rawQuery: &q}
		if body != "" {
			v.rawBody = &body
		}
		g.add(v)
	}
}

func (g *gen) auth() {
	if !g.ep.Secured {
		return
	}
	g.add(variant{category: model.CatAuth, name: "no credentials", expect: model.ExpectReject, strict: true, stripAuth: true})
	g.add(variant{category: model.CatAuth, name: "invalid bearer token", expect: model.ExpectReject, strict: true, stripAuth: true,
		headers: map[string]string{"Authorization": "Bearer replayharness-invalid-token"}})
}

// --- body fields -----------------------------------------------------------

type probe struct {
	category string
	name     string
	value    any
	expect   model.Expect
	strict   bool
}

func (g *gen) bodyFields() {
	if !g.base.hasBody || g.ep.Body == nil || g.base.body == nil {
		return
	}
	if !g.isJSON() && !g.isForm() {
		return
	}
	g.walkFields(g.ep.Body, nil, 0)
}

func (g *gen) walkFields(s *model.Schema, path []any, depth int) {
	if s == nil || depth > 3 {
		return
	}
	switch {
	case s.Type == "object" || (s.Type == "" && len(s.Properties) > 0):
		for _, name := range s.PropertyNames() {
			child := s.Properties[name]
			p := append(append([]any(nil), path...), name)
			g.fieldProbes(child, p, s.IsRequired(name))
			if !g.isForm() {
				g.walkFields(child, p, depth+1)
			}
		}
	case s.Type == "array" && s.Items != nil && !g.isForm():
		g.walkFields(s.Items, append(append([]any(nil), path...), 0), depth+1)
	}
}

func (g *gen) fieldProbes(s *model.Schema, path []any, required bool) {
	if s == nil {
		return
	}
	label := g.fieldLabel() + pathString(path)
	if required {
		if g.ep.Inferred {
			g.mutate(probe{model.CatMissing, label + " omitted (present in every sample)", nil, model.ExpectNoCrash, false}, path, true)
		} else {
			g.mutate(probe{model.CatMissing, label + " omitted (required)", nil, model.ExpectReject, true}, path, true)
		}
	}
	if g.isForm() {
		for _, p := range boundaryProbes(s, label, nameOf(path), g.strictFor(true)) {
			p.value = scalarString(p.value)
			g.mutate(p, path, false)
		}
		for _, p := range stringExtremes(s, label, g.opts) {
			g.mutate(p, path, false)
		}
		return
	}
	if !s.Nullable && s.Type != "" {
		if required && !g.ep.Inferred {
			g.mutate(probe{model.CatNull, label + " = null", nil, model.ExpectReject, true}, path, false)
		} else {
			g.mutate(probe{model.CatNull, label + " = null", nil, model.ExpectNoCrash, false}, path, false)
		}
	}
	if g.ep.Protocol == model.ProtoGRPC {
		// A wrong type cannot be encoded as protobuf. An enum number the
		// .proto does not define can.
		if len(s.Enum) > 0 {
			g.mutate(probe{model.CatBoundary, label + " = 999 (an enum number the .proto does not define)", json.Number("999"), model.ExpectNoCrash, false}, path, false)
		}
	} else {
		for _, w := range wrongTypes(s) {
			g.mutate(probe{model.CatWrongType, fmt.Sprintf("%s = %s (%s, expected %s)", label, show(w.value), w.name, typeName(s)), w.value, model.ExpectReject, false}, path, false)
		}
	}
	for _, p := range boundaryProbes(s, label, nameOf(path), g.strictFor(true)) {
		g.mutate(p, path, false)
	}
	for _, p := range extremeProbes(s, label, g.opts) {
		g.mutate(p, path, false)
	}
}

func (g *gen) fieldLabel() string {
	switch g.ep.Protocol {
	case model.ProtoGRPC:
		return "message."
	case model.ProtoGraphQL:
		return "$"
	case model.ProtoWebSocket:
		return "message."
	}
	return "body."
}

func nameOf(path []any) string {
	for i := len(path) - 1; i >= 0; i-- {
		if s, ok := path[i].(string); ok {
			return s
		}
	}
	return ""
}

func (g *gen) mutate(p probe, path []any, del bool) {
	body, ok := setAt(deepCopy(g.base.body), path, p.value, del)
	if !ok {
		return
	}
	g.add(variant{category: p.category, name: p.name, expect: p.expect, strict: p.strict, setBody: true, body: body})
}

type typed struct {
	name  string
	value any
}

func wrongTypes(s *model.Schema) []typed {
	switch s.Type {
	case "string":
		return []typed{{"number", json.Number("12345")}, {"boolean", true}, {"array", []any{"a"}}}
	case "integer":
		return []typed{{"string", "abc"}, {"float", 1.5}, {"boolean", true}}
	case "number":
		return []typed{{"string", "abc"}, {"boolean", true}}
	case "boolean":
		return []typed{{"string", "yes"}, {"number", json.Number("1")}}
	case "array":
		return []typed{{"string", "not-an-array"}, {"object", map[string]any{}}}
	case "object":
		return []typed{{"string", "not-an-object"}, {"array", []any{}}}
	}
	return nil
}

// boundaryProbes probes the limits the schema declares: values exactly at a
// limit should be accepted, values just past it rejected.
func boundaryProbes(s *model.Schema, label, name string, strictEnum bool) []probe {
	var out []probe
	at := func(n string, v any) {
		out = append(out, probe{model.CatBoundary, label + " " + n, v, model.ExpectSuccess, false})
	}
	past := func(n string, v any) {
		out = append(out, probe{model.CatBoundary, label + " " + n, v, model.ExpectReject, false})
	}
	switch s.Type {
	case "integer", "number":
		integer := s.Type == "integer"
		if s.Minimum != nil {
			m := *s.Minimum
			if s.ExclusiveMinimum {
				past(fmt.Sprintf("= %s (exclusive minimum)", formatNum(m)), numValue(m, integer))
			} else {
				at(fmt.Sprintf("= %s (minimum)", formatNum(m)), numValue(m, integer))
				past(fmt.Sprintf("= %s (below minimum %s)", formatNum(m-1), formatNum(m)), numValue(m-1, integer))
			}
		}
		if s.Maximum != nil {
			m := *s.Maximum
			if s.ExclusiveMaximum {
				past(fmt.Sprintf("= %s (exclusive maximum)", formatNum(m)), numValue(m, integer))
			} else {
				at(fmt.Sprintf("= %s (maximum)", formatNum(m)), numValue(m, integer))
				past(fmt.Sprintf("= %s (above maximum %s)", formatNum(m+1), formatNum(m)), numValue(m+1, integer))
			}
		}
	case "string":
		fixed := fixedFormat(s.Format) || s.Pattern != ""
		if s.MinLength != nil && *s.MinLength > 0 {
			n := *s.MinLength
			if !fixed {
				at(fmt.Sprintf("is %s (minLength)", chars(n)), fillString(s, name, n))
			}
			past(fmt.Sprintf("is %s (below minLength %d)", chars(n-1), n), strings.Repeat("a", n-1))
		}
		if s.MaxLength != nil {
			n := *s.MaxLength
			if !fixed {
				at(fmt.Sprintf("is %s (maxLength)", chars(n)), fillString(s, name, n))
			}
			past(fmt.Sprintf("is %s (above maxLength %d)", chars(n+1), n), fillString(s, name, n+1))
		}
		if len(s.Enum) == 0 {
			if bad, ok := badFormat[s.Format]; ok {
				out = append(out, probe{model.CatFormat, fmt.Sprintf("%s = %s (not a valid %s)", label, show(bad), s.Format), bad, model.ExpectReject, false})
			}
		}
	case "array":
		if s.MinItems != nil && *s.MinItems > 0 {
			n := *s.MinItems - 1
			past(fmt.Sprintf("has %d items (below minItems %d)", n, *s.MinItems), repeatItem(s, name, n))
		}
		if s.MaxItems != nil {
			n := *s.MaxItems + 1
			past(fmt.Sprintf("has %d items (above maxItems %d)", n, *s.MaxItems), repeatItem(s, name, n))
		}
	}
	if len(s.Enum) > 0 {
		var bad any = "not-a-valid-option"
		if s.Type == "integer" || s.Type == "number" {
			bad = json.Number("987654321")
		}
		out = append(out, probe{model.CatBoundary, fmt.Sprintf("%s = %s (not in enum)", label, show(bad)), bad, model.ExpectReject, strictEnum})
		if first, ok := s.Enum[0].(string); ok {
			if alt := swapCase(first); alt != first && !enumHas(s.Enum, alt) {
				out = append(out, probe{model.CatBoundary, fmt.Sprintf("%s = %s (enum value in a different case)", label, show(alt)), alt, model.ExpectReject, false})
			}
		}
	}
	return out
}

var badFormat = map[string]string{
	"email": "not-an-email", "uuid": "not-a-uuid", "date": "2024-13-45", "date-time": "yesterday at noon",
	"uri": "not a uri", "url": "not a url", "ipv4": "999.999.999.999", "ipv6": "not:an:ip", "byte": "not base64!",
	"hostname": "-invalid-.example", "time": "25:61:61",
}

func repeatItem(s *model.Schema, name string, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = Valid(s.Items, singularName(name))
	}
	return out
}

func swapCase(s string) string {
	if strings.ToUpper(s) != s {
		return strings.ToUpper(s)
	}
	return strings.ToLower(s)
}

func enumHas(enum []any, v string) bool {
	for _, e := range enum {
		if e == v {
			return true
		}
	}
	return false
}

// extremeProbes are unusual but not invalid values. The server may accept or
// reject them; it must not crash.
func extremeProbes(s *model.Schema, label string, opts Options) []probe {
	nc := func(n string, v any) probe {
		return probe{model.CatExtreme, label + " = " + n, v, model.ExpectNoCrash, false}
	}
	var out []probe
	switch s.Type {
	case "integer":
		if s.Maximum == nil {
			out = append(out, nc("9223372036854775807 (int64 max)", json.Number("9223372036854775807")),
				nc("9223372036854775808 (int64 max + 1)", json.Number("9223372036854775808")))
		}
		if s.Minimum == nil {
			out = append(out, nc("-1", int64(-1)), nc("0", int64(0)),
				nc("-9223372036854775809 (int64 min - 1)", json.Number("-9223372036854775809")))
		}
	case "number":
		if s.Maximum == nil {
			out = append(out, nc("1e308", json.Number("1e308")), nc("1e400 (overflows float64)", json.Number("1e400")))
		}
		if s.Minimum == nil {
			out = append(out, nc("-0", json.Number("-0")), nc("-1.5", -1.5))
		}
	case "string":
		out = append(out, stringExtremes(s, label, opts)...)
	case "array":
		if s.MinItems == nil || *s.MinItems == 0 {
			out = append(out, nc("[] (empty array)", []any{}))
		}
		if s.MaxItems == nil {
			out = append(out, nc("1000 items", repeatItem(s, "", 1000)))
		}
	case "object":
		out = append(out, nc("{} (empty object)", map[string]any{}))
	}
	return out
}

func stringExtremes(s *model.Schema, label string, opts Options) []probe {
	nc := func(n string, v any) probe {
		return probe{model.CatExtreme, label + " = " + n, v, model.ExpectNoCrash, false}
	}
	var out []probe
	if len(s.Enum) > 0 {
		return out
	}
	if s.MinLength == nil || *s.MinLength == 0 {
		out = append(out, nc("empty string", ""))
	}
	out = append(out,
		nc("whitespace only", "   "),
		nc("unicode (emoji, CJK, RTL, combining marks)", "Zoë 🚀 测试 עברית é"),
		nc("NUL and control characters", "a\x00b\x01c\x1bd"),
		nc("newlines and tabs", "line1\nline2\r\n\tindented"),
		nc("leading and trailing spaces", "  padded  "),
	)
	if s.MaxLength == nil {
		out = append(out, nc(fmt.Sprintf("%d-char string", opts.LongString), strings.Repeat("a", opts.LongString)))
	}
	return out
}

// --- body shape ------------------------------------------------------------

func (g *gen) bodyShape() {
	if !g.base.hasBody {
		return
	}
	known := g.ep.Body != nil && !g.ep.Inferred
	hasRequired := g.ep.Body != nil && len(g.ep.Body.Required) > 0
	objectBody := g.ep.Body == nil || g.ep.Body.Type == "object" || len(g.ep.Body.Properties) > 0
	rejectIf := func(cond bool) model.Expect {
		if cond {
			return model.ExpectReject
		}
		return model.ExpectNoCrash
	}
	raw := func(name, body string, expect model.Expect, strict bool, headers map[string]string) {
		b := body
		g.add(variant{category: model.CatBodyShape, name: name, expect: expect, strict: strict, rawBody: &b, headers: headers})
	}

	raw("empty body", "", rejectIf(known && (g.ep.BodyRequired || hasRequired)), false, nil)
	if !g.isJSON() {
		raw("JSON body with a form content type", `{"replayharness":true}`, model.ExpectNoCrash, false, nil)
		raw("no Content-Type header", g.encode(g.base.body), model.ExpectNoCrash, false, map[string]string{"Content-Type": ""})
		return
	}
	if objectBody {
		raw("{} (empty object)", "{}", rejectIf(hasRequired), g.strictFor(hasRequired), nil)
		raw("[] (array instead of object)", "[]", rejectIf(known), false, nil)
	}
	raw("null", "null", rejectIf(known && !g.ep.Body.Nullable), false, nil)
	raw(`"a string" instead of a JSON object`, `"just a string"`, rejectIf(known), false, nil)
	raw("malformed JSON (truncated)", `{"name": "unterminated`, rejectIf(known), false, nil)
	raw("malformed JSON (trailing comma)", `{"a": 1,}`, rejectIf(known), false, nil)
	d := g.opts.NestingDepth
	raw(fmt.Sprintf("JSON nested %d levels deep", d), strings.Repeat(`{"a":`, d)+"1"+strings.Repeat("}", d), model.ExpectNoCrash, false, nil)

	base := g.encode(g.base.body)
	if m, ok := g.base.body.(map[string]any); ok {
		if len(m) > 0 {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			dup, _ := json.Marshal(m[keys[0]])
			kb, _ := json.Marshal(keys[0])
			raw(fmt.Sprintf("duplicate key %q", keys[0]), "{"+string(kb)+":"+string(dup)+","+strings.TrimPrefix(base, "{"), model.ExpectNoCrash, false, nil)
		}
		extra := deepCopy(m).(map[string]any)
		extra["replayharnessUnexpectedField"] = "surprise"
		g.add(variant{category: model.CatBodyShape, name: "unknown extra field", expect: model.ExpectNoCrash, setBody: true, body: extra})
		big := deepCopy(m).(map[string]any)
		big["replayharnessPadding"] = strings.Repeat("x", g.opts.LargeBody)
		g.add(variant{category: model.CatBodyShape, name: fmt.Sprintf("oversized body (%s)", byteSize(g.opts.LargeBody)), expect: model.ExpectNoCrash, setBody: true, body: big})
	}
	raw("valid JSON sent as text/plain", base, model.ExpectNoCrash, false, map[string]string{"Content-Type": "text/plain"})
	raw("no Content-Type header", base, model.ExpectNoCrash, false, map[string]string{"Content-Type": ""})
	raw("form-encoded body instead of JSON", "replayharness=1&name=test", model.ExpectNoCrash, false, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	raw("JSON with a UTF-8 byte order mark", "\ufeff"+base, model.ExpectNoCrash, false, nil)
}

func chars(n int) string {
	if n == 1 {
		return "1 char"
	}
	return fmt.Sprintf("%d chars", n)
}

func byteSize(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// --- path, query, headers, methods -----------------------------------------

func (g *gen) pathParams() {
	for _, p := range g.ep.ParamsIn("path") {
		label := "path " + p.Name
		typ, format := "", ""
		if p.Schema != nil {
			typ, format = p.Schema.Type, p.Schema.Format
		}
		add := func(n, v string, expect model.Expect, strict bool) {
			g.add(variant{category: model.CatPathParam, name: fmt.Sprintf("%s = %s", label, n), expect: expect, strict: strict, pathValues: map[string]string{p.Name: v}})
		}
		add(`"999999999" (likely nonexistent)`, "999999999", model.ExpectNoCrash, false)
		add(`"0"`, "0", model.ExpectNoCrash, false)
		add(`"-1"`, "-1", model.ExpectNoCrash, false)
		add(`"99999999999999999999" (overflows int64)`, "99999999999999999999", model.ExpectNoCrash, false)
		add("2000-char string", strings.Repeat("a", 2000), model.ExpectNoCrash, false)
		add("unicode", "ünïcødé-测试", model.ExpectNoCrash, false)
		add("contains a space", "a b", model.ExpectNoCrash, false)
		add("encoded slash", "a/b", model.ExpectNoCrash, false)
		switch {
		case typ == "integer" || typ == "number":
			add(`"abc" (not a number)`, "abc", model.ExpectReject, false)
			if typ == "integer" {
				add(`"1.5" (not an integer)`, "1.5", model.ExpectReject, false)
			}
		case format == "uuid":
			add(`"not-a-uuid"`, "not-a-uuid", model.ExpectReject, false)
			add("nil UUID", "00000000-0000-0000-0000-000000000000", model.ExpectNoCrash, false)
		default:
			add(`"abc"`, "abc", model.ExpectNoCrash, false)
		}
		if p.Schema != nil && len(p.Schema.Enum) > 0 {
			add(`"not-a-valid-option" (not in enum)`, "not-a-valid-option", model.ExpectReject, g.strictFor(true))
		}
		if p.Schema != nil && (p.Schema.Minimum != nil || p.Schema.Maximum != nil) {
			for _, bp := range boundaryProbes(p.Schema, label, p.Name, false) {
				g.add(variant{category: model.CatPathParam, name: bp.name, expect: bp.expect, pathValues: map[string]string{p.Name: scalarString(bp.value)}})
			}
		}
	}
}

func (g *gen) queryParams() {
	params := g.ep.ParamsIn("query")
	with := func(mod func(q url.Values)) url.Values {
		q := url.Values{}
		for k, v := range g.base.query {
			q[k] = append([]string(nil), v...)
		}
		mod(q)
		return q
	}
	for _, p := range params {
		p := p
		label := "query " + p.Name
		valid := scalarString(validParam(p))
		set := func(cat, n, v string, expect model.Expect, strict bool) {
			g.add(variant{category: cat, name: fmt.Sprintf("%s = %s", label, n), expect: expect, strict: strict, query: with(func(q url.Values) { q.Set(p.Name, v) })})
		}
		switch {
		case p.Required && g.ep.Inferred:
			g.add(variant{category: model.CatMissing, name: label + " omitted (present in every sample)", expect: model.ExpectNoCrash, query: with(func(q url.Values) { q.Del(p.Name) })})
		case p.Required:
			g.add(variant{category: model.CatMissing, name: label + " omitted (required)", expect: model.ExpectReject, strict: true, query: with(func(q url.Values) { q.Del(p.Name) })})
		default:
			set(model.CatQuery, fmt.Sprintf("%q (valid optional value)", valid), valid, model.ExpectSuccess, false)
		}
		typ := ""
		if p.Schema != nil {
			typ = p.Schema.Type
		}
		switch typ {
		case "integer", "number":
			set(model.CatWrongType, `"abc" (not a number)`, "abc", model.ExpectReject, false)
			set(model.CatExtreme, `"-1"`, "-1", model.ExpectNoCrash, false)
			set(model.CatExtreme, `"99999999999999999999" (overflows int64)`, "99999999999999999999", model.ExpectNoCrash, false)
		case "boolean":
			set(model.CatWrongType, `"maybe" (not a boolean)`, "maybe", model.ExpectReject, false)
		case "array":
			g.add(variant{category: model.CatQuery, name: label + " repeated 3 times", expect: model.ExpectNoCrash, query: with(func(q url.Values) {
				q.Del(p.Name)
				for i := 0; i < 3; i++ {
					q.Add(p.Name, valid)
				}
			})})
		}
		if p.Schema != nil {
			for _, bp := range boundaryProbes(p.Schema, label, p.Name, g.strictFor(true)) {
				v := scalarString(bp.value)
				g.add(variant{category: bp.category, name: bp.name, expect: bp.expect, strict: bp.strict, query: with(func(q url.Values) { q.Set(p.Name, v) })})
			}
		}
		set(model.CatExtreme, "empty value", "", model.ExpectNoCrash, false)
		set(model.CatExtreme, fmt.Sprintf("%d-char string", g.opts.LongString), strings.Repeat("a", g.opts.LongString), model.ExpectNoCrash, false)
		set(model.CatExtreme, "unicode", "Zoë 🚀 测试", model.ExpectNoCrash, false)
		if typ != "array" {
			g.add(variant{category: model.CatQuery, name: label + " given twice", expect: model.ExpectNoCrash, query: with(func(q url.Values) {
				q.Del(p.Name)
				q.Add(p.Name, valid)
				q.Add(p.Name, valid+"2")
			})})
		}
	}
	g.add(variant{category: model.CatQuery, name: "unknown query parameter", expect: model.ExpectNoCrash, query: with(func(q url.Values) { q.Set("replayharness_unknown", "1") })})
	if len(params) == 0 && g.ep.Inferred && g.ep.Method == "GET" {
		// Undocumented endpoints often read pagination parameters.
		for _, kv := range [][2]string{{"limit", "-1"}, {"limit", "1000000"}, {"offset", "-1"}, {"page", "0"}, {"page", "-1"}, {"page", "abc"}} {
			kv := kv
			g.add(variant{category: model.CatQuery, name: fmt.Sprintf("query %s = %q (pagination guess)", kv[0], kv[1]), expect: model.ExpectNoCrash,
				query: with(func(q url.Values) { q.Set(kv[0], kv[1]) })})
		}
	}
}

func (g *gen) headerProbes() {
	g.add(variant{category: model.CatHeaders, name: "Accept: application/xml", expect: model.ExpectNoCrash, headers: map[string]string{"Accept": "application/xml"}})
	for _, p := range g.ep.ParamsIn("header") {
		name := headerName(p.Name)
		if p.Required {
			g.add(variant{category: model.CatMissing, name: "header " + name + " omitted (required)", expect: model.ExpectReject, strict: g.strictFor(true), headers: map[string]string{name: ""}})
		}
		if p.Schema != nil && (p.Schema.Type == "integer" || p.Schema.Type == "number") {
			g.add(variant{category: model.CatWrongType, name: "header " + name + ` = "abc" (not a number)`, expect: model.ExpectReject, headers: map[string]string{name: "abc"}})
		}
	}
}

func (g *gen) wrongMethods() {
	if !g.probeMethods {
		return
	}
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		if g.pathMethods[m] {
			continue
		}
		if g.ep.Inferred {
			g.add(variant{category: model.CatMethod, method: m, noBody: true, name: m + " (not seen for this path)", expect: model.ExpectNoCrash})
			continue
		}
		g.add(variant{category: model.CatMethod, method: m, noBody: true, name: m + " (not defined for this path)", expect: model.ExpectReject})
	}
}

func (g *gen) consistency() {
	if g.ep.Method != "GET" {
		return
	}
	g.add(variant{category: model.CatConsistency, name: "same GET sent twice", expect: model.ExpectNoCrash, repeat: 2})
}

// capScenarios keeps at most max scenarios, taking them round-robin across
// categories so a large body schema does not crowd out the other checks.
func capScenarios(in []model.Scenario, max int) []model.Scenario {
	if max <= 0 || len(in) <= max {
		return in
	}
	byCat := map[string][]model.Scenario{}
	var order []string
	for _, s := range in {
		if _, ok := byCat[s.Category]; !ok {
			order = append(order, s.Category)
		}
		byCat[s.Category] = append(byCat[s.Category], s)
	}
	keep := map[string]bool{}
	for n := 0; n < max; {
		progressed := false
		for _, c := range order {
			if len(byCat[c]) > 0 && n < max {
				keep[byCat[c][0].ID] = true
				byCat[c] = byCat[c][1:]
				n++
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	out := make([]model.Scenario, 0, max)
	for _, s := range in {
		if keep[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

func scenarioID(sc model.Scenario) string {
	h := fnv.New64a()
	for _, s := range []string{sc.Endpoint, sc.Category, sc.Name, sc.Request.Method, sc.Request.Path, sc.Request.Query, sc.Request.Body} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%016x", h.Sum64())[:12]
}

func toHeader(m map[string]string) map[string][]string {
	h := map[string][]string{}
	for k, v := range m {
		h[k] = []string{v}
	}
	return h
}
