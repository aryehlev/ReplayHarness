package replay

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aryehlev/ReplayHarness/internal/check"
	"github.com/aryehlev/ReplayHarness/internal/model"
	"github.com/aryehlev/ReplayHarness/internal/report"
	"github.com/aryehlev/ReplayHarness/internal/runner"
)

// StepResult is what happened in one step.
type StepResult struct {
	Index    int             `json:"index"`
	Name     string          `json:"name,omitempty"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Status   int             `json:"status,omitempty"`
	MS       float64         `json:"ms"`
	Failures []string        `json:"failures,omitempty"`
	Findings []model.Finding `json:"findings,omitempty"`
	Body     string          `json:"body,omitempty"`
	Curl     string          `json:"curl,omitempty"`
}

// Failed reports whether the step broke an expectation or hit an error.
func (s StepResult) Failed() bool {
	if len(s.Failures) > 0 {
		return true
	}
	for _, f := range s.Findings {
		if f.Severity == model.SevError {
			return true
		}
	}
	return false
}

// Result is what happened in one replay.
type Result struct {
	Name   string       `json:"name"`
	Why    string       `json:"why,omitempty"`
	File   string       `json:"file,omitempty"`
	Passed bool         `json:"passed"`
	Steps  []StepResult `json:"steps"`
}

// Runner runs replays against one target.
type Runner struct {
	Client  *runner.Client
	Headers map[string]string // the client's global headers, shown redacted in curl commands
	Slow    time.Duration
}

// Run runs the replays one after another. Steps inside a replay run in order;
// a step whose request gets no response, or whose saved value is missing,
// ends its replay.
func (r *Runner) Run(ctx context.Context, replays []Replay) []Result {
	out := make([]Result, 0, len(replays))
	for _, f := range replays {
		if ctx.Err() != nil {
			break
		}
		out = append(out, r.runOne(ctx, f))
	}
	return out
}

func (r *Runner) runOne(ctx context.Context, f Replay) Result {
	res := Result{Name: f.Name, Why: f.Why, File: f.File, Passed: true}
	vars := BuiltinVars()
	for i, st := range f.Steps {
		sr := StepResult{Index: i + 1, Name: st.Name, Method: st.Method, Path: st.Path}
		req, err := BuildRequest(st, vars)
		if err != nil {
			sr.Failures = append(sr.Failures, err.Error())
			res.Steps, res.Passed = append(res.Steps, sr), false
			break
		}
		sr.Path = req.Path
		if req.Query != "" {
			sr.Path += "?" + req.Query
		}
		sr.Curl = report.Curl(report.Options{Target: r.Client.Target(), Headers: r.Headers, AuthHeaders: r.Client.AuthHeaders()},
			model.Scenario{Request: req, StripAuth: st.NoAuth})

		obs := r.Client.Do(ctx, req, st.NoAuth)
		sr.Status, sr.MS = obs.Status, obs.DurationMS
		match, _ := statusMatcher(st.Expect.Status)
		sr.Findings = check.Basic(obs, obs.Status >= 500 && match != nil && match(obs.Status), r.Slow)
		stop := obs.Status == 0
		if !stop {
			if exp, err := expandExpect(st.Expect, vars); err != nil {
				sr.Failures = append(sr.Failures, "expect: "+err.Error())
			} else {
				sr.Failures = append(sr.Failures, assertResponse(exp, obs)...)
			}
			names := make([]string, 0, len(st.Save))
			for n := range st.Save {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, name := range names {
				v, ok := extract(obs, st.Save[name])
				if !ok {
					sr.Failures = append(sr.Failures, fmt.Sprintf("could not save %s: %s is not in the response", name, st.Save[name]))
					stop = true
					continue
				}
				vars[name] = v
			}
		}
		if sr.Failed() {
			res.Passed = false
			sr.Body = check.Snippet(obs.Body)
		} else {
			sr.Curl = ""
		}
		res.Steps = append(res.Steps, sr)
		if stop {
			break
		}
	}
	return res
}

// BuiltinVars returns the variables every replay starts with.
func BuiltinVars() map[string]any {
	return map[string]any{"unique": uniqueID(), "timestamp": time.Now().UnixMilli()}
}

var varRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// substitute replaces {{name}} in s. escape encodes each value (for paths and
// query strings).
func substitute(s string, vars map[string]any, escape func(string) string) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		str := scalar(v)
		if escape != nil {
			str = escape(str)
		}
		return str
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unknown variable {{%s}}: save it in an earlier step (save: {%s: $.id})", missing[0], missing[0])
	}
	return out, nil
}

// substituteValue replaces variables inside a JSON value. A string that is
// exactly "{{name}}" takes the saved value with its type (a number stays a
// number).
func substituteValue(v any, vars map[string]any) (any, error) {
	switch t := v.(type) {
	case string:
		if m := varRe.FindStringSubmatch(t); m != nil && m[0] == strings.TrimSpace(t) {
			if val, ok := vars[m[1]]; ok {
				return val, nil
			}
		}
		return substitute(t, vars, nil)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			nv, err := substituteValue(val, vars)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			nv, err := substituteValue(val, vars)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	}
	return v, nil
}

// expandExpect fills saved variables into the expected values.
func expandExpect(e Expect, vars map[string]any) (Expect, error) {
	out := e
	if len(e.JSON) > 0 {
		out.JSON = make(map[string]any, len(e.JSON))
		for k, v := range e.JSON {
			nv, err := substituteValue(v, vars)
			if err != nil {
				return e, err
			}
			out.JSON[k] = nv
		}
	}
	if len(e.Headers) > 0 {
		out.Headers = make(map[string]string, len(e.Headers))
		for k, v := range e.Headers {
			nv, err := substitute(v, vars, nil)
			if err != nil {
				return e, err
			}
			out.Headers[k] = nv
		}
	}
	for _, list := range []*[]string{&out.Contains, &out.NotContains} {
		expanded := make([]string, len(*list))
		for i, v := range *list {
			nv, err := substitute(v, vars, nil)
			if err != nil {
				return e, err
			}
			expanded[i] = nv
		}
		*list = expanded
	}
	return out, nil
}

// BuildRequest turns a step into a request, filling in saved variables.
func BuildRequest(st Step, vars map[string]any) (model.Request, error) {
	req := model.Request{Method: st.Method, Headers: map[string]string{}}
	raw := st.Path
	// A path that starts with a saved path (e.g. a Location header) uses it as is.
	if m := varRe.FindStringSubmatchIndex(raw); m != nil && m[0] == 0 {
		if v, ok := vars[raw[m[2]:m[3]]]; ok && strings.HasPrefix(scalar(v), "/") {
			raw = scalar(v) + raw[m[1]:]
		}
	}
	path, rawQuery, _ := strings.Cut(raw, "?")
	p, err := substitute(path, vars, url.PathEscape)
	if err != nil {
		return req, err
	}
	if !strings.HasPrefix(p, "/") {
		return req, fmt.Errorf("path %q does not start with / after filling in variables; requests can only go to the configured target", p)
	}
	req.Path = p
	q, err := substitute(rawQuery, vars, url.QueryEscape)
	if err != nil {
		return req, err
	}
	if len(st.Query) > 0 {
		vals := url.Values{}
		for k, v := range st.Query {
			items, ok := v.([]any)
			if !ok {
				items = []any{v}
			}
			for _, item := range items {
				s, err := substitute(scalar(item), vars, nil)
				if err != nil {
					return req, err
				}
				vals.Add(k, s)
			}
		}
		if q != "" {
			q += "&"
		}
		q += vals.Encode()
	}
	req.Query = q
	for k, v := range st.Headers {
		s, err := substitute(v, vars, nil)
		if err != nil {
			return req, err
		}
		req.Headers[k] = s
	}
	switch {
	case st.JSON != nil:
		v, err := substituteValue(st.JSON, vars)
		if err != nil {
			return req, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return req, err
		}
		req.Body = string(b)
		if !hasHeader(req.Headers, "Content-Type") {
			req.Headers["Content-Type"] = "application/json"
		}
	case st.Body != nil:
		b, err := substitute(*st.Body, vars, nil)
		if err != nil {
			return req, err
		}
		req.Body = b
		if t := strings.TrimSpace(b); !hasHeader(req.Headers, "Content-Type") && (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) {
			req.Headers["Content-Type"] = "application/json"
		}
	}
	if len(req.Headers) == 0 {
		req.Headers = nil
	}
	return req, nil
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// statusMatcher turns 200, [200, 201], "2xx" or "404" into a check. A nil
// spec returns a nil matcher (any status below 500 passes).
func statusMatcher(spec any) (func(int) bool, error) {
	if spec == nil {
		return nil, nil
	}
	items, ok := spec.([]any)
	if !ok {
		items = []any{spec}
	}
	var checks []func(int) bool
	for _, item := range items {
		s := strings.ToLower(strings.TrimSpace(scalar(item)))
		switch {
		case len(s) == 3 && s[1:] == "xx" && s[0] >= '1' && s[0] <= '5':
			class := int(s[0] - '0')
			checks = append(checks, func(code int) bool { return code/100 == class })
		default:
			n, err := strconv.Atoi(s)
			if err != nil || n < 100 || n > 599 {
				return nil, fmt.Errorf("expect.status %v: use a code like 404, a class like 4xx, or a list of them", item)
			}
			checks = append(checks, func(code int) bool { return code == n })
		}
	}
	return func(code int) bool {
		for _, c := range checks {
			if c(code) {
				return true
			}
		}
		return false
	}, nil
}

func assertResponse(e Expect, o model.Observed) []string {
	var fails []string
	if match, _ := statusMatcher(e.Status); match != nil && !match(o.Status) {
		fails = append(fails, fmt.Sprintf("status %d, expected %s", o.Status, scalar(e.Status)))
	}
	if len(e.JSON) > 0 {
		var body any
		if err := json.Unmarshal([]byte(o.Body), &body); err != nil {
			fails = append(fails, "expected a JSON body, got: "+check.Snippet(o.Body))
		} else {
			paths := make([]string, 0, len(e.JSON))
			for p := range e.JSON {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			for _, p := range paths {
				if msg := assertJSON(body, p, e.JSON[p]); msg != "" {
					fails = append(fails, msg)
				}
			}
		}
	}
	names := make([]string, 0, len(e.Headers))
	for n := range e.Headers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		want, got := e.Headers[n], headerOf(o.Headers, n)
		switch {
		case want == "<absent>":
			if got != "" {
				fails = append(fails, fmt.Sprintf("header %s should be absent, got %q", n, got))
			}
		case got == "":
			fails = append(fails, fmt.Sprintf("header %s is missing", n))
		case want != "<exists>" && !strings.Contains(strings.ToLower(got), strings.ToLower(want)):
			fails = append(fails, fmt.Sprintf("header %s is %q, expected it to contain %q", n, got, want))
		}
	}
	for _, s := range e.Contains {
		if !strings.Contains(o.Body, s) {
			fails = append(fails, fmt.Sprintf("body does not contain %q", s))
		}
	}
	for _, s := range e.NotContains {
		if strings.Contains(o.Body, s) {
			fails = append(fails, fmt.Sprintf("body contains %q", s))
		}
	}
	if e.MaxMS > 0 && o.DurationMS > e.MaxMS {
		fails = append(fails, fmt.Sprintf("took %.0fms, expected at most %.0fms", o.DurationMS, e.MaxMS))
	}
	return fails
}

// Matchers usable as expected JSON values.
var matchers = map[string]func(v any, found bool) bool{
	"<exists>":  func(v any, found bool) bool { return found },
	"<absent>":  func(v any, found bool) bool { return !found },
	"<null>":    func(v any, found bool) bool { return found && v == nil },
	"<string>":  func(v any, found bool) bool { _, ok := v.(string); return found && ok },
	"<number>":  func(v any, found bool) bool { _, ok := v.(float64); return found && ok },
	"<integer>": func(v any, found bool) bool { f, ok := v.(float64); return found && ok && f == float64(int64(f)) },
	"<bool>":    func(v any, found bool) bool { _, ok := v.(bool); return found && ok },
	"<array>":   func(v any, found bool) bool { _, ok := v.([]any); return found && ok },
	"<object>":  func(v any, found bool) bool { _, ok := v.(map[string]any); return found && ok },
	"<nonempty>": func(v any, found bool) bool {
		return found && v != nil && v != "" && fmt.Sprint(v) != "[]" && fmt.Sprint(v) != "map[]"
	},
}

func assertJSON(body any, path string, want any) string {
	got, found := lookup(body, path)
	if s, ok := want.(string); ok {
		if m, ok := matchers[s]; ok {
			if !m(got, found) {
				if !found {
					return fmt.Sprintf("%s is missing, expected %s", path, s)
				}
				return fmt.Sprintf("%s is %s, expected %s", path, compact(got), s)
			}
			return ""
		}
	}
	if !found {
		return fmt.Sprintf("%s is missing, expected %s", path, compact(want))
	}
	if compact(got) != compact(want) {
		return fmt.Sprintf("%s is %s, expected %s", path, compact(got), compact(want))
	}
	return ""
}

// compact renders a value as canonical JSON so that 1 (YAML int) and 1.0
// (JSON number) compare equal but "1" and 1 do not.
func compact(v any) string {
	b, err := json.Marshal(normalizeNumbers(v))
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}

func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeNumbers(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeNumbers(val)
		}
		return out
	}
	return v
}

var segRe = regexp.MustCompile(`\.([^.\[\]]+)|\[(\d+)\]|\["([^"]*)"\]`)

