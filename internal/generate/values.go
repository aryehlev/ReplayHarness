package generate

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Valid returns a value that satisfies the schema, preferring the spec's own
// examples and defaults. name is the property name and steers string values
// (an "email" property gets an email address).
func Valid(s *model.Schema, name string) any { return valid(s, name, 0) }

func valid(s *model.Schema, name string, depth int) any {
	if s == nil {
		return "test"
	}
	if s.Example != nil {
		return s.Example
	}
	if s.Default != nil {
		return s.Default
	}
	for _, e := range s.Enum {
		if e != nil {
			return e
		}
	}
	switch s.Type {
	case "string":
		return validString(s, name)
	case "integer":
		return validNumber(s, true)
	case "number":
		return validNumber(s, false)
	case "boolean":
		return true
	case "array":
		n := 1
		if s.MinItems != nil && *s.MinItems > n {
			n = *s.MinItems
		}
		if s.MaxItems != nil && *s.MaxItems < n {
			n = *s.MaxItems
		}
		if depth > 5 {
			n = 0
		}
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			arr = append(arr, valid(s.Items, singularName(name), depth+1))
		}
		return arr
	case "object":
		obj := map[string]any{}
		if depth > 5 {
			return obj
		}
		for _, p := range s.PropertyNames() {
			obj[p] = valid(s.Properties[p], p, depth+1)
		}
		return obj
	}
	if len(s.Properties) > 0 {
		obj := map[string]any{}
		for _, p := range s.PropertyNames() {
			obj[p] = valid(s.Properties[p], p, depth+1)
		}
		return obj
	}
	return "test"
}

func validString(s *model.Schema, name string) string {
	lname := strings.ToLower(name)
	v := "test"
	switch s.Format {
	case "email":
		v = "user@example.com"
	case "uuid":
		v = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	case "date":
		v = "2024-01-15"
	case "date-time":
		v = "2024-01-15T10:30:00Z"
	case "time":
		v = "10:30:00"
	case "uri", "url", "iri":
		v = "https://example.com/resource"
	case "hostname":
		v = "example.com"
	case "ipv4":
		v = "192.0.2.10"
	case "ipv6":
		v = "2001:db8::1"
	case "byte":
		v = "aGVsbG8gd29ybGQ="
	case "password":
		v = "Sup3r-Secret!"
	default:
		switch {
		case strings.Contains(lname, "email"):
			v = "user@example.com"
		case strings.HasSuffix(lname, "url") || strings.HasSuffix(lname, "uri") || lname == "website":
			v = "https://example.com"
		case strings.Contains(lname, "phone"):
			v = "+15555550100"
		case strings.HasSuffix(lname, "date"):
			v = "2024-01-15"
		case lname == "name" || strings.HasSuffix(lname, "name"):
			v = "Test Name"
		case lname == "id" || strings.HasSuffix(lname, "id"):
			v = "1"
		}
	}
	if s.MinLength != nil && utf8.RuneCountInString(v) < *s.MinLength {
		v += strings.Repeat("x", *s.MinLength-utf8.RuneCountInString(v))
	}
	if s.MaxLength != nil && utf8.RuneCountInString(v) > *s.MaxLength {
		v = string([]rune(v)[:*s.MaxLength])
	}
	return v
}

func validNumber(s *model.Schema, integer bool) any {
	v := 1.0
	if s.Minimum != nil && (v < *s.Minimum || (s.ExclusiveMinimum && v <= *s.Minimum)) {
		v = *s.Minimum
		if s.ExclusiveMinimum {
			v++
		}
	}
	if s.Maximum != nil && (v > *s.Maximum || (s.ExclusiveMaximum && v >= *s.Maximum)) {
		v = *s.Maximum
		if s.ExclusiveMaximum {
			v--
		}
	}
	if integer {
		return int64(math.Ceil(v))
	}
	return v
}

func singularName(name string) string {
	if strings.HasSuffix(name, "s") {
		return strings.TrimSuffix(name, "s")
	}
	return name
}

// scalarString renders a value the way it appears in a path, query or form.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// show renders a value for a scenario name: short, quoted and printable.
func show(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		if n := utf8.RuneCountInString(t); n > 40 {
			return fmt.Sprintf("%d-char string", n)
		}
		return strconv.Quote(t)
	case json.Number:
		return t.String()
	case []any:
		if len(t) > 3 {
			return fmt.Sprintf("array of %d items", len(t))
		}
	case map[string]any:
		if len(t) == 0 {
			return "{}"
		}
		return "an object"
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) > 60 {
		return fmt.Sprintf("%T", v)
	}
	return string(b)
}

func typeName(s *model.Schema) string {
	if s == nil || s.Type == "" {
		return "any"
	}
	return s.Type
}

func formatNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func numValue(f float64, integer bool) any {
	if integer {
		return int64(f)
	}
	return f
}

// fillString makes a string of exactly n characters that still looks like
// the schema's format where that is possible.
func fillString(s *model.Schema, name string, n int) string {
	base := validString(&model.Schema{Format: s.Format}, name)
	if s.Format == "email" || strings.Contains(strings.ToLower(name), "email") {
		const domain = "@example.com"
		if n > len(domain) {
			return strings.Repeat("a", n-len(domain)) + domain
		}
	}
	if utf8.RuneCountInString(base) >= n {
		return string([]rune(base)[:n])
	}
	return base + strings.Repeat("x", n-utf8.RuneCountInString(base))
}

// fixedFormat reports formats whose valid values have a fixed shape, so
// length probes would just test the format.
func fixedFormat(f string) bool {
	switch f {
	case "uuid", "date", "date-time", "time", "ipv4", "ipv6", "byte":
		return true
	}
	return false
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		a := make([]any, len(t))
		for i, val := range t {
			a[i] = deepCopy(val)
		}
		return a
	}
	return v
}

// setAt sets (or deletes) the value at path inside root, which it modifies.
// Path elements are object keys (string) or array indexes (int). It reports
// false when the path does not exist.
func setAt(root any, path []any, value any, del bool) (any, bool) {
	if len(path) == 0 {
		if del {
			return root, false
		}
		return value, true
	}
	switch key := path[0].(type) {
	case string:
		m, ok := root.(map[string]any)
		if !ok {
			return root, false
		}
		if len(path) == 1 {
			if del {
				if _, ok := m[key]; !ok {
					return root, false
				}
				delete(m, key)
			} else {
				m[key] = value
			}
			return m, true
		}
		child, ok := m[key]
		if !ok {
			return root, false
		}
		nv, ok := setAt(child, path[1:], value, del)
		if !ok {
			return root, false
		}
		m[key] = nv
		return m, true
	case int:
		a, ok := root.([]any)
		if !ok || key >= len(a) {
			return root, false
		}
		if len(path) == 1 {
			if del {
				return root, false
			}
			a[key] = value
			return a, true
		}
		nv, ok := setAt(a[key], path[1:], value, del)
		if !ok {
			return root, false
		}
		a[key] = nv
		return a, true
	}
	return root, false
}

func pathString(path []any) string {
	var b strings.Builder
	for _, p := range path {
		switch t := p.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(t)
		case int:
			fmt.Fprintf(&b, "[%d]", t)
		}
	}
	return b.String()
}
