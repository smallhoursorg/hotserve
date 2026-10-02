// The e2e artifact host: serves /srv over plain HTTP on :8080, the
// stand-in for a GitHub or GitLab release asset URL. Standard library
// only, so the artifacts image needs no third-party server image (and
// no version of one to keep in step with anything).
package main

import (
	"log"
	"net/http"
	"time"
)

func main() {
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           http.FileServer(http.Dir("/srv")),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
