// Package model holds the types shared by discovery, generation, running and reporting.
package model

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema is the subset of JSON Schema that ReplayHarness understands.
type Schema struct {
	Type             string             `json:"type,omitempty"`
	Format           string             `json:"format,omitempty"`
	Properties       map[string]*Schema `json:"properties,omitempty"`
	Required         []string           `json:"required,omitempty"`
	Items            *Schema            `json:"items,omitempty"`
	Enum             []any              `json:"enum,omitempty"`
	Minimum          *float64           `json:"minimum,omitempty"`
	Maximum          *float64           `json:"maximum,omitempty"`
	ExclusiveMinimum bool               `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum bool               `json:"exclusiveMaximum,omitempty"`
	MinLength        *int               `json:"minLength,omitempty"`
	MaxLength        *int               `json:"maxLength,omitempty"`
	MinItems         *int               `json:"minItems,omitempty"`
	MaxItems         *int               `json:"maxItems,omitempty"`
	Pattern          string             `json:"pattern,omitempty"`
	Nullable         bool               `json:"nullable,omitempty"`
	Example          any                `json:"example,omitempty"`
	Default          any                `json:"default,omitempty"`
	Exclusive        [][]string         `json:"exclusive,omitempty"` // groups of properties of which at most one may be set (a protobuf oneof)
	TypeName         string             `json:"typeName,omitempty"`  // the protobuf message or GraphQL type, e.g. users.v1.User
}

// IsRequired reports whether the object schema lists name as required.
func (s *Schema) IsRequired(name string) bool {
	if s == nil {
		return false
	}
	for _, r := range s.Required {
		if r == name {
			return true
		}
	}
	return false
}

// PropertyNames returns the object's property names in a stable order.
func (s *Schema) PropertyNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Param is a path, query or header parameter.
type Param struct {
	Name     string  `json:"name"`
	In       string  `json:"in"`
	Required bool    `json:"required,omitempty"`
	Schema   *Schema `json:"schema,omitempty"`
	Example  any     `json:"example,omitempty"`
}

// ResponseSpec is what the spec documents for one response status.
type ResponseSpec struct {
	Description string  `json:"description,omitempty"`
	ContentType string  `json:"contentType,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Sample is a real request seen in recorded traffic. Generated scenarios
// start from it instead of from synthetic values.
type Sample struct {
	PathValues map[string]string `json:"pathValues,omitempty"`
	Query      string            `json:"query,omitempty"`
	Body       string            `json:"body,omitempty"`
}

// Protocols other than plain HTTP. An Endpoint or Request with an empty
// Protocol is HTTP.
const (
	ProtoGRPC      = "grpc"      // Path is /package.Service/Method, Body is the request message as JSON
	ProtoGraphQL   = "graphql"   // an HTTP POST of {query, variables}; GraphQL describes the operation
	ProtoWebSocket = "websocket" // Path is upgraded; Body is the request message (or, in a Request, a JSON array of messages)
)

// GraphQLOp is one root field of a GraphQL schema, e.g. query user(id: ID!).
type GraphQLOp struct {
	Operation string            `json:"operation"`           // query, mutation or subscription
	Field     string            `json:"field"`               // the root field, e.g. user
	ArgTypes  map[string]string `json:"argTypes,omitempty"`  // argument name -> GraphQL type, e.g. ID!
	ArgOrder  []string          `json:"argOrder,omitempty"`  // arguments in schema order
	Returns   string            `json:"returns,omitempty"`   // the GraphQL return type, e.g. [User!]!
	Selection string            `json:"selection,omitempty"` // a selection set that works for the return type, e.g. { id name }
}

// Endpoint is one method on one path template, e.g. GET /users/{id}, or one
// gRPC method, GraphQL root field or WebSocket path.
type Endpoint struct {
	Protocol     string                   `json:"protocol,omitempty"`
	Method       string                   `json:"method"`
	Path         string                   `json:"path"`
	Params       []Param                  `json:"params,omitempty"`
	Body         *Schema                  `json:"body,omitempty"`
	BodyType     string                   `json:"bodyType,omitempty"`
	BodyRequired bool                     `json:"bodyRequired,omitempty"`
	Responses    map[string]*ResponseSpec `json:"responses,omitempty"`
	Secured      bool                     `json:"secured,omitempty"`
	AnyMethod    bool                     `json:"anyMethod,omitempty"`
	Inferred     bool                     `json:"inferred,omitempty"`
	Source       string                   `json:"source,omitempty"`
	Code         string                   `json:"code,omitempty"` // where the route is registered in the source, e.g. api/users.go:42
	Samples      []Sample                 `json:"samples,omitempty"`
	Rules        []string                 `json:"rules,omitempty"`   // behavior the handler enforces or should, from the API map
	Stream       string                   `json:"stream,omitempty"`  // gRPC: client, server or bidi
	GraphQL      *GraphQLOp               `json:"graphql,omitempty"` // GraphQL: the root field
}

// Key identifies the endpoint, e.g. "GET /users/{id}", "GRPC
// /users.v1.Users/GetUser", "QUERY user" or "WS /chat".
func (e *Endpoint) Key() string {
	if e.GraphQL != nil {
		return e.Method + " " + e.GraphQL.Field
	}
	return e.Method + " " + e.Path
}

// Label names the endpoint for people, e.g. "gRPC users.v1.Users/GetUser".
func (e *Endpoint) Label() string {
	switch e.Protocol {
	case ProtoGRPC:
		l := "gRPC " + strings.TrimPrefix(e.Path, "/")
		if e.Stream != "" {
			l += " (" + e.Stream + " streaming)"
		}
		return l
	case ProtoGraphQL:
		if e.GraphQL != nil {
			return "GraphQL " + e.GraphQL.Operation + " " + e.GraphQL.Field
		}
	case ProtoWebSocket:
		return "WebSocket " + e.Path
	}
	if e.AnyMethod {
		return "ANY " + e.Path
	}
	return e.Key()
}

// ParamsIn returns the parameters at one location (path, query or header).
func (e *Endpoint) ParamsIn(in string) []Param {
	var out []Param
	for _, p := range e.Params {
		if p.In == in {
			out = append(out, p)
		}
	}
	return out
}

// Drift is a difference between the routes in the code and the spec.
type Drift struct {
	Kind   string `json:"kind"` // "undocumented": in code, not in spec. "unimplemented": in spec, not found in code.
	Method string `json:"method"`
	Path   string `json:"path"`
	Where  string `json:"where,omitempty"`
}

// API is everything discovered about a service.
type API struct {
	Title       string      `json:"title,omitempty"`
	Sources     []string    `json:"sources,omitempty"`
	Endpoints   []*Endpoint `json:"endpoints"`
	AuthHeaders []string    `json:"authHeaders,omitempty"`
	Drift       []Drift     `json:"drift,omitempty"`
	Notes       []string    `json:"notes,omitempty"`
}

// Endpoint returns the endpoint with the given key, or nil.
func (a *API) Endpoint(key string) *Endpoint {
	if a == nil {
		return nil
	}
	for _, e := range a.Endpoints {
		if e.Key() == key {
			return e
		}
	}
	return nil
}

// Request is a concrete request. Path and Query are already escaped. For
// gRPC, Path is /package.Service/Method, Headers are metadata and Body is the
// request message as JSON (a JSON array of messages when Stream is client or
// bidi). For WebSocket, Body is a JSON array of the messages to send.
type Request struct {
	Protocol     string            `json:"protocol,omitempty"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Query        string            `json:"query,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	BodyEncoding string            `json:"bodyEncoding,omitempty"` // "" or "base64"
	Stream       string            `json:"stream,omitempty"`       // gRPC: client, server or bidi
	Receive      int               `json:"receive,omitempty"`      // WebSocket: messages to wait for
	WaitMS       int               `json:"waitMs,omitempty"`       // WebSocket: how long to wait for them
}

// BodyBytes returns the decoded body.
func (r Request) BodyBytes() []byte { return decode(r.Body, r.BodyEncoding) }

// Target names what the request goes to, e.g. "GET /users/1", "gRPC
// users.v1.Users/GetUser" or "WebSocket /chat".
func (r Request) Target() string {
	switch r.Protocol {
	case ProtoGRPC:
		return "gRPC " + strings.TrimPrefix(r.Path, "/")
	case ProtoWebSocket:
		return "WebSocket " + r.Path
	}
	p := r.Method + " " + r.Path
	if r.Query != "" {
		p += "?" + r.Query
	}
	return p
}

// EncodeBody stores bytes as text, falling back to base64 for binary data.
func EncodeBody(b []byte) (body, encoding string) {
	if utf8.Valid(b) {
		return string(b), ""
	}
	return base64.StdEncoding.EncodeToString(b), "base64"
}

func decode(body, encoding string) []byte {
	if encoding == "base64" {
		if b, err := base64.StdEncoding.DecodeString(body); err == nil {
			return b
		}
	}
	return []byte(body)
}

// Expect is what a scenario expects from the server.
type Expect string

const (
	ExpectSuccess  Expect = "success"  // valid input: a 2xx is expected
	ExpectReject   Expect = "reject"   // invalid input: a 4xx is expected
	ExpectNoCrash  Expect = "no-crash" // unusual but not invalid input: anything except a 5xx or a dropped connection
	ExpectRecorded Expect = "recorded" // replayed traffic: the recorded response is expected
)

// RecordedResponse is a response captured by the recording proxy.
type RecordedResponse struct {
	Status       int               `json:"status"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	BodyEncoding string            `json:"bodyEncoding,omitempty"`
}

// BodyBytes returns the decoded body.
func (r RecordedResponse) BodyBytes() []byte { return decode(r.Body, r.BodyEncoding) }

// Scenario categories, in the order they are generated and reported.
const (
	CatHappy       = "happy"
	CatAuth        = "auth"
	CatMissing     = "missing-required"
	CatWrongType   = "wrong-type"
	CatNull        = "null"
	CatBoundary    = "boundary"
	CatFormat      = "format"
	CatExtreme     = "extreme"
	CatBodyShape   = "body-shape"
	CatPathParam   = "path-param"
	CatQuery       = "query"
	CatHeaders     = "headers"
	CatMethod      = "method"
	CatConsistency = "consistency"
	CatReplay      = "replay"
)

// Categories lists every scenario category.
var Categories = []string{
	CatHappy, CatAuth, CatMissing, CatWrongType, CatNull, CatBoundary, CatFormat, CatExtreme,
	CatBodyShape, CatPathParam, CatQuery, CatHeaders, CatMethod, CatConsistency, CatReplay,
}

// Scenario is one request to send and what to expect back.
type Scenario struct {
	ID        string            `json:"id"`
	Endpoint  string            `json:"endpoint"`
	Category  string            `json:"category"`
	Name      string            `json:"name"`
	Request   Request           `json:"request"`
	Expect    Expect            `json:"expect"`
	Strict    bool              `json:"strict,omitempty"`    // breaking the expectation is an error, not a warning
	Repeat    int               `json:"repeat,omitempty"`    // send this many times and compare
	StripAuth bool              `json:"stripAuth,omitempty"` // leave out the credentials given with -H
	Recorded  *RecordedResponse `json:"recorded,omitempty"`
}

// Suite is a set of scenarios together with what they were generated from.
type Suite struct {
	API       *API       `json:"api"`
	Scenarios []Scenario `json:"scenarios"`
}

// Severity ranks a finding.
type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
	SevInfo    Severity = "info"
)

// Rank orders severities: error > warning > info.
func (s Severity) Rank() int {
	switch s {
	case SevError:
		return 3
	case SevWarning:
		return 2
	case SevInfo:
		return 1
	}
	return 0
}

// Finding is one problem noticed in a response.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Detail   string   `json:"detail,omitempty"`
}

// Observed is one response (or failure) from the server. For gRPC, Code is
// the status code name and Status its HTTP equivalent (NOT_FOUND is 404,
// INTERNAL is 500), so the HTTP checks apply; Body is the response message as
// JSON, or {code, message, details} for an error. For WebSocket, Status is the
// handshake status (101 once upgraded), Body a JSON array of the messages
// received, and Close the close code the server sent.
type Observed struct {
	Status     int               `json:"status,omitempty"`
	Code       string            `json:"code,omitempty"`
	Close      int               `json:"close,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"`
	DurationMS float64           `json:"durationMs"`
	Error      string            `json:"error,omitempty"`
	Timeout    bool              `json:"timeout,omitempty"`
	Unsent     string            `json:"unsent,omitempty"` // the request could not be built (e.g. a gRPC message that does not fit its type); nothing was sent
}

// StatusText shows the outcome: the gRPC code, the WebSocket close, or the
// HTTP status.
func (o Observed) StatusText() string {
	switch {
	case o.Code != "":
		return o.Code
	case o.Status == 0:
		return "no response"
	case o.Status == 101 && o.Close != 0:
		return "101, closed " + strconv.Itoa(o.Close)
	}
	return strconv.Itoa(o.Status)
}

// Result is a scenario together with what happened when it ran.
type Result struct {
	Scenario Scenario   `json:"scenario"`
	Observed []Observed `json:"observed"`
	Baseline *Observed  `json:"baseline,omitempty"`
	Findings []Finding  `json:"findings,omitempty"`
}

// Worst returns the highest severity among the findings.
func (r Result) Worst() Severity {
	var worst Severity
	for _, f := range r.Findings {
		if f.Severity.Rank() > worst.Rank() {
			worst = f.Severity
		}
	}
	return worst
}
