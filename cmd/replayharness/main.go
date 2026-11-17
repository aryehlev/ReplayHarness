// Command replayharness generates request scenarios for an HTTP service, from
// its source tree, its OpenAPI spec, the running server, or recorded traffic,
// sends them, and reports what broke.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	replayharness "github.com/aryehlev/ReplayHarness"
	"github.com/aryehlev/ReplayHarness/internal/agent"
	"github.com/aryehlev/ReplayHarness/internal/check"
	"github.com/aryehlev/ReplayHarness/internal/discover"
	"github.com/aryehlev/ReplayHarness/internal/generate"
	"github.com/aryehlev/ReplayHarness/internal/mcp"
	"github.com/aryehlev/ReplayHarness/internal/model"
	"github.com/aryehlev/ReplayHarness/internal/record"
	"github.com/aryehlev/ReplayHarness/internal/replay"
	"github.com/aryehlev/ReplayHarness/internal/report"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

// version is set at build time by scripts/release.sh. A "go install" build
// reports the module version instead.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

const usageText = `ReplayHarness finds bugs in APIs with coding agents: REST, gRPC, GraphQL and
WebSocket. A discovery agent maps the API from the code, with or without an
OpenAPI spec, .proto file or GraphQL schema. A replay agent writes multi-step
requests that check what each handler should do, and ReplayHarness runs them
against your local server.

With a coding agent:
  replayharness mcp --target URL [--source .]        serve the tools to the agents (MCP over stdio)
  replayharness skill --install [--global]           install the skill and the two agents for Claude Code
  replayharness skill                                print the playbook for other agents (AGENTS.md)
  replayharness context <repo|spec|api.yaml|url>     what the agents see: endpoints, handlers, rules
  replayharness replay <replays.yaml|dir> --target URL
                                                     run replays the agent wrote

Without an agent:
  replayharness run    <repo|spec|api.yaml|url|scenarios.json> --target URL
                                                     send the built-in probes and report what broke
  replayharness scan   <repo|spec|api.yaml|url>      list the endpoints it finds
  replayharness gen    <repo|spec|api.yaml|url> [-o file]
                                                     write the probes without sending them
  replayharness record --upstream URL [-o traffic.jsonl]
                                                     proxy real traffic and record it
  replayharness replay <traffic.jsonl|file.har> --target URL [--mutate]
                                                     replay recorded traffic and compare
  replayharness version

gRPC: --grpc host:port when the gRPC server is not the --target, or
--target grpc://host:port for a gRPC-only server. --proto adds .proto files or
directories; without them the server is asked by reflection.

Requests only go to localhost and private addresses unless you pass
--allow-remote. Use a server you own, ideally a disposable one: requests
create, change and delete data.

Examples:
  claude mcp add replayharness -- replayharness mcp --target http://localhost:8080
  replayharness skill --install
  replayharness replay .replayharness/replays --target http://localhost:8080
  replayharness run . --target http://localhost:8080 --grpc localhost:9090

Run "replayharness <command> -h" for the flags of a command.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	switch args[0] {
	case "scan":
		return cmdScan(args[1:])
	case "gen", "generate":
		return cmdGen(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "record":
		return cmdRecord(args[1:])
	case "replay":
		// Recorded traffic is replayed and compared; anything else is a
		// file or directory of agent-written replays.
		for _, a := range args[1:] {
			if isTraffic(a) {
				return cmdReplay(args[1:])
			}
		}
		return cmdReplays(args[1:])
	case "context":
		return cmdContext(args[1:])
	case "check": // the name before 0.2
		return cmdReplays(args[1:])
	case "mcp":
		return cmdMCP(args[1:])
	case "skill":
		return cmdSkill(args[1:])
	case "version", "--version", "-version":
		fmt.Println("replayharness", version)
		return 0
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usageText)
	return 2
}

// parseArgs lets flags come before or after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type headerFlags map[string]string

func (h headerFlags) String() string { return "" }

func (h headerFlags) Set(v string) error {
	name, val, ok := strings.Cut(v, ":")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("header must look like 'Name: value'")
	}
	h[http.CanonicalHeaderKey(strings.TrimSpace(name))] = strings.TrimSpace(val)
	return nil
}

type genFlags struct {
	only, skip, endpoint string
	maxPer               int
	longString           int
	largeBody            int
}

func (g *genFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.only, "only", "", "comma-separated categories to generate (default: all)\ncategories: "+strings.Join(model.Categories, ", "))
	fs.StringVar(&g.skip, "skip", "", "comma-separated categories to leave out")
	fs.StringVar(&g.endpoint, "endpoint", "", "only endpoints whose \"METHOD /path\" matches this regular expression")
	fs.IntVar(&g.maxPer, "max-per-endpoint", 250, "cap on scenarios per endpoint (0 = no cap)")
	fs.IntVar(&g.longString, "long-string", 10_000, "length of the long-string probes")
	fs.IntVar(&g.largeBody, "large-body", 1<<20, "size in bytes of the oversized-body probe")
}

func (g *genFlags) options() generate.Options {
	o := generate.DefaultOptions()
	o.Only, o.Skip = set(g.only), set(g.skip)
	o.MaxPerEndpoint, o.LongString, o.LargeBody = g.maxPer, g.longString, g.largeBody
	return o
}

func set(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

type runFlags struct {
	target, baseline string
	grpc             string
	protos           string
	headers          headerFlags
	authHeaders      string
	concurrency      int
	rate             float64
	timeout, slow    time.Duration
	jsonOut          string
	junitOut         string
	failOn           string
	allowRemote      bool
	dryRun           bool
	noColor          bool
	verbose          bool
	exact            bool
	ignore           string
	source           string // what the scenarios came from, for the default --proto
}

// grpcRepro is the gRPC address shown in grpcurl commands.
func grpcRepro(target, grpc string) string {
	if grpc != "" {
		return grpc
	}
	if _, g := runner.SplitTarget(target); g != "" {
		return g
	}
	if u, err := url.Parse(target); err == nil {
		return u.Host
	}
	return ""
}

func (f *runFlags) register(fs *flag.FlagSet) {
	f.headers = headerFlags{}
	fs.StringVar(&f.target, "target", "", "base URL of the server to test, e.g. http://localhost:8080, or grpc://localhost:9090 for a gRPC-only server")
	fs.StringVar(&f.grpc, "grpc", "", "host:port of the gRPC server, when it is not the --target (grpcs://host:port for TLS)")
	fs.StringVar(&f.protos, "proto", "", "comma-separated .proto files or directories with the gRPC message types (default: the source tree; not needed when the server has reflection)")
	fs.StringVar(&f.baseline, "baseline", "", "base URL of a second server (e.g. the previous version) to compare every response with")
	fs.Var(f.headers, "H", "header sent with every request, e.g. -H 'Authorization: Bearer xyz' (repeatable)")
	fs.StringVar(&f.authHeaders, "auth-header", "", "comma-separated extra header names that carry credentials")
	fs.IntVar(&f.concurrency, "concurrency", 4, "requests in flight at once")
	fs.Float64Var(&f.rate, "rate", 50, "maximum requests per second (0 = no limit)")
	fs.DurationVar(&f.timeout, "timeout", 10*time.Second, "per-request timeout")
	fs.DurationVar(&f.slow, "slow", 2*time.Second, "warn about responses slower than this (0 = off)")
	fs.StringVar(&f.jsonOut, "json", "", "write the full report as JSON to this file")
	fs.StringVar(&f.junitOut, "junit", "", "write a JUnit XML report to this file (for CI)")
	fs.StringVar(&f.failOn, "fail-on", "error", "exit with status 1 when there are findings at this level: error, warning or never")
	fs.BoolVar(&f.allowRemote, "allow-remote", false, "allow targets outside localhost and private networks (only for servers you own)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "list the scenarios without sending them")
	fs.BoolVar(&f.noColor, "no-color", false, "disable colors")
	fs.BoolVar(&f.verbose, "v", false, "show notes and full details")
}

func (f *runFlags) registerReplay(fs *flag.FlagSet) {
	fs.BoolVar(&f.exact, "exact", false, "compare JSON values with the recording, not only their shape")
	fs.StringVar(&f.ignore, "ignore", strings.Join(check.DefaultIgnore, ","), "with --exact: comma-separated JSON keys whose values may change")
}

// protoPaths returns the --proto paths, or the source when it is a directory.
func (f *runFlags) protoPaths(source string) []string {
	if f.protos != "" {
		return splitOrdered(f.protos)
	}
	if info, err := os.Stat(source); err == nil && info.IsDir() {
		return []string{source}
	}
	return nil
}

func splitOrdered(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// loadAPI discovers endpoints from a repo, a spec file or a server URL. It
// also returns the default target (the server's origin for a URL source).
func loadAPI(ctx context.Context, src string) (*model.API, string, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		api, found, err := discover.FetchSpec(ctx, &http.Client{Timeout: 15 * time.Second}, src)
		if err != nil {
			return nil, "", err
		}
		u, _ := url.Parse(found)
		api.Sources = []string{found}
		return api, u.Scheme + "://" + u.Host, nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, "", err
	}
	if info.IsDir() {
		api, err := discover.ScanRepo(src)
		return api, "", err
	}
	api, err := discover.LoadSpecFile(src)
	return api, "", err
}

func filterEndpoints(api *model.API, pattern string) error {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("--endpoint: %w", err)
	}
	var keep []*model.Endpoint
	for _, ep := range api.Endpoints {
		if re.MatchString(ep.Key()) {
			keep = append(keep, ep)
		}
	}
	api.Endpoints = keep
	return nil
}

func isTraffic(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".jsonl") || strings.HasSuffix(lower, ".har") || strings.HasSuffix(lower, ".ndjson")
}

func isSuiteFile(path string) bool {
	if !strings.HasSuffix(strings.ToLower(path), ".json") {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var probe struct {
		Scenarios json.RawMessage `json:"scenarios"`
		OpenAPI   any             `json:"openapi"`
		Swagger   any             `json:"swagger"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.Scenarios != nil && probe.OpenAPI == nil && probe.Swagger == nil
}

