// Package record runs a reverse proxy that writes every request and response
// passing through it to a JSONL file, for `replayharness replay`.
package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/aryehlev/ReplayHarness/internal/discover"
	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Options configures the recording proxy.
type Options struct {
	Upstream    *url.URL
	Listen      string
	Out         io.Writer
	KeepSecrets bool  // keep Authorization, Cookie and similar headers in the recording
	MaxBody     int64 // bodies larger than this are recorded truncated
	Log         func(format string, args ...any)
	Ready       func(addr string)
}

// Serve runs the proxy until ctx is cancelled.
func Serve(ctx context.Context, opts Options) error {
	if opts.MaxBody <= 0 {
		opts.MaxBody = 1 << 20
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(opts.Upstream)
			pr.SetXForwarded()
			// Ask for an uncompressed body so the recording is readable; the
			// transport still negotiates gzip with the upstream on its own.
			pr.Out.Header.Del("Accept-Encoding")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			opts.Log("upstream error for %s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	var mu sync.Mutex
	enc := json.NewEncoder(opts.Out)
	enc.SetEscapeHTML(false)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "could not read request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
		r.ContentLength = int64(len(reqBody))

		capture := &capture{ResponseWriter: w, status: http.StatusOK, limit: opts.MaxBody}
		proxy.ServeHTTP(capture, r)

		ex := discover.Exchange{Time: start.UTC(), DurationMS: float64(time.Since(start).Microseconds()) / 1000}
		ex.Request = model.Request{
			Method:  r.Method,
			Path:    r.URL.EscapedPath(),
			Query:   r.URL.RawQuery,
			Headers: discover.SanitizeHeaders(r.Header, opts.KeepSecrets),
		}
		recorded := reqBody
		if int64(len(recorded)) > opts.MaxBody {
			recorded = recorded[:opts.MaxBody]
		}
		ex.Request.Body, ex.Request.BodyEncoding = model.EncodeBody(recorded)
		ex.Response = model.RecordedResponse{Status: capture.status, Headers: discover.SanitizeHeaders(w.Header(), opts.KeepSecrets)}
		ex.Response.Body, ex.Response.BodyEncoding = model.EncodeBody(capture.buf.Bytes())

		mu.Lock()
		err = enc.Encode(ex)
		mu.Unlock()
		if err != nil {
			opts.Log("could not write the recording: %v", err)
		}
		opts.Log("%-6s %s → %d (%.0fms)", r.Method, r.URL.RequestURI(), capture.status, ex.DurationMS)
	})

	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}
	if opts.Ready != nil {
		opts.Ready(ln.Addr().String())
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type capture struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         bytes.Buffer
	limit       int64
}

func (c *capture) WriteHeader(code int) {
	if !c.wroteHeader {
		c.status, c.wroteHeader = code, true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *capture) Write(b []byte) (int, error) {
	c.wroteHeader = true
	if room := c.limit - int64(c.buf.Len()); room > 0 {
		if int64(len(b)) > room {
			c.buf.Write(b[:room])
		} else {
			c.buf.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

func (c *capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *capture) Unwrap() http.ResponseWriter { return c.ResponseWriter }
