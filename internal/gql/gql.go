// Package gql reads GraphQL schemas (SDL files or an introspection result),
// turns each root field into an endpoint, builds queries for them, and reads
// GraphQL errors out of responses. GraphQL servers usually answer 200 even
// when a resolver crashed; Errors finds the errors that are really server
// failures.
package gql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Schema is the part of a GraphQL schema ReplayHarness needs.
type Schema struct {
	Types        map[string]*ast.Definition
	Query        string
	Mutation     string
	Subscription string
	where        map[string]string // "Query.user" -> file:line
}

var builtinScalars = map[string]string{"Int": "integer", "Float": "number", "String": "string", "Boolean": "boolean", "ID": "string"}

// ParseSDL reads schema files. Files that are not schemas (operation
// documents) or do not parse are skipped with a note.
func ParseSDL(sources []*ast.Source) (*Schema, []string) {
	doc := &ast.SchemaDocument{}
	var notes []string
	for _, src := range sources {
		d, err := parser.ParseSchema(src)
		if err != nil {
			if looksLikeSchema(src.Input) {
				notes = append(notes, fmt.Sprintf("skipped GraphQL schema %s: %v", src.Name, err))
			}
			continue
		}
		doc.Merge(d)
	}
	s := &Schema{Types: map[string]*ast.Definition{}, where: map[string]string{}}
	for _, d := range doc.Definitions {
		cp := *d
		s.Types[d.Name] = &cp
	}
	for _, ext := range doc.Extensions {
		d := s.Types[ext.Name]
		if d == nil {
			cp := *ext
			s.Types[ext.Name] = &cp
			continue
		}
		d.Fields = append(d.Fields, ext.Fields...)
		d.EnumValues = append(d.EnumValues, ext.EnumValues...)
		d.Types = append(d.Types, ext.Types...)
	}
	s.Query, s.Mutation, s.Subscription = "Query", "Mutation", "Subscription"
	for _, list := range []ast.SchemaDefinitionList{doc.Schema, doc.SchemaExtension} {
		for _, sd := range list {
			for _, ot := range sd.OperationTypes {
				switch ot.Operation {
				case ast.Query:
					s.Query = ot.Type
				case ast.Mutation:
					s.Mutation = ot.Type
				case ast.Subscription:
					s.Subscription = ot.Type
				}
			}
		}
	}
	for _, root := range []string{s.Query, s.Mutation, s.Subscription} {
		if d := s.Types[root]; d != nil {
			for _, f := range d.Fields {
				if f.Position != nil && f.Position.Src != nil {
					s.where[root+"."+f.Name] = fmt.Sprintf("%s:%d", f.Position.Src.Name, f.Position.Line)
				}
			}
		}
	}
	if len(s.Types) == 0 {
		return nil, notes
	}
	return s, notes
}

var schemaHintRe = regexp.MustCompile(`(?m)^\s*(?:extend\s+)?(?:type|schema|input|enum|interface|union|scalar)\b`)

func looksLikeSchema(src string) bool { return schemaHintRe.MatchString(src) }

// HasRoots reports whether the schema defines a query or mutation type.
func (s *Schema) HasRoots() bool {
	return s != nil && (s.Types[s.Query] != nil || s.Types[s.Mutation] != nil)
}

// Endpoints returns one endpoint per root field, served at path.
func (s *Schema) Endpoints(path string) []*model.Endpoint {
	var out []*model.Endpoint
	for _, op := range []struct{ name, root string }{{"query", s.Query}, {"mutation", s.Mutation}, {"subscription", s.Subscription}} {
		d := s.Types[op.root]
		if d == nil {
			continue
		}
		for _, f := range d.Fields {
			if strings.HasPrefix(f.Name, "__") {
				continue
			}
			gop := &model.GraphQLOp{Operation: op.name, Field: f.Name, ArgTypes: map[string]string{}, Returns: f.Type.String(), Selection: s.Selection(f.Type.Name())}
			body := &model.Schema{Type: "object", Properties: map[string]*model.Schema{}}
			for _, a := range f.Arguments {
				gop.ArgTypes[a.Name] = a.Type.String()
				gop.ArgOrder = append(gop.ArgOrder, a.Name)
				body.Properties[a.Name] = s.inputSchema(a.Type, 0)
				if a.Type.NonNull && a.DefaultValue == nil {
					body.Required = append(body.Required, a.Name)
				}
			}
			ep := &model.Endpoint{
				Protocol: model.ProtoGraphQL, Method: strings.ToUpper(op.name), Path: path, GraphQL: gop,
				Body: body, BodyType: "application/json", BodyRequired: true,
				Responses: map[string]*model.ResponseSpec{"200": {Description: f.Type.String()}},
				Source:    s.where[op.root+"."+f.Name],
			}
			out = append(out, ep)
		}
	}
	return out
}

