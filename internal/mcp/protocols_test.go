package mcp

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aryehlev/ReplayHarness/examples/protocols"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

// startProtocols serves the gRPC, GraphQL and WebSocket demo. The source is
// "target", so discover has to ask the running servers.
func startProtocols(t *testing.T) *session {
	g, err := protocols.NewGRPC()
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	srv := httptest.NewUnstartedServer(protocols.NewHTTP())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{Version: "test", Source: "target", Slow: 2 * time.Second,
		Config: runner.Config{Target: srv.URL, GRPCTarget: lis.Addr().String(), Timeout: 5 * time.Second, Concurrency: 8}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, inR, outW); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		cancel()
		<-done
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	return &session{t: t, in: inW, out: sc}
}

func TestOtherProtocols(t *testing.T) {
	s := startProtocols(t)

	// No source tree: the gRPC methods come from server reflection.
	text, isErr := s.tool("discover", nil)
	if isErr || !strings.Contains(text, "## gRPC orders.v1.Orders/CreateOrder") || !strings.Contains(text, "customer*: string") ||
		!strings.Contains(text, "gRPC server reflection") {
		t.Fatalf("discover:\n%s", text)
	}

	text, isErr = s.tool("send_request", map[string]any{"grpc": "orders.v1.Orders/GetOrder", "message": map[string]any{"id": 1}})
	if isErr || !strings.Contains(text, "gRPC OK") || !strings.Contains(text, `"customer":"ada"`) {
		t.Errorf("gRPC send_request:\n%s", text)
	}
	text, isErr = s.tool("send_request", map[string]any{"grpc": "orders.v1.Orders/GetOrder", "message": map[string]any{"nope": 1}})
	if !isErr || !strings.Contains(text, "fields: id (int64)") {
		t.Errorf("a message that does not fit should say which fields exist:\n%s", text)
	}
	text, isErr = s.tool("send_request", map[string]any{"graphql": "{ product(id: 0) { id } }"})
	if isErr || !strings.Contains(text, "HTTP 200") || !strings.Contains(text, "GraphQL error that looks like a server failure") {
		t.Errorf("GraphQL send_request:\n%s", text)
	}
	text, isErr = s.tool("send_request", map[string]any{"websocket": "/ws/chat", "send": []any{map[string]any{"type": "join", "room": "t", "name": "x"}}, "receive": 1})
	if isErr || !strings.Contains(text, "WebSocket 101") || !strings.Contains(text, `"type":"joined"`) {
		t.Errorf("WebSocket send_request:\n%s", text)
	}

	text, isErr = s.tool("run_replays", map[string]any{"replays": `
replays:
  - name: unknown orders are NOT_FOUND
    steps:
      - grpc: orders.v1.Orders/GetOrder
        message: {id: 12345}
        expect: {code: NOT_FOUND}
`})
	if isErr || !strings.Contains(text, "code UNKNOWN") || !strings.Contains(text, "0 passed, 1 failed") {
		t.Errorf("run_replays:\n%s", text)
	}

	text, isErr = s.tool("probe", map[string]any{"endpoint": "ListOrders"})
	if isErr || !strings.Contains(text, "INTERNAL") || !strings.Contains(text, "grpcurl") {
		t.Errorf("probe:\n%s", text)
	}
}
