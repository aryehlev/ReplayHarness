package discover

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Exchange is one recorded request and its response.
type Exchange struct {
	Time       time.Time              `json:"time"`
	Request    model.Request          `json:"request"`
	Response   model.RecordedResponse `json:"response"`
	DurationMS float64                `json:"durationMs,omitempty"`
}

// LoadTraffic reads a JSONL file written by `replayharness record`, or a HAR
// file exported from browser dev tools or a proxy.
func LoadTraffic(path string) ([]Exchange, error) {
	if strings.HasSuffix(strings.ToLower(path), ".har") {
		return loadHAR(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Exchange
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ex Exchange
		if err := json.Unmarshal(line, &ex); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, ex)
	}
	return out, sc.Err()
}

type harHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harFile struct {
	Log struct {
		Entries []struct {
			StartedDateTime string  `json:"startedDateTime"`
			Time            float64 `json:"time"`
			Request         struct {
				Method   string      `json:"method"`
				URL      string      `json:"url"`
				Headers  []harHeader `json:"headers"`
				PostData *struct {
					MimeType string `json:"mimeType"`
					Text     string `json:"text"`
				} `json:"postData"`
			} `json:"request"`
			Response struct {
				Status  int         `json:"status"`
				Headers []harHeader `json:"headers"`
				Content struct {
					MimeType string `json:"mimeType"`
					Text     string `json:"text"`
					Encoding string `json:"encoding"`
				} `json:"content"`
			} `json:"response"`
		} `json:"entries"`
	} `json:"log"`
}

func loadHAR(path string) ([]Exchange, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var har harFile
	if err := json.Unmarshal(data, &har); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []Exchange
	for _, e := range har.Log.Entries {
		u, err := url.Parse(e.Request.URL)
		if err != nil {
			continue
		}
		h := http.Header{}
		for _, hh := range e.Request.Headers {
			if !strings.HasPrefix(hh.Name, ":") {
				h.Add(hh.Name, hh.Value)
			}
		}
		ex := Exchange{DurationMS: e.Time}
		ex.Time, _ = time.Parse(time.RFC3339Nano, e.StartedDateTime)
		ex.Request = model.Request{Method: e.Request.Method, Path: u.EscapedPath(), Query: u.RawQuery, Headers: SanitizeHeaders(h, false)}
		if e.Request.PostData != nil {
			ex.Request.Body = e.Request.PostData.Text
			if ex.Request.Headers == nil {
				ex.Request.Headers = map[string]string{}
			}
			if _, ok := ex.Request.Headers["Content-Type"]; !ok && e.Request.PostData.MimeType != "" {
				ex.Request.Headers["Content-Type"] = e.Request.PostData.MimeType
			}
		}
		rh := http.Header{}
		for _, hh := range e.Response.Headers {
			rh.Add(hh.Name, hh.Value)
		}
		ex.Response = model.RecordedResponse{Status: e.Response.Status, Headers: SanitizeHeaders(rh, false), Body: e.Response.Content.Text}
		if e.Response.Content.Encoding == "base64" {
			ex.Response.BodyEncoding = "base64"
		}
		out = append(out, ex)
	}
	return out, nil
}

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Transfer-Encoding": true, "Upgrade": true,
	"Te": true, "Trailer": true, "Host": true, "Content-Length": true, "Accept-Encoding": true,
}

// SensitiveHeaders are dropped from recordings unless the user keeps them.
var SensitiveHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "Cookie": true, "Set-Cookie": true,
	"X-Api-Key": true, "Api-Key": true, "X-Auth-Token": true, "X-Csrf-Token": true, "X-Xsrf-Token": true,
}