func (s *Schema) inputSchema(t *ast.Type, depth int) *model.Schema {
	if t.Elem != nil {
		return &model.Schema{Type: "array", Items: s.inputSchema(t.Elem, depth+1), Nullable: !t.NonNull, TypeName: t.String()}
	}
	out := &model.Schema{Nullable: !t.NonNull, TypeName: t.NamedType}
	if js, ok := builtinScalars[t.NamedType]; ok {
		out.Type = js
		return out
	}
	d := s.Types[t.NamedType]
	switch {
	case d == nil || d.Kind == ast.Scalar:
		out.Type = "string"
		if strings.Contains(strings.ToLower(t.NamedType), "datetime") {
			out.Format = "date-time"
		}
	case d.Kind == ast.Enum:
		out.Type = "string"
		for _, v := range d.EnumValues {
			out.Enum = append(out.Enum, v.Name)
		}
	case d.Kind == ast.InputObject:
		out.Type = "object"
		out.Properties = map[string]*model.Schema{}
		if depth > 4 {
			return out
		}
		for _, f := range d.Fields {
			out.Properties[f.Name] = s.inputSchema(f.Type, depth+1)
			if f.Type.NonNull && f.DefaultValue == nil {
				out.Required = append(out.Required, f.Name)
			}
		}
		sort.Strings(out.Required)
	default:
		out.Type = "object"
	}
	return out
}

func (s *Schema) isLeaf(name string) bool {
	if _, ok := builtinScalars[name]; ok {
		return true
	}
	d := s.Types[name]
	return d == nil || d.Kind == ast.Scalar || d.Kind == ast.Enum
}

// Selection returns a selection set for a type: its scalar fields, and the
// scalar fields of the objects it holds, one level down. Leaf types need
// none.
func (s *Schema) Selection(typeName string) string { return s.selection(typeName, 0) }

func (s *Schema) selection(typeName string, depth int) string {
	if s.isLeaf(typeName) {
		return ""
	}
	d := s.Types[typeName]
	if d.Kind == ast.Union {
		return "{ __typename }"
	}
	var parts []string
	for _, f := range d.Fields {
		if len(parts) >= 15 {
			break
		}
		if strings.HasPrefix(f.Name, "__") || hasRequiredArgs(f) {
			continue
		}
		if s.isLeaf(f.Type.Name()) {
			parts = append(parts, f.Name)
			continue
		}
		if depth < 1 {
			if sub := s.selection(f.Type.Name(), depth+1); sub != "" && sub != "{ __typename }" {
				parts = append(parts, f.Name+" "+sub)
			}
		}
	}
	if len(parts) == 0 {
		return "{ __typename }"
	}
	return "{ " + strings.Join(parts, " ") + " }"
}

func hasRequiredArgs(f *ast.FieldDefinition) bool {
	for _, a := range f.Arguments {
		if a.Type.NonNull && a.DefaultValue == nil {
			return true
		}
	}
	return false
}

// Query builds an operation that calls the endpoint's field with every
// argument bound to a variable of the same name.
func Query(op *model.GraphQLOp) string {
	var vars, args []string
	for _, a := range op.ArgOrder {
		vars = append(vars, "$"+a+": "+op.ArgTypes[a])
		args = append(args, a+": $"+a)
	}
	q := op.Operation + " ReplayHarness"
	if len(vars) > 0 {
		q += "(" + strings.Join(vars, ", ") + ")"
	}
	q += " { " + op.Field
	if len(args) > 0 {
		q += "(" + strings.Join(args, ", ") + ")"
	}
	if op.Selection != "" {
		q += " " + op.Selection
	}
	return q + " }"
}

// Envelope is the JSON body of a GraphQL request.
func Envelope(query string, variables any, operationName string) string {
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	if operationName != "" {
		body["operationName"] = operationName
	}
	b, _ := json.Marshal(body)
	return string(b)
}

var internalRe = regexp.MustCompile(`(?i)internal (server |system )?error|unexpected error|\bpanic|runtime error|nil pointer|null ?pointer|index out of range|out of bounds|cannot read propert(y|ies) of (undefined|null)|undefined is not|is not a function|traceback|exception|segmentation|stack trace|SQLSTATE|syntax error at or near`)

var internalCodes = map[string]bool{"INTERNAL_SERVER_ERROR": true, "INTERNAL_ERROR": true, "INTERNAL": true, "SERVER_ERROR": true}

// Errors reads the errors of a GraphQL response. internal is set when one
// of them looks like a server failure rather than a rejected request: an
// INTERNAL_SERVER_ERROR code, a stack trace, or a message such as "internal
// system error" or "runtime error: index out of range". ok is false when the
// body is not a GraphQL response.
func Errors(body string) (messages []string, internal []string, ok bool) {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal([]byte(body), &resp) != nil || (resp.Data == nil && resp.Errors == nil) {
		return nil, nil, false
	}
	for _, e := range resp.Errors {
		messages = append(messages, e.Message)
		code, _ := e.Extensions["code"].(string)
		_, trace := e.Extensions["stacktrace"]
		if exc, ok := e.Extensions["exception"].(map[string]any); ok {
			_, trace = exc["stacktrace"]
		}
		if internalCodes[strings.ToUpper(code)] || trace || internalRe.MatchString(e.Message) {
			internal = append(internal, e.Message)
		}
	}
	return messages, internal, true
}
