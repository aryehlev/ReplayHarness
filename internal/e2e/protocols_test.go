package e2e

import (
	"context"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aryehlev/ReplayHarness/examples/protocols"
	"github.com/aryehlev/ReplayHarness/internal/check"
	"github.com/aryehlev/ReplayHarness/internal/discover"
	"github.com/aryehlev/ReplayHarness/internal/generate"
	"github.com/aryehlev/ReplayHarness/internal/model"
	"github.com/aryehlev/ReplayHarness/internal/replay"
	"github.com/aryehlev/ReplayHarness/internal/report"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

// startProtocols runs the demo: HTTP (GraphQL, WebSocket) and gRPC.
func startProtocols(t *testing.T) (httpURL, grpcAddr string) {
	t.Helper()
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
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the chat panics on purpose
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, lis.Addr().String()
}

// The replays the replay agent would write for the demo: the ones that
// describe a planted bug fail, the others pass.
func TestProtocolReplays(t *testing.T) {
	httpURL, grpcAddr := startProtocols(t)
	client, err := runner.NewClient(httpURL, runner.Config{GRPCTarget: grpcAddr, Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	replays, err := replay.Load("../../examples/protocols/replays.yaml")
	if err != nil {
		t.Fatal(err)
	}
	results := (&replay.Runner{Client: client, Slow: 2 * time.Second}).Run(context.Background(), replays)
	var passed []string
	for _, r := range results {
		if r.Passed {
			passed = append(passed, r.Name)
		}
	}
	want := []string{
		"a product can be fetched",
		"an order can be read back",
		"joining a room answers joined",
		"say before join is an error message",
		"watching a customer streams only their orders",
	}
	sort.Strings(passed)
	if strings.Join(passed, "|") != strings.Join(want, "|") {
		var b strings.Builder
		replay.WriteText(&b, results, false)
		t.Fatalf("passed %q, want %q\n%s", passed, want, b.String())
	}
	if len(results) != len(want)+12 {
		t.Fatalf("%d replays ran, want %d", len(results), len(want)+12)
	}

	byName := map[string]replay.Result{}
	for _, r := range results {
		byName[r.Name] = r
	}
	failure := func(name string) string {
		var b strings.Builder
		replay.WriteText(&b, []replay.Result{byName[name]}, false)
		return b.String()
	}
	for name, part := range map[string]string{
		"an unknown order is NOT_FOUND":                                      "code UNKNOWN",
		"a negative page size is rejected":                                   "server-error: returned INTERNAL",
		"a cancelled order cannot be cancelled again":                        "code OK, expected FAILED_PRECONDITION",
		"product 0 is null, not a crash":                                     "GraphQL error that looks like a server failure",
		"a negative limit is rejected":                                       "GraphQL error that looks like a server failure",
		"the member count drops when a client leaves":                        "$[0].members is 2, expected 1",
		"a message that is not JSON gets an error, not a dropped connection": "without a close frame",
		"an order's total comes from its items":                              `$.total_cents is "1", expected "3000"`,
	} {
		if !strings.Contains(failure(name), part) {
			t.Errorf("%s: want %q in\n%s", name, part, failure(name))
		}
	}
	if out := failure("an unknown order is NOT_FOUND"); !strings.Contains(out, "grpcurl -plaintext -d '{\"id\":999999}' "+grpcAddr+" orders.v1.Orders/GetOrder") {
		t.Errorf("no grpcurl repro:\n%s", out)
	}
}

// The probes find the bugs a schema can reveal, from the .proto file, the
// GraphQL schema and the API map, without an OpenAPI spec.
func TestProtocolProbes(t *testing.T) {
	httpURL, grpcAddr := startProtocols(t)
	dir := t.TempDir()
	src := "../../examples/protocols"
	for _, f := range []string{"grpc.go", "graphql.go", "ws.go", "protocols.go", "schema.graphql", "proto/orders/v1/orders.proto", "proto/google/api/field_behavior.proto"} {
		copyFile(t, filepath.Join(src, f), filepath.Join(dir, f))
	}
	copyFile(t, filepath.Join(src, "api.yaml"), filepath.Join(dir, discover.MapFile))

	api, err := discover.ScanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, ep := range api.Endpoints {
		counts[ep.Protocol]++
	}
	if counts[model.ProtoGRPC] != 5 || counts[model.ProtoGraphQL] != 4 || counts[model.ProtoWebSocket] != 1 || counts[""] != 1 {
		t.Fatalf("endpoints by protocol: %v\n%v", counts, api.Notes)
	}
	if len(api.Drift) != 0 {
		t.Errorf("unexpected drift: %+v", api.Drift)
	}
	if ep := api.Endpoint("GRPC /orders.v1.Orders/CreateOrder"); ep == nil || ep.Code != "grpc.go:159" || !ep.Body.IsRequired("customer") {
		t.Errorf("the map was not merged into the .proto endpoint: %+v", ep)
	}

	suite := generate.Suite(api, generate.DefaultOptions())
	results, err := runner.Run(context.Background(), runner.Config{
		Target: httpURL, GRPCTarget: grpcAddr, Protos: []string{dir}, Concurrency: 8, Timeout: 5 * time.Second,
		Check: check.Options{Slow: 2 * time.Second},
	}, suite)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.Build(results, api, report.Options{Target: httpURL, GRPCTarget: grpcAddr})
	has := func(endpoint, chk, msgPart string) bool {
		for _, g := range rep.Findings {
			if g.Endpoint == endpoint && g.Check == chk && strings.Contains(g.Message, msgPart) {
				return true
			}
		}
		return false
	}
	for _, w := range []struct{ endpoint, check, msg, why string }{
		{"GRPC /orders.v1.Orders/CreateOrder", "accepted-invalid", "missing a required field", "customer is REQUIRED but never checked"},
		{"GRPC /orders.v1.Orders/GetOrder", "server-error", "UNKNOWN", "a plain error instead of NOT_FOUND"},
		{"GRPC /orders.v1.Orders/ListOrders", "server-error", "INTERNAL", "a negative page_size panics"},
		{"GRPC /orders.v1.Orders/ListOrders", "error-leak", "Go stack trace", "the panic's stack goes to the client"},
		{"QUERY product", "server-error", "internal system error", "id 0 or below panics"},
		{"QUERY products", "server-error", "internal system error", "a negative limit panics"},
		{"MUTATION addReview", "accepted-invalid", "boundary", "rating is not range-checked"},
		{"MUTATION deleteProduct", "auth-bypass", "", "the admin token is never checked"},
		{"MUTATION deleteProduct", "get-mutation", "", "GET runs mutations"},
		{"WS /ws/chat", "connection", "", "a message that is not a JSON object drops the connection"},
		{"WS /ws/chat", "server-error", "1011", "a long room name closes with an internal error"},
	} {
		if !has(w.endpoint, w.check, w.msg) {
			t.Errorf("missed %s on %s (%s)", w.check, w.endpoint, w.why)
		}
	}
	for _, g := range rep.Findings {
		switch g.Endpoint {
		case "GET /health", "GRPC /orders.v1.Orders/WatchOrders", "GRPC /orders.v1.Orders/CancelOrder":
			if g.Severity != model.SevInfo {
				t.Errorf("false positive on a correct endpoint: %+v", g)
			}
		}
		if strings.HasPrefix(g.Endpoint, "GRPC ") && !strings.HasPrefix(g.Repro, "grpcurl ") {
			t.Errorf("gRPC finding without a grpcurl repro: %+v", g)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