// lookup evaluates a small JSON path: $, .key, [0], ["key"], and a final
// .length on arrays, strings and objects.
func lookup(v any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "$") {
		path = "$." + path
	}
	rest := path[1:]
	cur := v
	for rest != "" {
		m := segRe.FindStringSubmatch(rest)
		if m == nil || !strings.HasPrefix(rest, m[0]) {
			return nil, false
		}
		bracketKey := strings.HasPrefix(m[0], `["`)
		rest = rest[len(m[0]):]
		if m[2] != "" && !bracketKey {
			i, _ := strconv.Atoi(m[2])
			a, ok := cur.([]any)
			if !ok || i >= len(a) {
				return nil, false
			}
			cur = a[i]
			continue
		}
		key := m[1]
		if bracketKey {
			key = m[3]
		}
		if obj, ok := cur.(map[string]any); ok {
			if val, has := obj[key]; has {
				cur = val
				continue
			}
		}
		if key == "length" && rest == "" && !bracketKey {
			switch t := cur.(type) {
			case []any:
				return float64(len(t)), true
			case string:
				return float64(len([]rune(t))), true
			case map[string]any:
				return float64(len(t)), true
			}
		}
		return nil, false
	}
	return cur, true
}

func extract(o model.Observed, src string) (any, bool) {
	switch {
	case src == "status":
		return float64(o.Status), true
	case strings.HasPrefix(strings.ToLower(src), "header:"):
		v := headerOf(o.Headers, strings.TrimSpace(src[len("header:"):]))
		return v, v != ""
	}
	var body any
	if json.Unmarshal([]byte(o.Body), &body) != nil {
		return nil, false
	}
	return lookup(body, src)
}

