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

	"github.com/aryehlev/ReplayHarness/internal/grpcx"
	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Replay is a named sequence of requests.
type Replay struct {
	Name  string `json:"name" yaml:"name"`
	Why   string `json:"why,omitempty" yaml:"why,omitempty"` // the behavior being checked and where in the code
	Steps []Step `json:"steps" yaml:"steps"`
	File  string `json:"-" yaml:"-"`
}

// Step is one request and what its response must look like. An HTTP step
// has method and path; a gRPC step has grpc (and message or messages); a
// GraphQL step has graphql (and variables); a WebSocket step has websocket
// (and send).
type Step struct {
	Name    string            `json:"name,omitempty" yaml:"name,omitempty"`
	Method  string            `json:"method,omitempty" yaml:"method,omitempty"`
	Path    string            `json:"path,omitempty" yaml:"path,omitempty"` // may contain a query string and {{variables}}
	Query   map[string]any    `json:"query,omitempty" yaml:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"` // HTTP headers, or gRPC metadata
	JSON    any               `json:"json,omitempty" yaml:"json,omitempty"`       // body, sent as JSON
	Body    *string           `json:"body,omitempty" yaml:"body,omitempty"`       // raw body, sent as is
	NoAuth  bool              `json:"no_auth,omitempty" yaml:"no_auth,omitempty"`
	Expect  Expect            `json:"expect,omitempty" yaml:"expect,omitempty"`
	Save    map[string]string `json:"save,omitempty" yaml:"save,omitempty"` // variable -> $.json.path, header:Name or status

	GRPC     string `json:"grpc,omitempty" yaml:"grpc,omitempty"`         // package.Service/Method
	Message  any    `json:"message,omitempty" yaml:"message,omitempty"`   // the gRPC request message, as JSON
	Messages []any  `json:"messages,omitempty" yaml:"messages,omitempty"` // messages for a client or bidi streaming method

	GraphQL       string         `json:"graphql,omitempty" yaml:"graphql,omitempty"` // the query or mutation
	Variables     map[string]any `json:"variables,omitempty" yaml:"variables,omitempty"`
	OperationName string         `json:"operation_name,omitempty" yaml:"operation_name,omitempty"`

	WebSocket string `json:"websocket,omitempty" yaml:"websocket,omitempty"` // the path to upgrade
	Send      []any  `json:"send,omitempty" yaml:"send,omitempty"`           // messages to send; strings go as they are, anything else as JSON
	Receive   int    `json:"receive,omitempty" yaml:"receive,omitempty"`     // wait for this many messages (a step with fewer fails)
	WaitMS    int    `json:"wait_ms,omitempty" yaml:"wait_ms,omitempty"`     // how long to wait for messages
}

// Protocol returns the step's protocol: "" for HTTP, or grpc, graphql or
// websocket.
func (s Step) Protocol() string {
	switch {
	case s.GRPC != "":
		return model.ProtoGRPC
	case s.GraphQL != "":
		return model.ProtoGraphQL
	case s.WebSocket != "":
		return model.ProtoWebSocket
	}
	return ""
}