// buildSuite turns any source into scenarios.
func buildSuite(ctx context.Context, src string, g *genFlags) (*model.Suite, string, error) {
	switch {
	case isTraffic(src):
		return trafficSuite(src, true, g)
	case isSuiteFile(src):
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, "", err
		}
		var suite model.Suite
		if err := json.Unmarshal(data, &suite); err != nil {
			return nil, "", fmt.Errorf("%s: %w", src, err)
		}
		suite.Scenarios = filterScenarios(suite.Scenarios, g)
		return &suite, "", nil
	}
	api, defTarget, err := loadAPI(ctx, src)
	if err != nil {
		return nil, "", err
	}
	if err := filterEndpoints(api, g.endpoint); err != nil {
		return nil, "", err
	}
	if len(api.Endpoints) == 0 {
		return nil, "", fmt.Errorf("no endpoints found in %s", src)
	}
	return generate.Suite(api, g.options()), defTarget, nil
}

func filterScenarios(in []model.Scenario, g *genFlags) []model.Scenario {
	only, skip := set(g.only), set(g.skip)
	var re *regexp.Regexp
	if g.endpoint != "" {
		re, _ = regexp.Compile(g.endpoint)
	}
	var out []model.Scenario
	for _, s := range in {
		if (len(only) > 0 && !only[s.Category]) || skip[s.Category] || (re != nil && !re.MatchString(s.Endpoint)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func trafficSuite(path string, mutate bool, g *genFlags) (*model.Suite, string, error) {
	exs, err := discover.LoadTraffic(path)
	if err != nil {
		return nil, "", err
	}
	if len(exs) == 0 {
		return nil, "", fmt.Errorf("%s has no recorded requests", path)
	}
	api, keys := discover.EndpointsFromTraffic(exs)
	suite := &model.Suite{API: api}
	opts := g.options()
	if opts.Enabled(model.CatReplay) {
		suite.Scenarios = generate.Replay(exs, keys)
	}
	if mutate {
		if err := filterEndpoints(api, g.endpoint); err != nil {
			return nil, "", err
		}
		opts.Skip[model.CatReplay] = true
		suite.Scenarios = append(suite.Scenarios, generate.Suite(api, opts).Scenarios...)
	}
	suite.Scenarios = filterScenarios(suite.Scenarios, g)
	return suite, "", nil
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the discovered API as JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: replayharness scan <repo|spec|url|traffic.jsonl>")
		return 2
	}
	ctx, cancel := signalContext()
	defer cancel()
	var api *model.API
	if isTraffic(pos[0]) {
		exs, err := discover.LoadTraffic(pos[0])
		if err != nil {
			return fail(err)
		}
		api, _ = discover.EndpointsFromTraffic(exs)
	} else if api, _, err = loadAPI(ctx, pos[0]); err != nil {
		return fail(err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(api)
		return 0
	}
	printAPI(os.Stdout, api)
	return 0
}

func printAPI(w io.Writer, api *model.API) {
	fmt.Fprintf(w, "%s: %d endpoint(s) from %s\n\n", api.Title, len(api.Endpoints), strings.Join(api.Sources, ", "))
	for _, ep := range api.Endpoints {
		var bits []string
		for _, p := range ep.Params {
			if p.In != "path" {
				name := p.Name
				if p.Required {
					name += "*"
				}
				bits = append(bits, p.In+":"+name)
			}
		}
		if ep.Body != nil && len(ep.Body.Properties) > 0 {
			var fields []string
			for _, n := range ep.Body.PropertyNames() {
				if ep.Body.IsRequired(n) {
					n += "*"
				}
				fields = append(fields, n)
			}
			bits = append(bits, "body{"+strings.Join(fields, ", ")+"}")
		} else if ep.BodyType != "" {
			bits = append(bits, "body")
		}
		if ep.Secured {
			bits = append(bits, "auth")
		}
		method := ep.Method
		if ep.AnyMethod {
			method = "ANY"
		}
		fmt.Fprintf(w, "  %-7s %-32s %-40s %s\n", method, ep.Path, strings.Join(bits, " "), ep.Source)
	}
	if len(api.Drift) > 0 {
		fmt.Fprintln(w, "\nSpec drift:")
		for _, d := range api.Drift {
			label := "in the code, missing from the spec"
			if d.Kind == "unimplemented" {
				label = "in the spec, no route found in the code"
			}
			fmt.Fprintf(w, "  %-7s %-32s %s (%s)\n", d.Method, d.Path, label, d.Where)
		}
	}
	for _, n := range api.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

func cmdGen(args []string) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	var g genFlags
	g.register(fs)
	out := fs.String("o", "", "write the scenarios to this file (default: stdout)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: replayharness gen <repo|spec|url|traffic.jsonl> [-o scenarios.json]")
		return 2
	}
	ctx, cancel := signalContext()
	defer cancel()
	suite, _, err := buildSuite(ctx, pos[0], &g)
	if err != nil {
		return fail(err)
	}
	w := io.Writer(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(suite); err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "%d scenarios for %d endpoints (%s)\n", len(suite.Scenarios), len(suite.API.Endpoints), categoryCounts(suite.Scenarios))
	return 0
}

func categoryCounts(scs []model.Scenario) string {
	counts := map[string]int{}
	for _, s := range scs {
		counts[s.Category]++
	}
	var parts []string
	for _, c := range model.Categories {
		if counts[c] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", c, counts[c]))
		}
	}
	return strings.Join(parts, ", ")
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var g genFlags
	var f runFlags
	g.register(fs)
	f.register(fs)
	f.registerReplay(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: replayharness run <repo|spec|url|scenarios.json|traffic.jsonl> --target URL")
		return 2
	}
	ctx, cancel := signalContext()
	defer cancel()
	f.source = pos[0]
	suite, defTarget, err := buildSuite(ctx, pos[0], &g)
	if err != nil {
		return fail(err)
	}
	return execute(ctx, suite, defTarget, &f)
}

func cmdReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	var g genFlags
	var f runFlags
	g.register(fs)
	f.register(fs)
	f.registerReplay(fs)
	mutate := fs.Bool("mutate", false, "also generate scenarios that mutate the recorded requests")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: replayharness replay <traffic.jsonl|file.har> --target URL [--mutate]")
		return 2
	}
	ctx, cancel := signalContext()
	defer cancel()
	suite, _, err := trafficSuite(pos[0], *mutate, &g)
	if err != nil {
		return fail(err)
	}
	return execute(ctx, suite, "", &f)
}

