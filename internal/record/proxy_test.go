package record

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/discover"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRecordProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=abc")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"echo":` + string(body) + `}`))
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string)
	done := make(chan error)
	go func() {
		done <- Serve(ctx, Options{Upstream: u, Listen: "127.0.0.1:0", Out: &out, Ready: func(a string) { ready <- a }})
	}()
	addr := <-ready

	req, _ := http.NewRequest("POST", "http://"+addr+"/items/7?full=1", strings.NewReader(`{"n":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer top-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 || string(got) != `{"echo":{"n":1}}` {
		t.Fatalf("proxy changed the response: %d %s", resp.StatusCode, got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	rec := out.String()
	if strings.Contains(rec, "top-secret") || strings.Contains(rec, "session=abc") {
		t.Fatalf("credentials were recorded: %s", rec)
	}
	path := t.TempDir() + "/traffic.jsonl"
	if err := writeFile(path, rec); err != nil {
		t.Fatal(err)
	}
	exs, err := discover.LoadTraffic(path)
	if err != nil || len(exs) != 1 {
		t.Fatalf("load: %v, %d exchanges", err, len(exs))
	}
	ex := exs[0]
	if ex.Request.Method != "POST" || ex.Request.Path != "/items/7" || ex.Request.Query != "full=1" || ex.Request.Body != `{"n":1}` {
		t.Errorf("request = %+v", ex.Request)
	}
	if ex.Response.Status != 201 || ex.Response.Body != `{"echo":{"n":1}}` {
		t.Errorf("response = %+v", ex.Response)
	}
}