func headerOf(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = scalar(item)
		}
		return strings.Join(parts, " or ")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Summary counts passing and failing replays.
func Summary(results []Result) (passed, failed int) {
	for _, r := range results {
		if r.Passed {
			passed++
		} else {
			failed++
		}
	}
	return
}

// WriteText writes results for people and agents: failing replays in full,
// passing replays as one line each.
func WriteText(w io.Writer, results []Result, color bool) {
	red, green, dim, bold, reset := "", "", "", "", ""
	if color {
		red, green, dim, bold, reset = "\033[31m", "\033[32m", "\033[2m", "\033[1m", "\033[0m"
	}
	for _, r := range results {
		if r.Passed {
			fmt.Fprintf(w, "%s✓%s %s\n", green, reset, r.Name)
			continue
		}
		fmt.Fprintf(w, "%s✗ %s%s\n", red+bold, r.Name, reset)
		if r.Why != "" {
			fmt.Fprintf(w, "    %swhy: %s%s\n", dim, r.Why, reset)
		}
		for _, s := range r.Steps {
			mark := "ok"
			if s.Failed() {
				mark = red + "FAIL" + reset
			}
			status := "no response"
			if s.Status != 0 {
				status = strconv.Itoa(s.Status)
			}
			label := ""
			if s.Name != "" {
				label = " (" + s.Name + ")"
			}
			fmt.Fprintf(w, "    %d. %s %s → %s %s%s\n", s.Index, s.Method, s.Path, status, mark, label)
			for _, f := range s.Failures {
				fmt.Fprintf(w, "       - %s\n", f)
			}
			for _, f := range s.Findings {
				fmt.Fprintf(w, "       - %s %s: %s\n", f.Severity, f.Check, f.Message)
			}
			if s.Failed() {
				if s.Body != "" {
					fmt.Fprintf(w, "       %sbody: %s%s\n", dim, oneLine(s.Body), reset)
				}
				fmt.Fprintf(w, "       %s%s%s\n", dim, s.Curl, reset)
			}
		}
	}
	passed, failed := Summary(results)
	fmt.Fprintf(w, "\n%d passed, %d failed\n", passed, failed)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// WriteJUnit writes one test case per replay.
func WriteJUnit(path string, results []Result) error {
	var b strings.Builder
	_, failed := Summary(results)
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, "<testsuites name=\"replayharness\" tests=\"%d\" failures=\"%d\">\n", len(results), failed)
	fmt.Fprintf(&b, "  <testsuite name=\"replays\" tests=\"%d\" failures=\"%d\">\n", len(results), failed)
	for _, r := range results {
		secs := 0.0
		for _, s := range r.Steps {
			secs += s.MS / 1000
		}
		class := r.File
		if class == "" {
			class = "replays"
		}
		fmt.Fprintf(&b, "    <testcase name=%s classname=%s time=\"%.3f\"", attr(r.Name), attr(class), secs)
		if r.Passed {
			b.WriteString("/>\n")
			continue
		}
		var text strings.Builder
		WriteText(&text, []Result{r}, false)
		first := "replay failed"
		for _, s := range r.Steps {
			if s.Failed() {
				first = fmt.Sprintf("step %d: %s %s", s.Index, s.Method, s.Path)
				break
			}
		}
		b.WriteString(">\n      <failure message=" + attr(first) + " type=\"replay\">")
		_ = xml.EscapeText(&b, []byte(text.String()))
		b.WriteString("</failure>\n    </testcase>\n")
	}
	b.WriteString("  </testsuite>\n</testsuites>\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func attr(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return `"` + strings.ReplaceAll(b.String(), `"`, "&quot;") + `"`
}
