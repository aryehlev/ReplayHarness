// Package check decides what is wrong with a response: crashes, 5xx errors,
// invalid input that was accepted, responses that break the documented
// contract, leaked stack traces, slow answers, and differences from a
// baseline server or from recorded traffic.
package check

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Options tunes the checks.
type Options struct {
	Slow   time.Duration   // responses slower than this get a warning; 0 turns the check off
	Exact  bool            // replay: compare JSON values, not just their shape
	Ignore map[string]bool // replay with Exact: JSON keys whose values may change
}

// DefaultIgnore lists keys whose values usually change between runs.
var DefaultIgnore = []string{
	"id", "uuid", "created_at", "createdAt", "updated_at", "updatedAt", "timestamp", "time", "date",
	"request_id", "requestId", "trace_id", "traceId", "etag", "expires_at", "expiresAt", "token",
}

var phrases = map[string]string{
	model.CatHappy:       "a valid request",
	model.CatAuth:        "a request without valid credentials",
	model.CatMissing:     "a request missing a required field",
	model.CatWrongType:   "wrong-type input",
	model.CatNull:        "null input",
	model.CatBoundary:    "boundary input",
	model.CatFormat:      "badly formatted input",
	model.CatExtreme:     "extreme input",
	model.CatBodyShape:   "a malformed or unusual body",
	model.CatPathParam:   "an unusual path parameter",
	model.CatQuery:       "unusual query parameters",
	model.CatHeaders:     "unusual headers",
	model.CatMethod:      "an undefined method",
	model.CatConsistency: "a repeated GET",
	model.CatReplay:      "replayed traffic",
}

func describe(sc model.Scenario) string {
	if sc.Category == model.CatHappy && sc.Expect != model.ExpectSuccess {
		return "the baseline request"
	}
	if p, ok := phrases[sc.Category]; ok {
		return p
	}
	return sc.Category + " input"
}

// Evaluate returns the findings for one scenario. obs holds one response per
// repeat; base is the baseline server's response, if one was given.
func Evaluate(sc model.Scenario, ep *model.Endpoint, obs []model.Observed, base *model.Observed, opts Options) []model.Finding {
	var out []model.Finding
	add := func(check string, sev model.Severity, msg, detail string) {
		out = append(out, model.Finding{Check: check, Severity: sev, Message: msg, Detail: detail})
	}
	if len(obs) == 0 {
		return nil
	}
	o := obs[0]
	what := describe(sc)

	if o.Status == 0 {
		if o.Timeout {
			add("timeout", model.SevError, "no response before the timeout on "+what, o.Error)
		} else {
			add("connection", model.SevError, "connection failed on "+what+" (the server may have crashed or dropped the request)", o.Error)
		}
		return out
	}
	if o.Error != "" {
		add("connection", model.SevError, "connection dropped while the response body was being read, on "+what, o.Error)
	}

	switch {
	case sc.Expect == model.ExpectRecorded && sc.Recorded != nil && sc.Recorded.Status != o.Status:
		sev := model.SevError
		if o.Status < 500 && sc.Recorded.Status >= 500 {
			sev = model.SevInfo // it used to fail and now does not
		}
		add("regression", sev, fmt.Sprintf("recorded %d, now %d", sc.Recorded.Status, o.Status), snippet(o.Body))
	case o.Status >= 500:
		add("server-error", model.SevError, fmt.Sprintf("%d on %s", o.Status, what), snippet(o.Body))
	default:
		expectation(sc, o, what, add)
		if ep != nil {
			documented(sc, ep, o, add)
			contract(ep, o, add)
		}
		if sc.Expect == model.ExpectRecorded && sc.Recorded != nil {
			recorded(sc, o, opts, add)
		}
	}
	leak(o, add)
	if opts.Slow > 0 && o.DurationMS > float64(opts.Slow.Milliseconds()) {
		add("slow", model.SevWarning, fmt.Sprintf("slower than %s on %s", opts.Slow, what), fmt.Sprintf("%.0fms", o.DurationMS))
	}
	if len(obs) > 1 {
		consistency(obs, add)
	}
	if base != nil {
		baseline(o, *base, add)
	}
	return out
}

type addFunc func(check string, sev model.Severity, msg, detail string)

