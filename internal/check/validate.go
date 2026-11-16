package check

import (
	"fmt"
	"math"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Validate checks a decoded JSON value against a schema: types, required
// properties, enums and nullability. It returns at most 10 problems.
func Validate(s *model.Schema, v any) []string {
	var errs []string
	validate(s, v, "$", &errs, 0)
	return errs
}

func validate(s *model.Schema, v any, path string, errs *[]string, depth int) {
	if s == nil || len(*errs) >= 10 || depth > 32 {
		return
	}
	if v == nil {
		if !s.Nullable && s.Type != "" {
			*errs = append(*errs, fmt.Sprintf("%s is null, expected %s", path, article(s.Type)))
		}
		return
	}
	if len(s.Enum) > 0 && !enumContains(s.Enum, v) {
		*errs = append(*errs, fmt.Sprintf("%s is not one of the allowed values", path))
	}
	mismatch := func() {
		*errs = append(*errs, fmt.Sprintf("%s is %s, expected %s", path, jsonType(v), article(s.Type)))
	}
	switch s.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			mismatch()
			return
		}
		for _, r := range s.Required {
			if _, ok := m[r]; !ok {
				*errs = append(*errs, fmt.Sprintf("%s.%s is missing (required)", path, r))
			}
		}
		for _, name := range s.PropertyNames() {
			if val, ok := m[name]; ok {
				validate(s.Properties[name], val, path+"."+name, errs, depth+1)
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			mismatch()
			return
		}
		for i, item := range a {
			if i >= 50 {
				break
			}
			validate(s.Items, item, fmt.Sprintf("%s[%d]", path, i), errs, depth+1)
		}
	case "string":
		if _, ok := v.(string); !ok {
			mismatch()
		}
	case "integer":
		if f, ok := v.(float64); !ok || f != math.Trunc(f) {
			mismatch()
		}
	case "number":
		if _, ok := v.(float64); !ok {
			mismatch()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			mismatch()
		}
	}
}

func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

func article(t string) string {
	switch t {
	case "integer", "object", "array":
		return "an " + t
	case "":
		return "a value"
	}
	return "a " + t
}