func execute(ctx context.Context, suite *model.Suite, defTarget string, f *runFlags) int {
	target := f.target
	if target == "" {
		target = defTarget
	}
	if f.dryRun {
		for _, s := range suite.Scenarios {
			q := ""
			if s.Request.Query != "" {
				q = "?" + s.Request.Query
			}
			if len(q) > 80 {
				q = q[:77] + "..."
			}
			fmt.Printf("%s  %-16s %-6s %s%s  %s\n", s.ID, s.Category, s.Request.Method, s.Request.Path, q, s.Name)
		}
		fmt.Fprintf(os.Stderr, "%d scenarios (%s)\n", len(suite.Scenarios), categoryCounts(suite.Scenarios))
		return 0
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "error: --target is required, e.g. --target http://localhost:8080")
		return 2
	}
	switch f.failOn {
	case "error", "warning", "never":
	default:
		fmt.Fprintln(os.Stderr, "error: --fail-on must be error, warning or never")
		return 2
	}
	if len(suite.Scenarios) == 0 {
		fmt.Fprintln(os.Stderr, "error: no scenarios to run")
		return 2
	}

	auth := map[string]bool{}
	for _, h := range runner.DefaultAuthHeaders {
		auth[http.CanonicalHeaderKey(h)] = true
	}
	var extraAuth []string
	for h := range set(f.authHeaders) {
		extraAuth = append(extraAuth, h)
		auth[http.CanonicalHeaderKey(h)] = true
	}
	if suite.API != nil {
		for _, h := range suite.API.AuthHeaders {
			auth[http.CanonicalHeaderKey(h)] = true
		}
	}
	ignore := map[string]bool{}
	for k := range set(f.ignore) {
		ignore[k] = true
	}
	color := !f.noColor && os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)

	cfg := runner.Config{
		Target: target, GRPCTarget: f.grpc, Protos: f.protoPaths(f.source), Baseline: f.baseline, Headers: f.headers, AuthHeaders: extraAuth,
		Concurrency: f.concurrency, Rate: f.rate, Timeout: f.timeout, AllowRemote: f.allowRemote,
		UserAgent: "ReplayHarness/" + version,
		Check:     check.Options{Slow: f.slow, Exact: f.exact, Ignore: ignore},
	}
	if isTTY(os.Stderr) {
		var mu sync.Mutex
		cfg.Progress = func(done, total int) {
			mu.Lock()
			defer mu.Unlock()
			if done == total || done%10 == 0 {
				fmt.Fprintf(os.Stderr, "\r\033[K  %d/%d scenarios", done, total)
			}
		}
	}
	started := time.Now()
	results, err := runner.Run(ctx, cfg, suite)
	if cfg.Progress != nil {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	interrupted := errors.Is(err, context.Canceled)
	if err != nil && !interrupted {
		return fail(err)
	}
	rep := report.Build(results, suite.API, report.Options{
		Version: version, Target: target, GRPCTarget: grpcRepro(target, f.grpc), Baseline: f.baseline, Headers: f.headers, AuthHeaders: auth,
		Started: started, Duration: time.Since(started), Interrupted: interrupted,
	})
	report.WriteText(os.Stdout, rep, report.TextOptions{Color: color, Verbose: f.verbose})
	if f.jsonOut != "" {
		if err := report.WriteJSON(f.jsonOut, rep); err != nil {
			return fail(err)
		}
		fmt.Fprintf(os.Stderr, "JSON report: %s\n", f.jsonOut)
	}
	if f.junitOut != "" {
		if err := report.WriteJUnit(f.junitOut, rep, f.failOn); err != nil {
			return fail(err)
		}
		fmt.Fprintf(os.Stderr, "JUnit report: %s\n", f.junitOut)
	}
	if interrupted {
		return 130
	}
	if rep.Failed(f.failOn) {
		return 1
	}
	return 0
}

