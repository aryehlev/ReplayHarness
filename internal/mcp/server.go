// Package mcp serves ReplayHarness to coding agents over the Model Context
// Protocol (JSON-RPC 2.0 over stdio). The agent reads the code and decides
// what to check; these tools discover the endpoints, send requests, run
// replays and run the built-in probes. Every request goes to the one target
// fixed when the server starts.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aryehlev/ReplayHarness/internal/agent"
	"github.com/aryehlev/ReplayHarness/internal/check"
	"github.com/aryehlev/ReplayHarness/internal/discover"
	"github.com/aryehlev/ReplayHarness/internal/generate"
	"github.com/aryehlev/ReplayHarness/internal/model"
	"github.com/aryehlev/ReplayHarness/internal/replay"
	"github.com/aryehlev/ReplayHarness/internal/report"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

// Server holds what the tools need.
type Server struct {
	Version string
	Source  string        // default discovery source: a repo, a spec file or recorded traffic; "" fetches the spec from the target
	Config  runner.Config // target, headers, rate limit and timeout for every request
	Slow    time.Duration

	client *runner.Client
	mu     sync.Mutex
	api    *model.API // last discovery, reused by probe
	out    sync.Mutex
}

// Instructions are sent to the agent when it connects.
const Instructions = `ReplayHarness finds bugs in the HTTP service in this project by sending real requests to a running local server.
It works in two steps:
1. Discovery: read the code and write .replayharness/api.yaml, the API map: every route with its handler file:line, parameters, an example body, required fields, auth and the rules the handler enforces. No OpenAPI spec is needed. discover shows what is known so far and validates the map.
2. Replays: read the map and the handlers, write replays (multi-step requests with expectations) to .replayharness/replays/*.yaml, run them with run_replays, and check each failure against the code before reporting it. probe gives a quick baseline of built-in requests first.
In Claude Code, the replayharness-discovery and replayharness-replay agents do these steps.
Requests only go to the target set when ReplayHarness started; paths start with /. Requests may create, change and delete data. Do not try to reach other hosts.`

var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from in and writes responses to out until in closes.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	client, err := runner.NewClient(s.Config.Target, s.Config, nil)
	if err != nil {
		return err
	}
	defer client.Close()
	s.client = client

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.send(out, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification needs no answer
		}
		result, rerr := s.handle(ctx, req)
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		s.send(out, resp)
	}
	return sc.Err()
}

func (s *Server) send(out io.Writer, resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{-32603, err.Error()}})
	}
	s.out.Lock()
	defer s.out.Unlock()
	_, _ = out.Write(append(b, '\n'))
}