// Expect lists assertions on a response. With no status given, any status
// below 500 passes.
type Expect struct {
	Status      any               `json:"status,omitempty" yaml:"status,omitempty"` // 200, [200, 201], "2xx", "4xx"
	Code        any               `json:"code,omitempty" yaml:"code,omitempty"`     // gRPC: NOT_FOUND, [OK, NOT_FOUND] or 5
	Close       any               `json:"close,omitempty" yaml:"close,omitempty"`   // WebSocket: the close code the server sent, e.g. 1008 or [1000, 1001]
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
		if err := f.Steps[i].validate(); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Step) validate() error {
	kinds := 0
	for _, set := range []bool{s.GRPC != "", s.GraphQL != "", s.WebSocket != ""} {
		if set {
			kinds++
		}
	}
	if kinds > 1 {
		return fmt.Errorf("use one of grpc, graphql or websocket per step")
	}
	proto := s.Protocol()
	s.Method = strings.ToUpper(strings.TrimSpace(s.Method))
	if proto != model.ProtoGRPC && (s.Message != nil || len(s.Messages) > 0) {
		return fmt.Errorf("message and messages are for gRPC steps (grpc: package.Service/Method)")
	}
	if proto != model.ProtoGraphQL && (len(s.Variables) > 0 || s.OperationName != "") {
		return fmt.Errorf("variables and operation_name are for GraphQL steps (graphql: \"query { ... }\")")
	}
	if proto != model.ProtoWebSocket && (len(s.Send) > 0 || s.Receive != 0 || s.WaitMS != 0) {
		return fmt.Errorf("send, receive and wait_ms are for WebSocket steps (websocket: /path)")
	}
	if proto != model.ProtoGRPC && s.Expect.Code != nil {
		return fmt.Errorf("expect.code is for gRPC steps; use expect.status for HTTP")
	}
	if proto != model.ProtoWebSocket && s.Expect.Close != nil {
		return fmt.Errorf("expect.close is for WebSocket steps")
	}
	switch proto {
	case model.ProtoGRPC:
		if s.Method != "" || s.Path != "" || s.JSON != nil || s.Body != nil || len(s.Query) > 0 {
			return fmt.Errorf("a gRPC step takes grpc, message (or messages), headers and expect; not method, path, query, json or body")
		}
		if _, _, err := grpcx.SplitMethod(s.GRPC); err != nil {
			return err
		}
		if s.Message != nil && len(s.Messages) > 0 {
			return fmt.Errorf("use message or messages, not both")
		}
		if _, err := codeMatcher(s.Expect.Code); err != nil {
			return err
		}
	case model.ProtoGraphQL:
		if s.JSON != nil || s.Body != nil {
			return fmt.Errorf("a GraphQL step sends graphql and variables; leave out json and body")
		}
		if s.Method == "" {
			s.Method = "POST"
		}
		if s.Method != "POST" && s.Method != "GET" {
			return fmt.Errorf("a GraphQL step uses POST (the default) or GET")
		}
		if s.Path == "" {
			s.Path = "/graphql"
		}
	case model.ProtoWebSocket:
		if s.Method != "" || s.Path != "" || s.JSON != nil || s.Body != nil {
			return fmt.Errorf("a WebSocket step takes websocket (the path), query, headers, send, receive, wait_ms and expect")
		}
		if !strings.HasPrefix(s.WebSocket, "/") && !strings.HasPrefix(s.WebSocket, "{{") {
			return fmt.Errorf("websocket must be a path starting with / (the target server is set when ReplayHarness starts), got %q", s.WebSocket)
		}
		if s.Receive < 0 || s.WaitMS < 0 {
			return fmt.Errorf("receive and wait_ms cannot be negative")
		}
		if _, err := closeMatcher(s.Expect.Close); err != nil {
			return err
		}
	default:
		if s.Method == "" {
			return fmt.Errorf("method is required (or grpc, graphql or websocket for other protocols)")
		}
		if s.JSON != nil && s.Body != nil {
			return fmt.Errorf("use json or body, not both")
		}
	}
	if proto != model.ProtoGRPC && proto != model.ProtoWebSocket && !strings.HasPrefix(s.Path, "/") && !strings.HasPrefix(s.Path, "{{") {
		return fmt.Errorf("path must start with / (the target server is set when ReplayHarness starts), got %q", s.Path)
	}
	if _, err := statusMatcher(s.Expect.Status); err != nil {
		return err
	}
	for name, src := range s.Save {
		if src != "status" && src != "code" && !strings.HasPrefix(src, "$") && !strings.HasPrefix(strings.ToLower(src), "header:") {
			return fmt.Errorf("save %s: use a JSON path like $.id, header:Location, status or code", name)
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
