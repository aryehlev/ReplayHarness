// Command protocols-demo runs the demo: HTTP (GraphQL and WebSocket) on
// 127.0.0.1:8081 and gRPC on 127.0.0.1:9091.
package main

import (
	"io"
	"log"
	"net"
	"net/http"

	"github.com/aryehlev/ReplayHarness/examples/protocols"
)

func main() {
	g, err := protocols.NewGRPC()
	if err != nil {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:9091")
	if err != nil {
		log.Fatal(err)
	}
	go func() { log.Fatal(g.Serve(lis)) }()
	log.Println("gRPC on 127.0.0.1:9091, GraphQL on http://127.0.0.1:8081/graphql, WebSocket on ws://127.0.0.1:8081/ws/chat")
	srv := &http.Server{Addr: "127.0.0.1:8081", Handler: protocols.NewHTTP(), ErrorLog: log.New(io.Discard, "", 0)}
	log.Fatal(srv.ListenAndServe())
}
