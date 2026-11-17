// Package agent renders what ReplayHarness knows about a service in a form a
// coding agent can act on: each endpoint's contract and where its handler is
// registered, so the agent knows which code to read before writing replays.
package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/gql"
	"github.com/aryehlev/ReplayHarness/internal/model"
)

// ReplayFormat is a short reference for writing replays, shown to agents.
const ReplayFormat = `Replay format (YAML or JSON):

  replays:
    - name: a deleted user is gone            # the behavior, stated as a fact
      why: deleteUser returns 204 without removing the row (api/users.go:88)
      steps:
        - method: POST
          path: /users
          json: {name: Ada, email: "ada-{{unique}}@example.com"}
          expect: {status: 201, json: {$.name: Ada, $.id: "<integer>"}}
          save: {id: $.id}                    # later steps can use {{id}}
        - method: DELETE
          path: /users/{{id}}
          expect: {status: 204}
        - method: GET
          path: /users/{{id}}
          expect: {status: 404}

Step fields: method, path (may include ?query), query, headers, json (a JSON body)
or body (a raw body), no_auth (leave out the credentials ReplayHarness was given),
expect, save.
expect: status (404, [200, 201], "4xx"); json ($.path: value, or a matcher:
<exists> <absent> <null> <string> <number> <integer> <bool> <array> <object>
<nonempty>; $.items.length works); headers (name: substring, <exists>, <absent>);
contains / not_contains (body substrings); max_ms.
save: variable: $.json.path, header:Location or status. Saved values work in
paths, query values, headers, bodies and expected values ($.id: "{{id}}"); a
path may be just a saved path (path: "{{location}}").
Built-in variables: {{unique}} (random per replay run), {{timestamp}} (unix ms).
Without expect.status any status below 500 passes. A 5xx, a dropped connection,
a timeout or a leaked stack trace fails a step even when no assertion mentions it.

Other protocols: a step has grpc, graphql or websocket instead of method and path.

  gRPC:
    - grpc: users.v1.Users/GetUser
      message: {id: "{{id}}"}              # the request as JSON, field names as in the .proto
      headers: {authorization: "Bearer x"} # metadata
      expect: {code: NOT_FOUND, json: {$.message: "<string>"}}
  messages: [{...}, {...}] sends a client stream; a server stream's reply is a JSON
  array. expect.code takes OK, INVALID_ARGUMENT, NOT_FOUND, ... or a list. An error's
  body is {code, message, details}. int64 fields come back as strings ("42"), the
  way protojson writes them. save: {c: code} keeps the code.

  GraphQL:
    - graphql: "query ($id: ID!) { user(id: $id) { id email } }"
      variables: {id: "{{id}}"}
      path: /graphql                       # the default; method POST (or GET)
      expect: {json: {$.data.user.email: "<string>"}}
  A response with errors fails the step unless expect.json mentions $.errors, e.g.
  {"$.errors[0].message": "<string>", $.data.user: "<null>"}. In YAML flow maps,
  quote keys that contain [ ].

  WebSocket:
    - websocket: /ws/chat?room=1
      send: [{type: join}, "plain text"]   # strings are sent as they are, the rest as JSON
      receive: 2                           # wait for 2 messages; fewer fails the step
      wait_ms: 1000                        # without receive, collect for this long (default 1000)
      expect: {status: 101, json: {"$[0].type": joined, $.length: 2}}
  The body is the JSON array of messages received. expect.close is the close code
  the server sent (1008, 4401, ...); expect.status is the handshake status (101
  once upgraded, 401 if refused).

Built-in failures also cover gRPC INTERNAL, UNKNOWN and DATA_LOSS, GraphQL errors
that look like crashes ("internal system error", a stack trace, "index out of
range"), and a WebSocket closed with 1011 or dropped without a close frame.`

