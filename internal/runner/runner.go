// Package runner sends scenarios to a server and evaluates the responses.
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

// DefaultAuthHeaders are left out of scenarios that test missing credentials.
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
			return nil, fmt.Errorf("refusing to send generated traffic to %s (%s is a public address). "+
				"ReplayHarness is for servers you run yourself; pass --allow-remote if you own this host", host, ip)
		}
	}
	return u, nil
}

// Run sends every scenario and returns the results in scenario order. When
// ctx is cancelled it stops early and returns the results it has.
func Run(ctx context.Context, cfg Config, suite *model.Suite) ([]model.Result, error) {
	target, err := CheckTarget(cfg.Target, cfg.AllowRemote)
	if err != nil {
		return nil, err
	}
	var baseline *url.URL
	if cfg.Baseline != "" {
		if baseline, err = CheckTarget(cfg.Baseline, cfg.AllowRemote); err != nil {
			return nil, err
		}
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
	client := &http.Client{
		Timeout:       cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConnsPerHost: cfg.Concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	defer client.CloseIdleConnections()

	auth := map[string]bool{}
	for _, h := range DefaultAuthHeaders {
		auth[http.CanonicalHeaderKey(h)] = true
	}
	for _, h := range cfg.AuthHeaders {
		auth[http.CanonicalHeaderKey(h)] = true
	}
	endpoints := map[string]*model.Endpoint{}
	if suite.API != nil {
		for _, h := range suite.API.AuthHeaders {
			auth[http.CanonicalHeaderKey(h)] = true
		}
		for _, ep := range suite.API.Endpoints {
			endpoints[ep.Key()] = ep
		}
	}
	r := &runner{cfg: cfg, client: client, auth: auth}

	var tick <-chan time.Time
	if cfg.Rate > 0 {
		t := time.NewTicker(time.Duration(float64(time.Second) / cfg.Rate))
		defer t.Stop()
		tick = t.C
	}
	wait := func() bool {
		if tick == nil {
			return ctx.Err() == nil
		}
		select {
		case <-tick:
			return true
		case <-ctx.Done():
			return false
		}
	}

	results := make([]model.Result, len(suite.Scenarios))
	jobs := make(chan int)
	var done atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
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
				for k := 0; k < n && wait(); k++ {
					res.Observed = append(res.Observed, r.send(ctx, target, sc))
				}
				if baseline != nil && len(res.Observed) > 0 && wait() {
					b := r.send(ctx, baseline, sc)
					res.Baseline = &b
				}
				if len(res.Observed) == n && ctx.Err() == nil {
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

type runner struct {
	cfg    Config
	client *http.Client
	auth   map[string]bool
}

func (r *runner) send(ctx context.Context, base *url.URL, sc model.Scenario) model.Observed {
	u := strings.TrimRight(base.String(), "/") + sc.Request.Path
	if sc.Request.Query != "" {
		u += "?" + sc.Request.Query
	}
	body := sc.Request.BodyBytes()
	var rdr io.Reader
	if len(body) > 0 || sc.Request.Headers["Content-Type"] != "" {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, sc.Request.Method, u, rdr)
	if err != nil {
		return model.Observed{Error: "could not build the request: " + err.Error()}
	}
	req.Header.Set("User-Agent", r.cfg.UserAgent)
	for k, v := range r.cfg.Headers {
		if sc.StripAuth && r.auth[http.CanonicalHeaderKey(k)] {
			continue
		}
		req.Header.Set(k, v)
	}
	for k, v := range sc.Request.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		obs := model.Observed{DurationMS: ms(time.Since(start)), Error: err.Error()}
		var ne net.Error
		obs.Timeout = (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded)
		return obs
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, r.cfg.MaxBody+1))
	obs := model.Observed{Status: resp.StatusCode, DurationMS: ms(time.Since(start))}
	if readErr != nil {
		obs.Error = readErr.Error()
	}
	if int64(len(data)) > r.cfg.MaxBody {
		data, obs.Truncated = data[:r.cfg.MaxBody], true
	}
	obs.Body = string(data)
	obs.Headers = map[string]string{}
	for k, v := range resp.Header {
		obs.Headers[k] = strings.Join(v, ", ")
	}
	return obs
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
