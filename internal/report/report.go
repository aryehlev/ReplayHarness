// Package report groups findings and writes them as text, JSON or JUnit XML.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Options describes the run being reported.
type Options struct {
	Version     string
	Target      string
	GRPCTarget  string // host:port (or grpcs://host:port) for gRPC repro commands
	Baseline    string
	Headers     map[string]string // global headers, shown redacted in repro commands
	AuthHeaders map[string]bool
	Started     time.Time
	Duration    time.Duration
	Interrupted bool
}

// Group is one distinct problem: the same check failing with the same
// message on the same endpoint, however many scenarios triggered it.
type Group struct {
	Endpoint   string         `json:"endpoint"`
	Check      string         `json:"check"`
	Severity   model.Severity `json:"severity"`
	Message    string         `json:"message"`
	Count      int            `json:"count"`
	Example    string         `json:"example"`
	ScenarioID string         `json:"scenarioId"`
	Status     int            `json:"status,omitempty"`
	Detail     string         `json:"detail,omitempty"`
	Repro      string         `json:"repro,omitempty"`
}

// Summary counts distinct problems by severity.
type Summary struct {
	Scenarios int `json:"scenarios"`
	Endpoints int `json:"endpoints"`
	Errors    int `json:"errors"`
	Warnings  int `json:"warnings"`
	Info      int `json:"info"`
}