// Context renders the API for an agent. grpcAddr is where gRPC calls go,
// if the API has gRPC methods.
func Context(api *model.API, target, grpcAddr string) string {
	var b strings.Builder
	title := api.Title
	if title == "" {
		title = "API"
	}
	fmt.Fprintf(&b, "# %s: %d endpoint(s)\n\n", title, len(api.Endpoints))
	if len(api.Sources) > 0 {
		fmt.Fprintf(&b, "Discovered from: %s\n", strings.Join(api.Sources, ", "))
	}
	if target != "" {
		fmt.Fprintf(&b, "Requests go to: %s (fixed; paths in replays start with /)\n", target)
	}
	if grpcAddr != "" && hasProtocol(api, model.ProtoGRPC) {
		fmt.Fprintf(&b, "gRPC calls go to: %s\n", grpcAddr)
	}
	b.WriteString("\n")
	for _, ep := range api.Endpoints {
		switch ep.Protocol {
		case model.ProtoGRPC:
			fmt.Fprintf(&b, "## %s\n", ep.Label())
		case model.ProtoGraphQL:
			sig := ep.GraphQL.Field
			if len(ep.GraphQL.ArgOrder) > 0 {
				var args []string
				for _, a := range ep.GraphQL.ArgOrder {
					args = append(args, a+": "+ep.GraphQL.ArgTypes[a])
				}
				sig += "(" + strings.Join(args, ", ") + ")"
			}
			if ep.GraphQL.Returns != "" {
				sig += ": " + ep.GraphQL.Returns
			}
			fmt.Fprintf(&b, "## GraphQL %s %s (POST %s)\n", ep.GraphQL.Operation, sig, ep.Path)
		default:
			fmt.Fprintf(&b, "## %s\n", ep.Label())
		}
		switch {
		case ep.Code != "":
			fmt.Fprintf(&b, "- handler registered at: %s\n", ep.Code)
		case ep.Inferred && strings.HasSuffix(ep.Source, "routes in source code"):
			fmt.Fprintf(&b, "- found in the code by pattern matching (%s)\n", ep.Source)
		case ep.Inferred:
			fmt.Fprintf(&b, "- seen in: %s\n", ep.Source)
		case ep.Protocol != "" && ep.Source != "":
			fmt.Fprintf(&b, "- defined at %s; handler not located yet (read the code and add it to the API map)\n", ep.Source)
		default:
			fmt.Fprintf(&b, "- handler: not found in the code (documented in %s)\n", ep.Source)
		}
		if ep.Secured {
			b.WriteString("- auth: required\n")
		}
		switch ep.Protocol {
		case model.ProtoGRPC:
			if ep.Body != nil {
				fmt.Fprintf(&b, "- request %s: %s\n", typeName(ep.Body), Schema(ep.Body))
			}
			if ok := ep.Responses["OK"]; ok != nil && ok.Schema != nil {
				fmt.Fprintf(&b, "- response %s: %s\n", typeName(ok.Schema), Schema(ok.Schema))
			}
		case model.ProtoGraphQL:
			if ep.Body != nil && len(ep.Body.Properties) > 0 {
				fmt.Fprintf(&b, "- variables: %s\n", Schema(ep.Body))
			}
			fmt.Fprintf(&b, "- query: %s\n", gql.Query(ep.GraphQL))
		case model.ProtoWebSocket:
			if ep.Body != nil {
				fmt.Fprintf(&b, "- message: %s\n", Schema(ep.Body))
			}
		default:
			for _, p := range ep.Params {
				req := ""
				if p.Required {
					req = ", required"
				}
				fmt.Fprintf(&b, "- %s param %s: %s%s\n", p.In, p.Name, Schema(p.Schema), req)
			}
			if ep.Body != nil || ep.BodyType != "" {
				req := ""
				if ep.BodyRequired {
					req = ", required"
				}
				body := "shape unknown (read the handler)"
				if ep.Body != nil {
					body = Schema(ep.Body)
				}
				fmt.Fprintf(&b, "- body (%s%s): %s\n", ep.BodyType, req, body)
			}
		}
		if len(ep.Responses) > 0 && ep.Protocol != model.ProtoGRPC && ep.Protocol != model.ProtoGraphQL {
			codes := make([]string, 0, len(ep.Responses))
			for c := range ep.Responses {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			var parts []string
			for _, c := range codes {
				rs := ep.Responses[c]
				part := c
				if rs.Schema != nil {
					part += " " + Schema(rs.Schema)
				}
				if rs.Description != "" && rs.Schema == nil {
					part += " (" + rs.Description + ")"
				}
				parts = append(parts, part)
			}
			fmt.Fprintf(&b, "- responses: %s\n", strings.Join(parts, "; "))
		}
		for _, r := range ep.Rules {
			fmt.Fprintf(&b, "- rule: %s\n", r)
		}
		if len(ep.Samples) > 0 {
			s := ep.Samples[0]
			sample := fillPath(ep.Path, s.PathValues)
			if s.Query != "" {
				sample += "?" + s.Query
			}
			if s.Body != "" {
				body := s.Body
				if len(body) > 200 {
					body = body[:200] + "…"
				}
				sample += " " + body
			}
			fmt.Fprintf(&b, "- recorded example: %s\n", sample)
		}
		b.WriteString("\n")
	}
	if len(api.Drift) > 0 {
		b.WriteString("## Spec drift\n")
		for _, d := range api.Drift {
			if d.Kind == "undocumented" {
				fmt.Fprintf(&b, "- %s %s is registered in the code (%s) but neither the spec nor the API map lists it\n", d.Method, d.Path, d.Where)
			} else {
				fmt.Fprintf(&b, "- %s %s is listed in %s, but no route for it was found in the code by pattern matching\n", d.Method, d.Path, d.Where)
			}
		}
		b.WriteString("\n")
	}
	for _, n := range api.Notes {
		fmt.Fprintf(&b, "Note: %s\n", n)
	}
	return b.String()
}

func hasProtocol(api *model.API, proto string) bool {
	for _, ep := range api.Endpoints {
		if ep.Protocol == proto {
			return true
		}
	}
	return false
}

func typeName(s *model.Schema) string {
	if s.TypeName != "" {
		return s.TypeName
	}
	return "message"
}

func fillPath(tmpl string, values map[string]string) string {
	for k, v := range values {
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", v)
	}
	return tmpl
}

// Schema renders a schema compactly, e.g. {name*: string(1-50), age: integer(0-150)}.
func Schema(s *model.Schema) string { return schema(s, 0) }

func schema(s *model.Schema, depth int) string {
	if s == nil {
		return "any"
	}
	var out string
	switch {
	case len(s.Enum) > 0:
		parts := make([]string, len(s.Enum))
		for i, e := range s.Enum {
			parts[i] = fmt.Sprint(e)
		}
		out = "one of " + strings.Join(parts, "|")
	case s.Type == "object" || (s.Type == "" && len(s.Properties) > 0):
		if depth > 2 {
			out = "{…}"
			break
		}
		var fields []string
		inGroup := map[string]bool{}
		for _, g := range s.Exclusive {
			for _, n := range g {
				inGroup[n] = true
			}
		}
		for _, n := range s.PropertyNames() {
			if inGroup[n] {
				continue
			}
			name := n
			if s.IsRequired(n) {
				name += "*"
			}
			fields = append(fields, name+": "+schema(s.Properties[n], depth+1))
		}
		for _, g := range s.Exclusive {
			var alts []string
			for _, n := range g {
				alts = append(alts, n+": "+schema(s.Properties[n], depth+1))
			}
			fields = append(fields, "one of ("+strings.Join(alts, " | ")+")")
		}
		out = "{" + strings.Join(fields, ", ") + "}"
		if len(fields) == 0 {
			out = "object"
			if s.TypeName != "" && depth > 0 {
				out = s.TypeName
			}
		}
	case s.Type == "array":
		out = "[" + schema(s.Items, depth+1) + "]" + bounds(intf(s.MinItems), intf(s.MaxItems), " items")
	case s.Type == "string":
		out = "string"
		if s.Format != "" {
			out += "<" + s.Format + ">"
		}
		out += bounds(intf(s.MinLength), intf(s.MaxLength), " chars")
		if s.Pattern != "" {
			out += " matching " + s.Pattern
		}
	case s.Type == "integer" || s.Type == "number":
		out = s.Type
		if s.Format != "" {
			out += "<" + s.Format + ">"
		}
		out += bounds(s.Minimum, s.Maximum, "")
	case s.Type == "":
		out = "any"
	default:
		out = s.Type
	}
	if s.Nullable {
		out += "|null"
	}
	return out
}

func intf(i *int) *float64 {
	if i == nil {
		return nil
	}
	f := float64(*i)
	return &f
}

func bounds(min, max *float64, unit string) string {
	switch {
	case min != nil && max != nil:
		return fmt.Sprintf("(%g-%g%s)", *min, *max, unit)
	case min != nil:
		return fmt.Sprintf("(≥%g%s)", *min, unit)
	case max != nil:
		return fmt.Sprintf("(≤%g%s)", *max, unit)
	}
	return ""
}