func cmdRecord(args []string) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	upstream := fs.String("upstream", "", "base URL of the server to record, e.g. http://localhost:8080")
	listen := fs.String("listen", "127.0.0.1:8099", "address the proxy listens on")
	out := fs.String("o", "traffic.jsonl", "file to append the recording to")
	keep := fs.Bool("keep-secrets", false, "keep Authorization, Cookie and similar headers in the recording")
	quiet := fs.Bool("q", false, "do not print each request")
	if _, err := parseArgs(fs, args); err != nil {
		return 2
	}
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "usage: replayharness record --upstream http://localhost:8080 [--listen 127.0.0.1:8099] [-o traffic.jsonl]")
		return 2
	}
	u, err := url.Parse(*upstream)
	if err != nil || u.Host == "" {
		return fail(fmt.Errorf("invalid --upstream %q", *upstream))
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	ctx, cancel := signalContext()
	defer cancel()
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	opts := record.Options{
		Upstream: u, Listen: *listen, Out: f, KeepSecrets: *keep,
		Ready: func(addr string) {
			fmt.Fprintf(os.Stderr, "Recording http://%s -> %s into %s\n", addr, *upstream, *out)
			fmt.Fprintln(os.Stderr, "Point your app, browser or tests at the proxy. Press Ctrl-C to stop.")
			if !*keep {
				fmt.Fprintln(os.Stderr, "Credentials (Authorization, Cookie, API keys) are left out of the recording; pass --keep-secrets to keep them.")
			}
		},
	}
	if !*quiet {
		opts.Log = logf
	}
	if err := record.Serve(ctx, opts); err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "\nSaved to %s. Replay it with:\n  replayharness replay %s --target %s --mutate\n", *out, *out, *upstream)
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 2
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func cmdContext(args []string) int {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	target := fs.String("target", "", "base URL of the server under test (shown to the agent)")
	format := fs.Bool("with-format", true, "append the replay format reference")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: replayharness context <repo|spec|url|traffic.jsonl> [--target URL]")
		return 2
	}
	ctx, cancel := signalContext()
	defer cancel()
	var api *model.API
	if isTraffic(pos[0]) {
		exs, err := discover.LoadTraffic(pos[0])
		if err != nil {
			return fail(err)
		}
		api, _ = discover.EndpointsFromTraffic(exs)
	} else if api, _, err = loadAPI(ctx, pos[0]); err != nil {
		return fail(err)
	}
	fmt.Print(agent.Context(api, *target, ""))
	if *format {
		fmt.Printf("\n## Writing replays\n\n%s\n", agent.ReplayFormat)
	}
	return 0
}