func (s *Server) handle(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		for _, v := range protocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "replayharness", "version": s.Version},
			"instructions":    Instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		text, err := s.call(ctx, p.Name, p.Arguments)
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func tools() []any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []any{
		map[string]any{
			"name": "discover",
			"description": "List the service's endpoints: method, path, parameters, body schema, responses, whether auth is required, the file:line of each handler " +
				"and the rules from the API map. Reads .replayharness/api.yaml (the API map) and any OpenAPI spec, and finds routes in the code by pattern. " +
				"Reports routes in the code that the map or spec leave out, and errors in the map. Call it again after editing the map to validate it.",
			"inputSchema": obj(map[string]any{
				"source": str("A source tree, an API map, an OpenAPI/Swagger file, or recorded traffic (.jsonl/.har). Defaults to the project ReplayHarness was started for; \"target\" fetches the spec from the running server."),
			}),
		},
		map[string]any{
			"name":        "send_request",
			"description": "Send one request to the target server and see the full response. Use it to explore before writing replays.",
			"inputSchema": obj(map[string]any{
				"method":  str("HTTP method, e.g. GET"),
				"path":    str("Path starting with /, may include a query string"),
				"query":   map[string]any{"type": "object", "description": "Query parameters", "additionalProperties": true},
				"headers": map[string]any{"type": "object", "description": "Extra headers", "additionalProperties": map[string]any{"type": "string"}},
				"json":    map[string]any{"description": "Body, sent as JSON"},
				"body":    str("Raw body, sent as is (use with a Content-Type header)"),
				"no_auth": map[string]any{"type": "boolean", "description": "Leave out the credentials the server was started with"},
			}, "method", "path"),
		},
		map[string]any{
			"name":        "run_replays",
			"description": "Run request replays: ordered steps with expectations, where values saved from one response ({{id}}) feed later steps. Returns which replays failed, at which step, why, and a curl command per failing step.\n\n" + agent.ReplayFormat,
			"inputSchema": obj(map[string]any{
				"replays": str("Replays as YAML or JSON text"),
				"file":    str("A replay file or a directory of them, instead of inline replays"),
			}),
		},
		map[string]any{
			"name": "probe",
			"description": "Run ReplayHarness's built-in requests for each endpoint: valid requests, missing required fields, wrong types, values at and past limits, " +
				"malformed bodies, unusual path and query values, missing credentials and undefined methods. Reports crashes, 5xx errors, accepted invalid input, " +
				"responses that break the spec and leaked stack traces, grouped, with a curl command each. Uses the endpoints from the last discover call.",
			"inputSchema": obj(map[string]any{
				"endpoint":         str("Only endpoints whose \"METHOD /path\" matches this regular expression"),
				"only":             str("Comma-separated categories: " + strings.Join(model.Categories, ", ")),
				"max_per_endpoint": map[string]any{"type": "integer", "description": "Cap on requests per endpoint (default 100)"},
			}),
		},
	}
}

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "discover":
		var a struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		api, err := s.discover(ctx, a.Source)
		if err != nil {
			return "", err
		}
		text := agent.Context(api, s.client.Target())
		src := a.Source
		if src == "" {
			src = s.Source
		}
		if info, err := os.Stat(src); err == nil && info.IsDir() {
			if abs, err := filepath.Abs(src); err == nil {
				text = "Code locations are relative to " + abs + "\n\n" + text
			}
		}
		return text, nil
	case "send_request":
		return s.sendRequest(ctx, args)
	case "run_replays":
		return s.runFlows(ctx, args)
	case "probe":
		return s.probe(ctx, args)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func (s *Server) discover(ctx context.Context, source string) (*model.API, error) {
	if source == "" {
		source = s.Source
	}
	var api *model.API
	var err error
	switch {
	case source == "" || source == "target":
		api, _, err = discover.FetchSpec(ctx, &http.Client{Timeout: 15 * time.Second}, s.client.Target())
	case strings.HasSuffix(strings.ToLower(source), ".jsonl") || strings.HasSuffix(strings.ToLower(source), ".har"):
		var exs []discover.Exchange
		if exs, err = discover.LoadTraffic(source); err == nil {
			api, _ = discover.EndpointsFromTraffic(exs)
		}
	default:
		var info os.FileInfo
		if info, err = os.Stat(source); err == nil {
			if info.IsDir() {
				api, err = discover.ScanRepo(source)
			} else {
				api, err = discover.LoadSpecFile(source)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.api = api
	s.mu.Unlock()
	return api, nil
}

func (s *Server) sendRequest(ctx context.Context, args json.RawMessage) (string, error) {
	var step replay.Step
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&step); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	f := replay.Replay{Name: "send_request", Steps: []replay.Step{step}}
	data, _ := json.Marshal([]replay.Replay{f})
	replays, err := replay.Parse(data)
	if err != nil {
		return "", err
	}
	req, err := replay.BuildRequest(replays[0].Steps[0], replay.BuiltinVars())
	if err != nil {
		return "", err
	}
	obs := s.client.Do(ctx, req, step.NoAuth)
	var b strings.Builder
	if obs.Status == 0 {
		fmt.Fprintf(&b, "No response after %.0fms: %s\n", obs.DurationMS, obs.Error)
	} else {
		fmt.Fprintf(&b, "HTTP %d (%.0fms)\n", obs.Status, obs.DurationMS)
		names := make([]string, 0, len(obs.Headers))
		for k := range obs.Headers {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(&b, "%s: %s\n", k, obs.Headers[k])
		}
		body := obs.Body
		if len(body) > 8000 {
			body = body[:8000] + fmt.Sprintf("\n… (%d more bytes)", len(obs.Body)-8000)
		}
		fmt.Fprintf(&b, "\n%s\n", body)
	}
	if fs := check.Basic(obs, false, s.Slow); len(fs) > 0 {
		b.WriteString("\nProblems:\n")
		for _, f := range fs {
			fmt.Fprintf(&b, "- %s %s: %s\n", f.Severity, f.Check, f.Message)
		}
	}
	return b.String(), nil
}

func (s *Server) runFlows(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Replays string `json:"replays"`
		File    string `json:"file"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	var replays []replay.Replay
	var err error
	switch {
	case a.File != "":
		replays, err = replay.Load(a.File)
	case strings.TrimSpace(a.Replays) != "":
		replays, err = replay.Parse([]byte(a.Replays))
	default:
		return "", fmt.Errorf("pass replays (YAML or JSON text) or file")
	}
	if err != nil {
		return "", err
	}
	r := &replay.Runner{Client: s.client, Headers: s.Config.Headers, Slow: s.Slow}
	var b strings.Builder
	replay.WriteText(&b, r.Run(ctx, replays), false)
	return b.String(), nil
}

func (s *Server) probe(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Endpoint       string `json:"endpoint"`
		Only           string `json:"only"`
		MaxPerEndpoint int    `json:"max_per_endpoint"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	s.mu.Lock()
	api := s.api
	s.mu.Unlock()
	if api == nil {
		var err error
		if api, err = s.discover(ctx, ""); err != nil {
			return "", fmt.Errorf("discover failed, so there is nothing to probe: %w", err)
		}
	}
	opts := generate.DefaultOptions()
	opts.MaxPerEndpoint = 100
	if a.MaxPerEndpoint > 0 {
		opts.MaxPerEndpoint = a.MaxPerEndpoint
	}
	opts.Only = map[string]bool{}
	for _, c := range strings.Split(a.Only, ",") {
		if c = strings.TrimSpace(c); c != "" {
			opts.Only[c] = true
		}
	}
	filtered := *api
	if a.Endpoint != "" {
		re, err := compile(a.Endpoint)
		if err != nil {
			return "", err
		}
		filtered.Endpoints = nil
		for _, ep := range api.Endpoints {
			if re.MatchString(ep.Key()) {
				filtered.Endpoints = append(filtered.Endpoints, ep)
			}
		}
	}
	suite := generate.Suite(&filtered, opts)
	if len(suite.Scenarios) == 0 {
		return "No requests to send: no endpoints matched.", nil
	}
	started := time.Now()
	cfg := s.Config
	cfg.Check = check.Options{Slow: s.Slow}
	cfg.Progress = nil
	results, err := runner.Run(ctx, cfg, suite)
	if err != nil {
		return "", err
	}
	auth := map[string]bool{}
	for k := range s.client.AuthHeaders() {
		auth[k] = true
	}
	rep := report.Build(results, &filtered, report.Options{Version: s.Version, Target: s.client.Target(), Headers: s.Config.Headers,
		AuthHeaders: auth, Started: started, Duration: time.Since(started)})
	var b strings.Builder
	report.WriteText(&b, rep, report.TextOptions{})
	return b.String(), nil
}
