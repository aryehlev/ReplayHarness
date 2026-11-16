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
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aryehlev/ReplayHarness/internal/check"
	"github.com/aryehlev/ReplayHarness/internal/discover"
	"github.com/aryehlev/ReplayHarness/internal/generate"
	"github.com/aryehlev/ReplayHarness/internal/model"
	"github.com/aryehlev/ReplayHarness/internal/record"
	"github.com/aryehlev/ReplayHarness/internal/report"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

var version = "0.1.0"

const usageText = `ReplayHarness builds requests that try to break your HTTP service, sends
them, and reports what broke.

Usage:
  replayharness scan   <repo|spec|url>                   list the endpoints it finds
  replayharness gen    <repo|spec|url> [-o file]         write the scenarios without sending them
  replayharness run    <repo|spec|url|scenarios.json> --target URL
                                                         generate, send and report
  replayharness record --upstream URL [--listen addr] [-o traffic.jsonl]
                                                         proxy real traffic and record it
  replayharness replay <traffic.jsonl|file.har> --target URL [--mutate]
                                                         replay recorded traffic and compare
  replayharness version

A source can be a source tree (OpenAPI files and routes in the code are both
used), an OpenAPI/Swagger file, the URL of a running server (its spec is
fetched), a scenarios file written by gen, or recorded traffic.

Generated traffic is only sent to localhost and private addresses unless you
pass --allow-remote. Run it against a server you own, ideally a disposable one:
scenarios create, change and delete data.

Examples:
  replayharness run . --target http://localhost:8080
  replayharness run http://localhost:8000 -H "Authorization: Bearer $TOKEN"
  replayharness run ./api --target http://localhost:8080 --baseline http://localhost:9090
  replayharness record --upstream http://localhost:8080 -o traffic.jsonl
  replayharness replay traffic.jsonl --target http://localhost:8080 --mutate

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
		return cmdReplay(args[1:])
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
}

func (f *runFlags) register(fs *flag.FlagSet) {
	f.headers = headerFlags{}
	fs.StringVar(&f.target, "target", "", "base URL of the server to test, e.g. http://localhost:8080")
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
		Target: target, Baseline: f.baseline, Headers: f.headers, AuthHeaders: extraAuth,
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
		Version: version, Target: target, Baseline: f.baseline, Headers: f.headers, AuthHeaders: auth,
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