func cmdReplays(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	var f runFlags
	f.register(fs)
	format := fs.String("format", "text", "output format: text or json")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) == 0 || f.target == "" {
		fmt.Fprintln(os.Stderr, "usage: replayharness replay <replays.yaml|dir>... --target URL [-H 'Name: value'] [--format json] [--junit file]\n       replayharness replay <traffic.jsonl|file.har> --target URL [--mutate]")
		return 2
	}
	var replays []replay.Replay
	for _, p := range pos {
		fl, err := replay.Load(p)
		if err != nil {
			return fail(err)
		}
		replays = append(replays, fl...)
	}
	if f.dryRun {
		for _, fl := range replays {
			unit := "steps"
			if len(fl.Steps) == 1 {
				unit = "step"
			}
			fmt.Printf("%s (%d %s)\n", fl.Name, len(fl.Steps), unit)
		}
		return 0
	}
	client, err := runner.NewClient(f.target, runner.Config{
		GRPCTarget: f.grpc, Protos: f.protoPaths("."),
		Headers: f.headers, AuthHeaders: splitList(f.authHeaders), Rate: f.rate, Timeout: f.timeout,
		AllowRemote: f.allowRemote, UserAgent: "ReplayHarness/" + version,
	}, nil)
	if err != nil {
		return fail(err)
	}
	defer client.Close()
	ctx, cancel := signalContext()
	defer cancel()
	results := (&replay.Runner{Client: client, Headers: f.headers, Slow: f.slow}).Run(ctx, replays)
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		passed, failed := replay.Summary(results)
		_ = enc.Encode(map[string]any{"target": client.Target(), "passed": passed, "failed": failed, "replays": results})
	default:
		replay.WriteText(os.Stdout, results, !f.noColor && os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout))
	}
	if f.junitOut != "" {
		if err := replay.WriteJUnit(f.junitOut, results); err != nil {
			return fail(err)
		}
	}
	if ctx.Err() != nil {
		return 130
	}
	if _, failed := replay.Summary(results); failed > 0 && f.failOn != "never" {
		return 1
	}
	return 0
}

func cmdMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	var f runFlags
	f.register(fs)
	source := fs.String("source", ".", "what discover reads by default: a source tree, a spec file, recorded traffic, or \"target\" to fetch the spec from the server")
	if _, err := parseArgs(fs, args); err != nil {
		return 2
	}
	if f.target == "" {
		fmt.Fprintln(os.Stderr, "usage: replayharness mcp --target http://localhost:8080 [--grpc localhost:9090] [--source .] [-H 'Authorization: Bearer ...']")
		return 2
	}
	httpTarget, grpcTarget := runner.SplitTarget(f.target)
	if _, err := runner.CheckTarget(httpTarget, f.allowRemote); err != nil {
		return fail(err)
	}
	if f.grpc == "" {
		f.grpc = grpcTarget
	}
	if f.grpc != "" {
		if err := runner.CheckGRPCTarget(f.grpc, f.allowRemote); err != nil {
			return fail(err)
		}
	}
	ctx, cancel := signalContext()
	defer cancel()
	srv := &mcp.Server{
		Version: version,
		Source:  *source,
		Slow:    f.slow,
		Config: runner.Config{
			Target: httpTarget, GRPCTarget: f.grpc, Protos: f.protoPaths(*source),
			Headers: f.headers, AuthHeaders: splitList(f.authHeaders), Concurrency: f.concurrency,
			Rate: f.rate, Timeout: f.timeout, AllowRemote: f.allowRemote, UserAgent: "ReplayHarness/" + version,
		},
	}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return fail(err)
	}
	return 0
}

func cmdSkill(args []string) int {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	install := fs.Bool("install", false, "install the skill and the two agents into .claude/ in this project")
	global := fs.Bool("global", false, "with --install: install into ~/.claude/ for every project")
	force := fs.Bool("force", false, "with --install: overwrite files that were changed")
	if _, err := parseArgs(fs, args); err != nil {
		return 2
	}
	if !*install {
		fmt.Print(replayharness.Playbook())
		return 0
	}
	root := "."
	if *global {
		home, err := os.UserHomeDir()
		if err != nil {
			return fail(err)
		}
		root = home
	}
	files := replayharness.AgentFiles()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(root, name)
		if old, err := os.ReadFile(path); err == nil && string(old) != files[name] && !*force {
			fmt.Fprintf(os.Stderr, "Kept %s: it differs from this version (use --force to overwrite)\n", path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(path, []byte(files[name]), 0o644); err != nil {
			return fail(err)
		}
		fmt.Fprintf(os.Stderr, "Wrote %s\n", path)
	}
	return 0
}

func splitList(csv string) []string {
	var out []string
	for h := range set(csv) {
		out = append(out, h)
	}
	return out
}
