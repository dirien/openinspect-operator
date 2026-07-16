// cfmock serves the mock Cloudflare API for e2e tests in kind.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/dirien/openinspect-operator/internal/cfmock"
)

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8787"
	}
	log.Printf("cfmock listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, cfmock.New().Handler()))
}