func expectation(sc model.Scenario, o model.Observed, what string, add addFunc) {
	switch sc.Expect {
	case model.ExpectSuccess:
		if o.Status < 400 {
			return
		}
		sev := model.SevWarning
		hint := "the server rejected input that satisfies the spec, or the generated data is not valid for it"
		switch o.Status {
		case 401, 403:
			sev, hint = model.SevInfo, "pass credentials with -H 'Authorization: Bearer ...' to test past authentication"
		case 404:
			sev, hint = model.SevInfo, "the generated id or path probably does not exist; seed data or a recorded sample would get further"
		}
		add("rejected-valid", sev, fmt.Sprintf("%s got %d", what, o.Status), hint)
	case model.ExpectReject:
		if o.Status < 200 || o.Status >= 300 {
			return
		}
		sev := model.SevWarning
		if sc.Strict {
			sev = model.SevError
		}
		switch sc.Category {
		case model.CatAuth:
			add("auth-bypass", model.SevError, fmt.Sprintf("returned %d to a request without valid credentials, but the spec requires authentication", o.Status), "")
		case model.CatMethod:
			add("undefined-method", sev, fmt.Sprintf("%s is not defined for this path but returned %d", sc.Request.Method, o.Status), "")
		default:
			add("accepted-invalid", sev, fmt.Sprintf("accepted %s with %d", what, o.Status), "")
		}
	}
}

func responseSpec(ep *model.Endpoint, status int) *model.ResponseSpec {
	code := strconv.Itoa(status)
	if rs, ok := ep.Responses[code]; ok {
		return rs
	}
	if rs, ok := ep.Responses[code[:1]+"XX"]; ok {
		return rs
	}
	return ep.Responses["DEFAULT"]
}

func documented(sc model.Scenario, ep *model.Endpoint, o model.Observed, add addFunc) {
	if len(ep.Responses) == 0 || sc.Category == model.CatMethod || sc.Category == model.CatAuth || sc.Category == model.CatReplay {
		return
	}
	if responseSpec(ep, o.Status) == nil {
		add("undocumented-status", model.SevWarning, fmt.Sprintf("returned %d, which the spec does not list for this endpoint", o.Status), "")
	}
}

func contract(ep *model.Endpoint, o model.Observed, add addFunc) {
	if o.Status < 200 || o.Status >= 300 || o.Status == 204 {
		return
	}
	rs := responseSpec(ep, o.Status)
	if rs == nil {
		return
	}
	ct := header(o.Headers, "Content-Type")
	wantJSON := strings.Contains(rs.ContentType, "json")
	body := strings.TrimSpace(o.Body)
	if wantJSON && body != "" && !strings.Contains(strings.ToLower(ct), "json") {
		add("content-type", model.SevWarning, fmt.Sprintf("documented as %s but served as %q", rs.ContentType, ct), "")
	}
	if rs.Schema == nil || o.Truncated || !wantJSON {
		return
	}
	if body == "" {
		add("schema-mismatch", model.SevError, fmt.Sprintf("returned %d with an empty body, but the spec documents a JSON body", o.Status), "")
		return
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		add("schema-mismatch", model.SevError, "response body is not valid JSON", snippet(o.Body))
		return
	}
	if errs := Validate(rs.Schema, v); len(errs) > 0 {
		if len(errs) > 3 {
			errs = append(errs[:3], fmt.Sprintf("and %d more", len(errs)-3))
		}
		add("schema-mismatch", model.SevError, "response does not match the documented schema: "+strings.Join(errs, "; "), snippet(o.Body))
	}
}

var leakPatterns = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`Traceback \(most recent call last\)`), "a Python traceback"},
	{regexp.MustCompile(`goroutine \d+ \[`), "a Go stack trace"},
	{regexp.MustCompile(`\bat [\w$.]+\([\w$]+\.(?:java|kt|scala):\d+\)`), "a JVM stack trace"},
	{regexp.MustCompile(`\n\s+at .+\(.+:\d+:\d+\)`), "a JavaScript stack trace"},
	{regexp.MustCompile(`\bat .+ in .+\.cs:line \d+`), "a .NET stack trace"},
	{regexp.MustCompile(`\.rb:\d+:in [` + "`" + `']`), "a Ruby backtrace"},
	{regexp.MustCompile(`(?m)^#\d+ /.+\.php\(\d+\)`), "a PHP stack trace"},
	{regexp.MustCompile(`(?i)SQLSTATE\[|syntax error at or near|ORA-\d{5}|SQLITE_ERROR|You have an error in your SQL syntax`), "a database error"},
}

func leak(o model.Observed, add addFunc) {
	for _, p := range leakPatterns {
		if p.re.MatchString(o.Body) {
			add("error-leak", model.SevWarning, "response body contains "+p.what, snippet(o.Body))
			return
		}
	}
}

