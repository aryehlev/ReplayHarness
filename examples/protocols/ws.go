package protocols

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// chat is a WebSocket chat at /ws/chat. Clients send
// {"type":"join","room":"general","name":"ada"}, then
// {"type":"say","text":"hi"}. The server answers join with
// {"type":"joined","room":...,"members":N} and echoes say to the room as
// {"type":"message","from":...,"text":...}.
type chat struct {
	mu    sync.Mutex
	rooms map[string]map[*websocket.Conn]string
}

func newChat() *chat { return &chat{rooms: map[string]map[*websocket.Conn]string{}} }

func (c *chat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	room, name := "", ""
	// BUG: the member is never removed from the room when the connection
	// ends, so "members" keeps counting clients that left.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg map[string]any
		// BUG: the decode error is ignored; a message that is not a JSON
		// object leaves msg nil and the type assertion below panics, which
		// drops the connection without a close frame.
		_ = json.Unmarshal(data, &msg)
		switch msg["type"].(string) {
		case "join":
			room, _ = msg["room"].(string)
			name, _ = msg["name"].(string)
			if len(room) > 100 {
				// BUG: an internal error close (1011) for a client mistake;
				// this should be an error message or a 1008 close.
				conn.Close(websocket.StatusInternalError, "room name too long")
				return
			}
			c.mu.Lock()
			if c.rooms[room] == nil {
				c.rooms[room] = map[*websocket.Conn]string{}
			}
			c.rooms[room][conn] = name
			members := len(c.rooms[room])
			c.mu.Unlock()
			send(ctx, conn, map[string]any{"type": "joined", "room": room, "members": members})
		case "say":
			text, _ := msg["text"].(string)
			if room == "" {
				send(ctx, conn, map[string]any{"type": "error", "error": "join a room first"})
				continue
			}
			c.mu.Lock()
			peers := make([]*websocket.Conn, 0, len(c.rooms[room]))
			for p := range c.rooms[room] {
				peers = append(peers, p)
			}
			c.mu.Unlock()
			for _, p := range peers {
				send(ctx, p, map[string]any{"type": "message", "from": name, "text": text})
			}
		default:
			send(ctx, conn, map[string]any{"type": "error", "error": "unknown message type"})
		}
	}
}

func send(ctx context.Context, conn *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = conn.Write(ctx, websocket.MessageText, b)
}
