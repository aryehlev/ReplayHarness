// Package wsx opens a WebSocket, sends messages and collects the replies.
package wsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// DefaultWait is how long to collect messages when the request does not say
// how many to expect.
const DefaultWait = time.Second

// URL turns the HTTP base URL and a request path into a ws:// or wss:// URL.
func URL(base *url.URL, req model.Request) string {
	u := *base
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	s := strings.TrimRight(u.String(), "/") + req.Path
	if req.Query != "" {
		s += "?" + req.Query
	}
	return s
}

// Do opens the connection, sends each message in the request body (a JSON
// array; strings go as they are, anything else as JSON text), then reads
// messages until it has req.Receive of them, the server closes, or the wait
// is over. The body of the result is a JSON array of what arrived.
func Do(ctx context.Context, client *http.Client, base *url.URL, req model.Request, headers http.Header, timeout time.Duration, maxBody int64) model.Observed {
	var send []any
	if b := req.BodyBytes(); len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &send); err != nil {
			return model.Observed{Unsent: "WebSocket messages must be a JSON array: " + err.Error()}
		}
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	start := time.Now()
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dctx, URL(base, req), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	obs := model.Observed{Headers: map[string]string{}}
	if resp != nil {
		obs.Status = resp.StatusCode
		for k, v := range resp.Header {
			obs.Headers[k] = strings.Join(v, ", ")
		}
	}
	if err != nil {
		obs.DurationMS = ms(time.Since(start))
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			if resp.Body != nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				obs.Body = string(b)
			}
			return obs
		}
		obs.Status = 0
		obs.Error = err.Error()
		obs.Timeout = errors.Is(err, context.DeadlineExceeded)
		return obs
	}
	defer conn.CloseNow()
	if maxBody > 0 {
		conn.SetReadLimit(maxBody)
	}
	var writeErr error
	for i, m := range send {
		text, ok := m.(string)
		if !ok {
			b, _ := json.Marshal(m)
			text = string(b)
		}
		if err := conn.Write(dctx, websocket.MessageText, []byte(text)); err != nil {
			writeErr = fmt.Errorf("sending message %d: %w", i+1, err)
			break
		}
	}

	wait := time.Duration(req.WaitMS) * time.Millisecond
	if wait <= 0 {
		wait = DefaultWait
		if req.Receive > 0 {
			wait = timeout
		}
	}
	rctx, rcancel := context.WithTimeout(ctx, wait)
	defer rcancel()
	received := []any{}
read:
	for req.Receive <= 0 || len(received) < req.Receive {
		typ, data, err := conn.Read(rctx)
		if err != nil {
			var ce websocket.CloseError
			switch {
			case errors.As(err, &ce):
				obs.Close = int(ce.Code)
				if ce.Reason != "" {
					obs.Headers["Close-Reason"] = ce.Reason
				}
			case rctx.Err() != nil:
				// The wait is over.
			case writeErr != nil:
				// The server closed while a message was being sent: a close
				// code, if any, came with the write error.
				var wce websocket.CloseError
				if errors.As(writeErr, &wce) {
					obs.Close = int(wce.Code)
				} else {
					obs.Close = int(websocket.StatusAbnormalClosure)
					obs.Error = writeErr.Error()
				}
			default:
				obs.Close = int(websocket.StatusAbnormalClosure)
				obs.Error = "the server dropped the connection without a close frame"
			}
			break read
		}
		var v any
		if typ == websocket.MessageText && json.Unmarshal(data, &v) == nil {
			received = append(received, v)
		} else if typ == websocket.MessageText {
			received = append(received, string(data))
		} else {
			received = append(received, map[string]any{"binary": len(data)})
		}
	}
	obs.DurationMS = ms(time.Since(start))
	b, _ := json.Marshal(received)
	obs.Body = string(b)
	if obs.Close == 0 {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	return obs
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Effective maps a WebSocket outcome to the HTTP status that means the same,
// for the checks: 1011 (internal error) is a 500, a policy or data close
// (1003, 1007, 1008, 1009, 4000-4999) is a rejection, and a connection that
// stayed open or closed normally is a 200. A handshake that was refused keeps
// its status.
func Effective(o model.Observed) int {
	if o.Status != http.StatusSwitchingProtocols {
		return o.Status
	}
	switch c := o.Close; {
	case c == 1011 || c == 1014:
		return 500
	case c == 1008:
		return 403
	case c == 1003 || c == 1007 || c == 1009:
		return 400
	case c >= 4400 && c <= 4499:
		return c - 4000
	case c >= 4000 && c <= 4999:
		return 400
	}
	return 200
}