// Report is the full outcome of a run.
type Report struct {
	Tool        string            `json:"tool"`
	Version     string            `json:"version"`
	Target      string            `json:"target"`
	Baseline    string            `json:"baseline,omitempty"`
	StartedAt   time.Time         `json:"startedAt"`
	DurationMS  int64             `json:"durationMs"`
	Interrupted bool              `json:"interrupted,omitempty"`
	Summary     Summary           `json:"summary"`
	Findings    []Group           `json:"findings"`
	Endpoints   []string          `json:"endpoints"`
	Labels      map[string]string `json:"labels,omitempty"` // endpoint -> how to show it, e.g. "gRPC users.v1.Users/GetUser"
	Drift       []model.Drift     `json:"drift,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
	Results     []model.Result    `json:"results"`
}

// Build groups the findings of a run.
func Build(results []model.Result, api *model.API, o Options) *Report {
	rep := &Report{
		Tool: "replayharness", Version: o.Version, Target: o.Target, Baseline: o.Baseline,
		StartedAt: o.Started, DurationMS: o.Duration.Milliseconds(), Interrupted: o.Interrupted,
		Findings: []Group{}, Results: results,
	}
	if api != nil {
		rep.Drift, rep.Notes = api.Drift, api.Notes
		for _, ep := range api.Endpoints {
			if l := ep.Label(); l != ep.Key() {
				if rep.Labels == nil {
					rep.Labels = map[string]string{}
				}
				rep.Labels[ep.Key()] = l
			}
		}
	}
	type key struct{ ep, check, msg string }
	index := map[key]int{}
	worst := map[string]int{}
	seen := map[string]bool{}
	for _, r := range results {
		ep := r.Scenario.Endpoint
		if !seen[ep] {
			seen[ep] = true
			rep.Endpoints = append(rep.Endpoints, ep)
		}
		for _, f := range r.Findings {
			k := key{ep, f.Check, f.Message}
			if i, ok := index[k]; ok {
				rep.Findings[i].Count++
				continue
			}
			index[k] = len(rep.Findings)
			g := Group{Endpoint: ep, Check: f.Check, Severity: f.Severity, Message: f.Message, Count: 1,
				Example: r.Scenario.Name, ScenarioID: r.Scenario.ID, Detail: f.Detail, Repro: Command(o, r.Scenario)}
			if len(r.Observed) > 0 {
				g.Status = r.Observed[0].Status
			}
			rep.Findings = append(rep.Findings, g)
			if f.Severity.Rank() > worst[ep] {
				worst[ep] = f.Severity.Rank()
			}
		}
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Endpoint != b.Endpoint {
			if worst[a.Endpoint] != worst[b.Endpoint] {
				return worst[a.Endpoint] > worst[b.Endpoint]
			}
			return a.Endpoint < b.Endpoint
		}
		if a.Severity != b.Severity {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		return a.Count > b.Count
	})
	sort.Strings(rep.Endpoints)
	rep.Summary = Summary{Scenarios: len(results), Endpoints: len(rep.Endpoints)}
	for _, g := range rep.Findings {
		switch g.Severity {
		case model.SevError:
			rep.Summary.Errors++
		case model.SevWarning:
			rep.Summary.Warnings++
		case model.SevInfo:
			rep.Summary.Info++
		}
	}
	return rep
}

// Failed reports whether the run should fail: failOn is "error", "warning"
// or "never".
func (r *Report) Failed(failOn string) bool {
	switch failOn {
	case "never":
		return false
	case "warning":
		return r.Summary.Errors+r.Summary.Warnings > 0
	}
	return r.Summary.Errors > 0
}

// Command renders a scenario as a shell command that reproduces it: curl for
// HTTP and GraphQL, grpcurl for gRPC, websocat for WebSocket. Credentials are
// redacted.
func Command(o Options, sc model.Scenario) string {
	switch sc.Request.Protocol {
	case model.ProtoGRPC:
		return grpcurl(o, sc)
	case model.ProtoWebSocket:
		return websocat(o, sc)
	}
	return Curl(o, sc)
}

// headerArgs lists the headers to show, global ones first, with credentials
// redacted.
func headerArgs(o Options, sc model.Scenario, flag string, lower bool) []string {
	var parts, names []string
	for k := range o.Headers {
		if sc.StripAuth && o.AuthHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		if _, overridden := sc.Request.Headers[k]; !overridden {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	name := func(k string) string {
		if lower {
			return strings.ToLower(k)
		}
		return k
	}
	for _, k := range names {
		v := o.Headers[k]
		if o.AuthHeaders[http.CanonicalHeaderKey(k)] {
			v = "<redacted>"
		}
		parts = append(parts, flag, shellQuote(name(k)+": "+v))
	}
	names = names[:0]
	for k := range sc.Request.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		parts = append(parts, flag, shellQuote(name(k)+": "+sc.Request.Headers[k]))
	}
	return parts
}

func grpcurl(o Options, sc model.Scenario) string {
	addr := o.GRPCTarget
	parts := []string{"grpcurl"}
	switch {
	case strings.HasPrefix(addr, "grpcs://"):
		addr = strings.TrimPrefix(addr, "grpcs://")
	default:
		addr = strings.TrimPrefix(addr, "grpc://")
		parts = append(parts, "-plaintext")
	}
	if addr == "" {
		if u, err := url.Parse(o.Target); err == nil {
			addr = u.Host
		}
	}
	parts = append(parts, headerArgs(o, sc, "-H", true)...)
	body := sc.Request.Body
	if body == "" {
		body = "{}"
	}
	method := strings.TrimPrefix(sc.Request.Path, "/")
	if len(body) > 2000 {
		parts = append(parts, "-d", "@", addr, method, "< message.json", fmt.Sprintf("# %d-byte message: see scenario %s in the JSON report", len(body), sc.ID))
		return strings.Join(parts, " ")
	}
	parts = append(parts, "-d", shellQuote(body), addr, method)
	return strings.Join(parts, " ")
}

func websocat(o Options, sc model.Scenario) string {
	u := o.Target
	if base, err := url.Parse(o.Target); err == nil {
		u = wsURL(base, sc.Request)
	}
	var msgs []any
	_ = json.Unmarshal([]byte(sc.Request.Body), &msgs)
	var lines []string
	for _, m := range msgs {
		text, ok := m.(string)
		if !ok {
			b, _ := json.Marshal(m)
			text = string(b)
		}
		if len(text) > 2000 {
			return fmt.Sprintf("websocat %s  # sends a %d-byte message: see scenario %s in the JSON report", shellQuote(u), len(text), sc.ID)
		}
		lines = append(lines, shellQuote(text))
	}
	parts := []string{}
	if len(lines) > 0 {
		parts = append(parts, "printf '%s\\n'", strings.Join(lines, " "), "|")
	}
	parts = append(parts, "websocat")
	parts = append(parts, headerArgs(o, sc, "-H", false)...)
	return strings.Join(append(parts, shellQuote(u)), " ")
}

func wsURL(base *url.URL, req model.Request) string {
	u := *base
	u.Scheme = "ws"
	if base.Scheme == "https" {
		u.Scheme = "wss"
	}
	s := strings.TrimRight(u.String(), "/") + req.Path
	if req.Query != "" {
		s += "?" + req.Query
	}
	return s
}

// Curl renders a scenario as a curl command that reproduces it. Credentials
// are redacted.
func Curl(o Options, sc model.Scenario) string {
	u := strings.TrimRight(o.Target, "/") + sc.Request.Path
	if sc.Request.Query != "" {
		u += "?" + sc.Request.Query
	}
	parts := []string{"curl", "-sS", "-i"}
	if sc.Request.Method != http.MethodGet || sc.Request.Body != "" {
		parts = append(parts, "-X", sc.Request.Method)
	}
	parts = append(parts, shellQuote(u))
	parts = append(parts, headerArgs(o, sc, "-H", false)...)
	body := sc.Request.BodyBytes()
	switch {
	case len(body) == 0:
	case sc.Request.BodyEncoding == "base64" || !utf8.Valid(body) || strings.ContainsRune(string(body), 0):
		parts = append(parts, "--data-binary", "@body.bin", fmt.Sprintf("# binary %d-byte body: see the JSON report", len(body)))
	case len(body) > 2000:
		parts = append(parts, "--data-binary", "@body.json", fmt.Sprintf("# %d-byte body: see scenario %s in the JSON report", len(body), sc.ID))
	default:
		parts = append(parts, "--data-raw", shellQuote(string(body)))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteJSON writes the report, trimming large bodies.
func WriteJSON(path string, r *Report) error {
	trimmed := *r
	trimmed.Results = make([]model.Result, len(r.Results))
	for i, res := range r.Results {
		res.Scenario.Request.Body = trim(res.Scenario.Request.Body, 4096)
		obs := make([]model.Observed, len(res.Observed))
		for j, o := range res.Observed {
			o.Body = trim(o.Body, 4096)
			obs[j] = o
		}
		res.Observed = obs
		if res.Baseline != nil {
			b := *res.Baseline
			b.Body = trim(b.Body, 4096)
			res.Baseline = &b
		}
		trimmed.Results[i] = res
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&trimmed); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…(%d bytes omitted)", len(s)-cut)
}

// TextOptions controls terminal output.
type TextOptions struct {
	Color   bool
	Verbose bool // also show info-level findings and full details
}

type palette struct{ red, yellow, dim, bold, green, reset string }

func colors(on bool) palette {
	if !on {
		return palette{}
	}
	return palette{red: "\033[31m", yellow: "\033[33m", dim: "\033[2m", bold: "\033[1m", green: "\033[32m", reset: "\033[0m"}
}

// WriteText writes a human-readable report.
func WriteText(w io.Writer, r *Report, o TextOptions) {
	c := colors(o.Color)
	fmt.Fprintf(w, "\n%sReplayHarness %s%s · %d scenarios · %d endpoints · %s", c.bold, r.Version, c.reset,
		r.Summary.Scenarios, r.Summary.Endpoints, r.Target)
	if r.Baseline != "" {
		fmt.Fprintf(w, " vs baseline %s", r.Baseline)
	}
	fmt.Fprintf(w, " · %s\n", (time.Duration(r.DurationMS) * time.Millisecond).Round(10*time.Millisecond))
	if r.Interrupted {
		fmt.Fprintf(w, "%sInterrupted: showing the scenarios that finished.%s\n", c.yellow, c.reset)
	}

	byEndpoint := map[string][]Group{}
	var order []string
	for _, g := range r.Findings {
		if g.Severity == model.SevInfo && !o.Verbose {
			continue
		}
		if _, ok := byEndpoint[g.Endpoint]; !ok {
			order = append(order, g.Endpoint)
		}
		byEndpoint[g.Endpoint] = append(byEndpoint[g.Endpoint], g)
	}
	for _, ep := range order {
		fmt.Fprintf(w, "\n%s%s%s\n", c.bold, r.label(ep), c.reset)
		for _, g := range byEndpoint[ep] {
			mark, col := "✗", c.red
			switch g.Severity {
			case model.SevWarning:
				mark, col = "!", c.yellow
			case model.SevInfo:
				mark, col = "·", c.dim
			}
			count := ""
			if g.Count > 1 {
				count = fmt.Sprintf("  %s×%d%s", c.dim, g.Count, c.reset)
			}
			fmt.Fprintf(w, "  %s%s %-7s%s %-19s %s%s\n", col, mark, g.Severity, c.reset, g.Check, g.Message, count)
			fmt.Fprintf(w, "    %se.g. %s%s\n", c.dim, g.Example, c.reset)
			if g.Detail != "" && g.Detail != g.Example && (o.Verbose || g.Severity == model.SevError) {
				fmt.Fprintf(w, "    %s%s%s\n", c.dim, oneLine(g.Detail, o.Verbose), c.reset)
			}
			if g.Severity != model.SevInfo || o.Verbose {
				fmt.Fprintf(w, "    %s%s%s\n", c.dim, g.Repro, c.reset)
			}
		}
	}

	clean := 0
	var cleanNames []string
	for _, ep := range r.Endpoints {
		if _, ok := byEndpoint[ep]; !ok {
			clean++
			cleanNames = append(cleanNames, r.label(ep))
		}
	}
	if clean > 0 {
		list := strings.Join(cleanNames, ", ")
		if len(cleanNames) > 6 {
			list = strings.Join(cleanNames[:6], ", ") + fmt.Sprintf(", and %d more", len(cleanNames)-6)
		}
		fmt.Fprintf(w, "\n%s✓ %d endpoint(s) with no problems:%s %s\n", c.green, clean, c.reset, list)
	}

	if len(r.Drift) > 0 {
		fmt.Fprintf(w, "\n%sSpec drift%s\n", c.bold, c.reset)
		for _, d := range r.Drift {
			label := "in the code, missing from the spec"
			if d.Kind == "unimplemented" {
				label = "in the spec, no route found in the code"
			}
			fmt.Fprintf(w, "  %-7s %-28s %s  %s%s%s\n", d.Method, d.Path, label, c.dim, d.Where, c.reset)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "%snote: %s%s\n", c.dim, n, c.reset)
	}

	fmt.Fprintf(w, "\n%s%d error(s)%s · %s%d warning(s)%s · %d note(s)", c.red, r.Summary.Errors, c.reset,
		c.yellow, r.Summary.Warnings, c.reset, r.Summary.Info)
	if r.Summary.Info > 0 && !o.Verbose {
		fmt.Fprintf(w, " %s(-v shows notes)%s", c.dim, c.reset)
	}
	fmt.Fprintln(w)
}

func (r *Report) label(ep string) string {
	if l, ok := r.Labels[ep]; ok {
		return l
	}
	return ep
}

func oneLine(s string, full bool) string {
	s = strings.Join(strings.Fields(s), " ")
	if !full && len(s) > 160 {
		cut := 160
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
