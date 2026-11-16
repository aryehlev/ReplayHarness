// Package replay runs request replays: short, ordered lists of requests with
// expectations. A coding agent writes them after reading the handlers, to
// state what the code should do ("a deleted user is gone", "creating a user
// without an email is rejected"). Values from one response, such as a new
// id, can be saved and used by later steps.
//
// A replay file is YAML or JSON:
//
//	replays:
//	  - name: a deleted user is gone
//	    why: deleteUser answers 204 without removing anything (api/users.go:88)
//	    steps:
//	      - method: POST
//	        path: /users
//	        json: {name: Ada, email: "ada-{{unique}}@example.com"}
//	        expect: {status: 201}
//	        save: {id: $.id}
//	      - {method: DELETE, path: "/users/{{id}}", expect: {status: 204}}
//	      - {method: GET, path: "/users/{{id}}", expect: {status: 404}}
package replay

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Replay is a named sequence of requests.
type Replay struct {
	Name  string `json:"name" yaml:"name"`
	Why   string `json:"why,omitempty" yaml:"why,omitempty"` // the behavior being checked and where in the code
	Steps []Step `json:"steps" yaml:"steps"`
	File  string `json:"-" yaml:"-"`
}

// Step is one request and what its response must look like.
type Step struct {
	Name    string            `json:"name,omitempty" yaml:"name,omitempty"`
	Method  string            `json:"method" yaml:"method"`
	Path    string            `json:"path" yaml:"path"` // may contain a query string and {{variables}}
	Query   map[string]any    `json:"query,omitempty" yaml:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	JSON    any               `json:"json,omitempty" yaml:"json,omitempty"` // body, sent as JSON
	Body    *string           `json:"body,omitempty" yaml:"body,omitempty"` // raw body, sent as is
	NoAuth  bool              `json:"no_auth,omitempty" yaml:"no_auth,omitempty"`
	Expect  Expect            `json:"expect,omitempty" yaml:"expect,omitempty"`
	Save    map[string]string `json:"save,omitempty" yaml:"save,omitempty"` // variable -> $.json.path, header:Name or status
}

// Expect lists assertions on a response. With no status given, any status
// below 500 passes.
type Expect struct {
	Status      any               `json:"status,omitempty" yaml:"status,omitempty"` // 200, [200, 201], "2xx", "4xx"
	JSON        map[string]any    `json:"json,omitempty" yaml:"json,omitempty"`     // $.path -> value or matcher
	Headers     map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Contains    []string          `json:"contains,omitempty" yaml:"contains,omitempty"`
	NotContains []string          `json:"not_contains,omitempty" yaml:"not_contains,omitempty"`
	MaxMS       float64           `json:"max_ms,omitempty" yaml:"max_ms,omitempty"`
}

// Parse reads replays from YAML or JSON. It accepts {replays: [...]}, a list of
// replays, or a single replay.
func Parse(data []byte) ([]Replay, error) {
	trimmed := bytes.TrimSpace(data)
	var doc any
	var err error
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		err = json.Unmarshal(trimmed, &doc)
	} else {
		err = yaml.Unmarshal(trimmed, &doc)
	}
	if err != nil {
		return nil, err
	}
	doc = normalize(doc)
	var list any
	switch t := doc.(type) {
	case map[string]any:
		if fl, ok := t["replays"]; ok {
			list = fl
		} else if fl, ok := t["flows"]; ok {
			list = fl
		} else {
			list = []any{t}
		}
	case []any:
		list = t
	default:
		return nil, fmt.Errorf("expected replays: [...], a list of replays or one replay")
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	var replays []Replay
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&replays); err != nil {
		return nil, fmt.Errorf("invalid replay: %w", err)
	}
	for i := range replays {
		if err := replays[i].validate(); err != nil {
			name := replays[i].Name
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			return nil, fmt.Errorf("replay %s: %w", name, err)
		}
	}
	return replays, nil
}

func (f *Replay) validate() error {
	if strings.TrimSpace(f.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}
	for i := range f.Steps {
		s := &f.Steps[i]
		s.Method = strings.ToUpper(strings.TrimSpace(s.Method))
		if s.Method == "" {
			return fmt.Errorf("step %d: method is required", i+1)
		}
		if !strings.HasPrefix(s.Path, "/") && !strings.HasPrefix(s.Path, "{{") {
			return fmt.Errorf("step %d: path must start with / (the target server is set when ReplayHarness starts), got %q", i+1, s.Path)
		}
		if s.JSON != nil && s.Body != nil {
			return fmt.Errorf("step %d: use json or body, not both", i+1)
		}
		if _, err := statusMatcher(s.Expect.Status); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		for name, src := range s.Save {
			if src != "status" && !strings.HasPrefix(src, "$") && !strings.HasPrefix(strings.ToLower(src), "header:") {
				return fmt.Errorf("step %d: save %s: use a JSON path like $.id, header:Location or status", i+1, name)
			}
		}
	}
	return nil
}

// Load reads one replay file, or every .yaml, .yml and .json file in a
// directory (sorted by name).
func Load(path string) ([]Replay, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	files := []string{path}
	if info.IsDir() {
		files = nil
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			switch strings.ToLower(filepath.Ext(e.Name())) {
			case ".yaml", ".yml", ".json":
				if !e.IsDir() {
					files = append(files, filepath.Join(path, e.Name()))
				}
			}
		}
		sort.Strings(files)
		if len(files) == 0 {
			return nil, fmt.Errorf("no .yaml, .yml or .json replay files in %s", path)
		}
	}
	var all []Replay
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		replays, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for i := range replays {
			replays[i].File = f
		}
		all = append(all, replays...)
	}
	return all, nil
}

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

func uniqueID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