// SanitizeHeaders flattens headers, dropping hop-by-hop headers and, unless
// keepSecrets is set, credentials.
func SanitizeHeaders(h http.Header, keepSecrets bool) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		ck := http.CanonicalHeaderKey(k)
		if hopHeaders[ck] || (!keepSecrets && SensitiveHeaders[ck]) || len(v) == 0 {
			continue
		}
		out[ck] = strings.Join(v, ", ")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexIDRe = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	intRe   = regexp.MustCompile(`^-?\d+$`)
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{20,}$`)
	digitRe = regexp.MustCompile(`\d`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Templatize turns a concrete path into a template: /users/42/orders/9f1c...
// becomes /users/{userId}/orders/{orderId}. It returns the template and the
// values that were replaced.
func Templatize(path string) (string, map[string]string) {
	segs := strings.Split(path, "/")
	values := map[string]string{}
	for i, s := range segs {
		dec, err := url.PathUnescape(s)
		if err != nil {
			dec = s
		}
		if !isIDSegment(dec) {
			continue
		}
		name := "id"
		if i > 0 && segs[i-1] != "" && !strings.HasPrefix(segs[i-1], "{") {
			name = singular(segs[i-1]) + "Id"
		}
		for n := 2; values[name] != ""; n++ {
			name = fmt.Sprintf("%s%d", strings.TrimRight(name, "0123456789"), n)
		}
		values[name] = dec
		segs[i] = "{" + name + "}"
	}
	return strings.Join(segs, "/"), values
}

func isIDSegment(s string) bool {
	return intRe.MatchString(s) || uuidRe.MatchString(s) || hexIDRe.MatchString(s) ||
		(tokenRe.MatchString(s) && digitRe.MatchString(s))
}

func singular(s string) string {
	s = strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(s))
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return s[:len(s)-1]
	}
	return s
}

// EndpointsFromTraffic infers endpoints from recorded exchanges: path
// templates, query parameters and JSON body shapes. It also returns the
// endpoint key of each exchange.
func EndpointsFromTraffic(exs []Exchange) (*model.API, []string) {
	type group struct {
		ep         *model.Endpoint
		pathValues map[string][]string
		queries    []url.Values
		bodies     []any
		forms      []url.Values
		count      int
	}
	groups := map[string]*group{}
	var order []string
	keys := make([]string, len(exs))
	for i, ex := range exs {
		tmpl, values := Templatize(ex.Request.Path)
		method := strings.ToUpper(ex.Request.Method)
		key := method + " " + tmpl
		keys[i] = key
		g, ok := groups[key]
		if !ok {
			g = &group{ep: &model.Endpoint{Method: method, Path: tmpl, Source: "traffic", Inferred: true}, pathValues: map[string][]string{}}
			groups[key] = g
			order = append(order, key)
		}
		g.count++
		for k, v := range values {
			g.pathValues[k] = append(g.pathValues[k], v)
		}
		q, _ := url.ParseQuery(ex.Request.Query)
		g.queries = append(g.queries, q)
		ct := strings.ToLower(ex.Request.Headers["Content-Type"])
		body := ex.Request.BodyBytes()
		switch {
		case len(body) == 0:
		case strings.Contains(ct, "json") || (ct == "" && json.Valid(body)):
			var v any
			if json.Unmarshal(body, &v) == nil {
				g.bodies = append(g.bodies, v)
				g.ep.BodyType = "application/json"
			}
		case strings.Contains(ct, "x-www-form-urlencoded"):
			if f, err := url.ParseQuery(string(body)); err == nil {
				g.forms = append(g.forms, f)
				g.ep.BodyType = "application/x-www-form-urlencoded"
			}
		}
		if len(g.ep.Samples) < 3 {
			g.ep.Samples = append(g.ep.Samples, model.Sample{PathValues: values, Query: ex.Request.Query, Body: string(body)})
		}
	}

	api := &model.API{Title: "recorded traffic", Sources: []string{fmt.Sprintf("%d recorded requests", len(exs))}}
	for _, key := range order {
		g := groups[key]
		ep := g.ep
		for _, name := range PathParamNames(ep.Path) {
			ep.Params = append(ep.Params, model.Param{Name: name, In: "path", Required: true, Schema: inferScalar(g.pathValues[name])})
		}
		seen := map[string][]string{}
		present := map[string]int{}
		for _, q := range g.queries {
			for name, vals := range q {
				seen[name] = append(seen[name], vals...)
				present[name]++
			}
		}
		names := make([]string, 0, len(seen))
		for n := range seen {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			ep.Params = append(ep.Params, model.Param{Name: name, In: "query", Required: present[name] == len(g.queries), Schema: inferScalar(seen[name])})
		}
		for _, b := range g.bodies {
			ep.Body = mergeInferred(ep.Body, InferSchema(b))
		}
		if len(g.forms) > 0 {
			ep.Body = inferForm(g.forms)
		}
		api.Endpoints = append(api.Endpoints, ep)
	}
	return api, keys
}

// InferSchema guesses a schema from one JSON value.
func InferSchema(v any) *model.Schema {
	switch t := v.(type) {
	case nil:
		return &model.Schema{Nullable: true}
	case bool:
		return &model.Schema{Type: "boolean"}
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return &model.Schema{Type: "integer"}
		}
		return &model.Schema{Type: "number"}
	case string:
		return &model.Schema{Type: "string", Format: detectFormat(t)}
	case []any:
		s := &model.Schema{Type: "array"}
		for _, item := range t {
			s.Items = mergeInferred(s.Items, InferSchema(item))
		}
		return s
	case map[string]any:
		s := &model.Schema{Type: "object", Properties: map[string]*model.Schema{}}
		for k, val := range t {
			s.Properties[k] = InferSchema(val)
			s.Required = append(s.Required, k)
		}
		sort.Strings(s.Required)
		return s
	}
	return &model.Schema{}
}

func mergeInferred(a, b *model.Schema) *model.Schema {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if a.Type == "" {
		out := *b
		out.Nullable = true
		return &out
	}
	out := *a
	out.Nullable = a.Nullable || b.Nullable
	if b.Type == "" {
		return &out
	}
	if a.Type != b.Type {
		if (a.Type == "integer" && b.Type == "number") || (a.Type == "number" && b.Type == "integer") {
			out.Type = "number"
			return &out
		}
		return &model.Schema{Nullable: out.Nullable}
	}
	if a.Format != b.Format {
		out.Format = ""
	}
	switch a.Type {
	case "object":
		props := map[string]*model.Schema{}
		for k, v := range a.Properties {
			props[k] = v
		}
		for k, v := range b.Properties {
			props[k] = mergeInferred(props[k], v)
		}
		out.Properties = props
		var req []string
		for _, r := range a.Required {
			if b.IsRequired(r) {
				req = append(req, r)
			}
		}
		out.Required = req
	case "array":
		out.Items = mergeInferred(a.Items, b.Items)
	}
	return &out
}

func inferScalar(values []string) *model.Schema {
	if len(values) == 0 {
		return &model.Schema{Type: "string"}
	}
	ints, nums, bools := true, true, true
	format := detectFormat(values[0])
	for _, v := range values {
		if !intRe.MatchString(v) {
			ints = false
		}
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			nums = false
		}
		if v != "true" && v != "false" {
			bools = false
		}
		if detectFormat(v) != format {
			format = ""
		}
	}
	switch {
	case ints:
		return &model.Schema{Type: "integer"}
	case nums:
		return &model.Schema{Type: "number"}
	case bools:
		return &model.Schema{Type: "boolean"}
	}
	return &model.Schema{Type: "string", Format: format}
}

func inferForm(forms []url.Values) *model.Schema {
	s := &model.Schema{Type: "object", Properties: map[string]*model.Schema{}}
	count := map[string]int{}
	values := map[string][]string{}
	for _, f := range forms {
		for k, v := range f {
			count[k]++
			values[k] = append(values[k], v...)
		}
	}
	for k, vals := range values {
		s.Properties[k] = &model.Schema{Type: "string", Format: inferScalar(vals).Format}
		if count[k] == len(forms) {
			s.Required = append(s.Required, k)
		}
	}
	sort.Strings(s.Required)
	return s
}

func detectFormat(s string) string {
	switch {
	case uuidRe.MatchString(s):
		return "uuid"
	case emailRe.MatchString(s):
		return "email"
	case dateRe.MatchString(s):
		return "date"
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return "date-time"
	}
	return ""
}
