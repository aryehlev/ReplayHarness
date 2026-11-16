// Command buggy-api serves the demo API from examples/buggyapi.
package main

import (
	"flag"
	"io"
	"log"
	"net/http"

	"github.com/aryehlev/ReplayHarness/examples/buggyapi"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	quiet := flag.Bool("q", false, "do not log handler panics")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: buggyapi.Handler()}
	if *quiet {
		srv.ErrorLog = log.New(io.Discard, "", 0)
	}
	log.Printf("buggy API listening on http://%s (spec: examples/buggyapi/openapi.yaml)", *addr)
	log.Fatal(srv.ListenAndServe())
}