func consistency(obs []model.Observed, add addFunc) {
	first := obs[0]
	for _, o := range obs[1:] {
		switch {
		case o.Status == 0:
			add("inconsistent", model.SevError, "the same GET failed when sent again", o.Error)
			return
		case o.Status >= 500 && first.Status < 500:
			add("inconsistent", model.SevError, fmt.Sprintf("the same GET returned %d, then %d", first.Status, o.Status), snippet(o.Body))
			return
		case o.Status != first.Status:
			add("inconsistent", model.SevWarning, fmt.Sprintf("the same GET returned %d, then %d", first.Status, o.Status), "")
			return
		}
		if diffs := jsonShapeDiff(first.Body, o.Body); len(diffs) > 0 {
			add("inconsistent", model.SevWarning, "the same GET returned differently shaped bodies: "+strings.Join(diffs, "; "), "")
			return
		}
	}
}

func baseline(o, b model.Observed, add addFunc) {
	if b.Status == 0 {
		add("divergence", model.SevInfo, "the baseline server did not answer", b.Error)
		return
	}
	if o.Status != b.Status {
		add("divergence", model.SevError, fmt.Sprintf("baseline returned %d, target returned %d", b.Status, o.Status), snippet(o.Body))
		return
	}
	if diffs := jsonShapeDiff(b.Body, o.Body); len(diffs) > 0 {
		add("divergence", model.SevWarning, "response shape differs from the baseline: "+strings.Join(diffs, "; "), "")
	}
}

func recorded(sc model.Scenario, o model.Observed, opts Options, add addFunc) {
	recBody := string(sc.Recorded.BodyBytes())
	if diffs := jsonShapeDiff(recBody, o.Body); len(diffs) > 0 {
		add("regression", model.SevWarning, "response shape changed: "+strings.Join(diffs, "; "), "")
		return
	}
	if !opts.Exact {
		return
	}
	var a, b any
	if json.Unmarshal([]byte(recBody), &a) != nil || json.Unmarshal([]byte(o.Body), &b) != nil {
		if strings.TrimSpace(recBody) != strings.TrimSpace(o.Body) {
			add("regression", model.SevWarning, "response body changed", "")
		}
		return
	}
	var diffs []string
	valueDiff(a, b, "$", opts.Ignore, &diffs)
	if len(diffs) > 0 {
		add("regression", model.SevWarning, "response values changed: "+strings.Join(diffs, "; "), "")
	}
}

// jsonShapeDiff compares the structure (keys and types) of two JSON bodies.
// Non-JSON bodies are not compared.
func jsonShapeDiff(a, b string) []string {
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return nil
	}
	var out []string
	shapeDiff(va, vb, "$", &out)
	return out
}

func shapeDiff(a, b any, path string, out *[]string) {
	if len(*out) >= 5 {
		return
	}
	ta, tb := jsonType(a), jsonType(b)
	if ta != tb {
		if ta != "null" && tb != "null" {
			*out = append(*out, fmt.Sprintf("%s was %s, now %s", path, ta, tb))
		}
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv := b.(map[string]any)
		for _, k := range unionKeys(av, bv) {
			_, inA := av[k]
			_, inB := bv[k]
			switch {
			case inA && !inB:
				*out = append(*out, fmt.Sprintf("%s.%s is gone", path, k))
			case !inA && inB:
				*out = append(*out, fmt.Sprintf("%s.%s is new", path, k))
			default:
				shapeDiff(av[k], bv[k], path+"."+k, out)
			}
			if len(*out) >= 5 {
				return
			}
		}
	case []any:
		bv := b.([]any)
		if len(av) > 0 && len(bv) > 0 {
			shapeDiff(av[0], bv[0], path+"[0]", out)
		}
	}
}

func valueDiff(a, b any, path string, ignore map[string]bool, out *[]string) {
	if len(*out) >= 5 {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, path+" changed type")
			return
		}
		for _, k := range unionKeys(av, bv) {
			if ignore[k] {
				continue
			}
			valueDiff(av[k], bv[k], path+"."+k, ignore, out)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, path+" changed type")
			return
		}
		if len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s had %d items, now %d", path, len(av), len(bv)))
			return
		}
		for i := range av {
			valueDiff(av[i], bv[i], fmt.Sprintf("%s[%d]", path, i), ignore, out)
		}
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			*out = append(*out, fmt.Sprintf("%s was %s, now %s", path, short(a), short(b)))
		}
	}
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	var keys []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

func short(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 40 {
		return string(b[:37]) + "..."
	}
	return string(b)
}

func header(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// snippet trims a body for display.
func snippet(body string) string {
	body = strings.TrimSpace(body)
	const max = 600
	if len(body) <= max {
		return body
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + fmt.Sprintf("… (%d more bytes)", len(body)-cut)
}
