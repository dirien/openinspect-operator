// cfmock serves the mock Cloudflare API for e2e tests in kind.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dirien/openinspect-operator/internal/cfmock"
)

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8787"
	}
	log.Printf("cfmock listening on %s", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           cfmock.New().Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
