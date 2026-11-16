// Package runner sends requests to the server under test and evaluates the
// responses.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aryehlev/ReplayHarness/internal/check"
	"github.com/aryehlev/ReplayHarness/internal/model"
)

// DefaultAuthHeaders are left out of requests that test missing credentials.
var DefaultAuthHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "Api-Key", "X-Auth-Token"}

// Config controls a run.
type Config struct {
	Target      string
	Baseline    string            // optional second server to compare against
	Headers     map[string]string // sent with every request (for example credentials)
	AuthHeaders []string          // extra header names that carry credentials
	Concurrency int
	Rate        float64 // requests per second; 0 means no limit
	Timeout     time.Duration
	MaxBody     int64
	AllowRemote bool
	UserAgent   string
	Check       check.Options
	Progress    func(done, total int)
}

// CheckTarget refuses targets outside loopback and private networks unless
// allowRemote is set. Generated traffic is meant for servers you run.
func CheckTarget(raw string, allowRemote bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("target %q must be an http:// or https:// URL", raw)
	}
	if allowRemote {
		return u, nil
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return u, nil
	}
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		if ips, err = net.LookupIP(host); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
	}
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
			return nil, fmt.Errorf("refusing to send requests to %s (%s is a public address). "+
				"ReplayHarness is for servers you run yourself; pass --allow-remote if you own this host", host, ip)
		}
	}
	return u, nil
}

// Client sends requests to one target with the configured headers, rate
// limit and timeout. It is safe for concurrent use.
type Client struct {
	base *url.URL
	cfg  Config
	http *http.Client
	auth map[string]bool
	tick *time.Ticker
}

// NewClient checks the target and prepares a client for it.
func NewClient(target string, cfg Config, extraAuth []string) (*Client, error) {
	base, err := CheckTarget(target, cfg.AllowRemote)
	if err != nil {
		return nil, err
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 1 << 20
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "ReplayHarness"
	}
	c := &Client{
		base: base,
		cfg:  cfg,
		auth: map[string]bool{},
		http: &http.Client{
			Timeout:       cfg.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: cfg.Concurrency * 2,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
	for _, list := range [][]string{DefaultAuthHeaders, cfg.AuthHeaders, extraAuth} {
		for _, h := range list {
			c.auth[http.CanonicalHeaderKey(h)] = true
		}
	}
	if cfg.Rate > 0 {
		c.tick = time.NewTicker(time.Duration(float64(time.Second) / cfg.Rate))
	}
	return c, nil
}

// Target returns the base URL requests go to.
func (c *Client) Target() string { return c.base.String() }

// AuthHeaders returns the header names treated as credentials.
func (c *Client) AuthHeaders() map[string]bool { return c.auth }

// Close releases the client's resources.
func (c *Client) Close() {
	if c.tick != nil {
		c.tick.Stop()
	}
	c.http.CloseIdleConnections()
}

func (c *Client) wait(ctx context.Context) bool {
	if c.tick == nil {
		return ctx.Err() == nil
	}
	select {
	case <-c.tick.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Do sends one request. stripAuth leaves out the credential headers given in
// the configuration. A request that never got a response has Status 0.
func (c *Client) Do(ctx context.Context, r model.Request, stripAuth bool) model.Observed {
	if !c.wait(ctx) {
		return model.Observed{Error: "cancelled"}
	}
	u := strings.TrimRight(c.base.String(), "/") + r.Path
	if r.Query != "" {
		u += "?" + r.Query
	}
	body := r.BodyBytes()
	var rdr io.Reader
	if len(body) > 0 || headerValue(r.Headers, "Content-Type") != "" {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, rdr)
	if err != nil {
		return model.Observed{Error: "could not build the request: " + err.Error()}
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	for k, v := range c.cfg.Headers {
		if stripAuth && c.auth[http.CanonicalHeaderKey(k)] {
			continue
		}
		req.Header.Set(k, v)
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		obs := model.Observed{DurationMS: ms(time.Since(start)), Error: err.Error()}
		var ne net.Error
		obs.Timeout = (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded)
		return obs
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBody+1))
	obs := model.Observed{Status: resp.StatusCode, DurationMS: ms(time.Since(start))}
	if readErr != nil {
		obs.Error = readErr.Error()
	}
	if int64(len(data)) > c.cfg.MaxBody {
		data, obs.Truncated = data[:c.cfg.MaxBody], true
	}
	obs.Body = string(data)
	obs.Headers = map[string]string{}
	for k, v := range resp.Header {
		obs.Headers[k] = strings.Join(v, ", ")
	}
	return obs
}

func headerValue(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// Run sends every scenario and returns the results in scenario order. When
// ctx is cancelled it stops early and returns the results it has.
func Run(ctx context.Context, cfg Config, suite *model.Suite) ([]model.Result, error) {
	var apiAuth []string
	endpoints := map[string]*model.Endpoint{}
	if suite.API != nil {
		apiAuth = suite.API.AuthHeaders
		for _, ep := range suite.API.Endpoints {
			endpoints[ep.Key()] = ep
		}
	}
	target, err := NewClient(cfg.Target, cfg, apiAuth)
	if err != nil {
		return nil, err
	}
	defer target.Close()
	var baseline *Client
	if cfg.Baseline != "" {
		if baseline, err = NewClient(cfg.Baseline, cfg, apiAuth); err != nil {
			return nil, err
		}
		defer baseline.Close()
	}
	workers := cfg.Concurrency
	if workers <= 0 {
		workers = 4
	}

	results := make([]model.Result, len(suite.Scenarios))
	jobs := make(chan int)
	var done atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				sc := suite.Scenarios[i]
				res := model.Result{Scenario: sc}
				n := sc.Repeat
				if n < 1 {
					n = 1
				}
				for k := 0; k < n && ctx.Err() == nil; k++ {
					res.Observed = append(res.Observed, target.Do(ctx, sc.Request, sc.StripAuth))
				}
				if baseline != nil && ctx.Err() == nil {
					b := baseline.Do(ctx, sc.Request, sc.StripAuth)
					res.Baseline = &b
				}
				if ctx.Err() == nil {
					res.Findings = check.Evaluate(sc, endpoints[sc.Endpoint], res.Observed, res.Baseline, cfg.Check)
				} else {
					res.Observed = nil // interrupted: drop the partial result
				}
				results[i] = res
				if cfg.Progress != nil {
					cfg.Progress(int(done.Add(1)), len(suite.Scenarios))
				}
			}
		}()
	}
feed:
	for i := range suite.Scenarios {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	out := results[:0]
	for _, res := range results {
		if len(res.Observed) > 0 {
			out = append(out, res)
		}
	}
	return out, ctx.Err()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
