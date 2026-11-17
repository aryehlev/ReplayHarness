// Package protocols is a demo service with planted bugs in three protocols:
// a gRPC orders service (grpc.go, orders.proto), a GraphQL product catalog
// (graphql.go, schema.graphql) and a WebSocket chat (ws.go). Each BUG
// comment marks one. ReplayHarness's tests start it and check that the bugs
// are found; examples/protocols/replays.yaml shows the replays an agent
// writes for it.
package protocols

import "net/http"

// NewHTTP returns the HTTP side of the demo: GraphQL at /graphql and the
// chat at /ws/chat.
func NewHTTP() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/graphql", newCatalog())
	mux.Handle("/ws/chat", newChat())
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}
