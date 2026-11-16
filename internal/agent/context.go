// Package agent renders what ReplayHarness knows about a service in a form a
// coding agent can act on: each endpoint's contract and where its handler is
// registered, so the agent knows which code to read before writing flows.
package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// FlowFormat is a short reference for writing flows, shown to agents.
const FlowFormat = `Flow format (YAML or JSON):

  flows:
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
Built-in variables: {{unique}} (random per flow run), {{timestamp}} (unix ms).
Without expect.status any status below 500 passes. A 5xx, a dropped connection,
a timeout or a leaked stack trace fails a step even when no assertion mentions it.`

// Context renders the API for an agent.
func Context(api *model.API, target string) string {
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
		fmt.Fprintf(&b, "Requests go to: %s (fixed; paths in flows start with /)\n", target)
	}
	b.WriteString("\n")
	for _, ep := range api.Endpoints {
		method := ep.Method
		if ep.AnyMethod {
			method = "ANY"
		}
		fmt.Fprintf(&b, "## %s %s\n", method, ep.Path)
		switch {
		case ep.Code != "":
			fmt.Fprintf(&b, "- handler registered at: %s\n", ep.Code)
		case ep.Inferred:
			fmt.Fprintf(&b, "- seen in: %s\n", ep.Source)
		default:
			fmt.Fprintf(&b, "- handler: not found in the code (documented in %s)\n", ep.Source)
		}
		if ep.Secured {
			b.WriteString("- auth: required by the spec\n")
		}
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
		if len(ep.Responses) > 0 {
			codes := make([]string, 0, len(ep.Responses))
			for c := range ep.Responses {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			var parts []string
			for _, c := range codes {
				rs := ep.Responses[c]
				if rs.Schema != nil {
					parts = append(parts, c+" "+Schema(rs.Schema))
				} else {
					parts = append(parts, c)
				}
			}
			fmt.Fprintf(&b, "- responses: %s\n", strings.Join(parts, "; "))
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
				fmt.Fprintf(&b, "- %s %s is in the code (%s) but not in the spec\n", d.Method, d.Path, d.Where)
			} else {
				fmt.Fprintf(&b, "- %s %s is in the spec but no route was found in the code\n", d.Method, d.Path)
			}
		}
		b.WriteString("\n")
	}
	for _, n := range api.Notes {
		fmt.Fprintf(&b, "Note: %s\n", n)
	}
	return b.String()
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
		for _, n := range s.PropertyNames() {
			name := n
			if s.IsRequired(n) {
				name += "*"
			}
			fields = append(fields, name+": "+schema(s.Properties[n], depth+1))
		}
		out = "{" + strings.Join(fields, ", ") + "}"
		if len(fields) == 0 {
			out = "object"
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
		out = s.Type + bounds(s.Minimum, s.Maximum, "")
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
