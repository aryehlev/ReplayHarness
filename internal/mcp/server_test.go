package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aryehlev/ReplayHarness/examples/buggyapi"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

type session struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Scanner
	id  int
}

func (s *session) call(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	if _, err := s.in.Write(append(msg, '\n')); err != nil {
		s.t.Fatal(err)
	}
	if !s.out.Scan() {
		s.t.Fatalf("no response to %s", method)
	}
	var resp map[string]any
	if err := json.Unmarshal(s.out.Bytes(), &resp); err != nil {
		s.t.Fatal(err)
	}
	if int(resp["id"].(float64)) != s.id {
		s.t.Fatalf("response id %v, want %d", resp["id"], s.id)
	}
	return resp
}

func (s *session) tool(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	resp := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	res := resp["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
}

func start(t *testing.T) *session {
	srv := httptest.NewUnstartedServer(buggyapi.Handler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{Version: "test", Source: "../../examples/buggyapi", Slow: 2 * time.Second,
		Config: runner.Config{Target: srv.URL, Timeout: 5 * time.Second, Concurrency: 8}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, inR, outW); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	return &session{t: t, in: inW, out: sc}
}

func TestProtocol(t *testing.T) {
	s := start(t)
	init := s.call("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t"}})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2024-11-05" || res["instructions"] == "" {
		t.Errorf("initialize = %v", res)
	}
	// A notification gets no reply; the next reply must belong to the next request.
	_, _ = s.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	tools := s.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 4 {
		t.Errorf("tools = %d", len(tools))
	}
	if e := s.call("bogus", nil)["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Errorf("unknown method error = %v", e)
	}
}

func TestTools(t *testing.T) {
	s := start(t)
	text, isErr := s.tool("discover", nil)
	if isErr || !strings.Contains(text, "## DELETE /users/{id}") || !strings.Contains(text, "handler registered at: buggyapi.go:") {
		t.Fatalf("discover:\n%s", text)
	}

	text, isErr = s.tool("send_request", map[string]any{"method": "GET", "path": "/users/0"})
	if isErr || !strings.Contains(text, "HTTP 500") || !strings.Contains(text, "error-leak") {
		t.Errorf("send_request:\n%s", text)
	}
	if text, isErr = s.tool("send_request", map[string]any{"method": "GET", "path": "http://example.com/"}); !isErr {
		t.Errorf("a full URL must be refused, got:\n%s", text)
	}

	text, isErr = s.tool("run_replays", map[string]any{"file": "../../examples/buggyapi/replays.yaml"})
	if isErr || !strings.Contains(text, "3 passed, 7 failed") {
		t.Errorf("run_replays:\n%s", text)
	}
	text, isErr = s.tool("run_replays", map[string]any{"replays": "name: x\nsteps: [{method: GET, path: /health, expect: {status: 200}}]"})
	if isErr || !strings.Contains(text, "1 passed, 0 failed") {
		t.Errorf("inline run_replays:\n%s", text)
	}

	text, isErr = s.tool("probe", map[string]any{"endpoint": "^POST /users$"})
	if isErr || !strings.Contains(text, "server-error") || strings.Contains(text, "GET /health") {
		t.Errorf("probe:\n%s", text)
	}
}
