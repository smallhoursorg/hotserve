// The e2e demo app: what an indie hacker's service looks like to the
// orchestrator. Reports its version (from version.txt in the release)
// and pid; /health goes permanently 500 if a "broken" marker file
// shipped in the artifact — the broken-deploy scenario.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

func main() {
	version := "unknown"
	if b, err := os.ReadFile("version.txt"); err == nil {
		version = strings.TrimSpace(string(b))
	}
	broken := false
	if _, err := os.Stat("broken"); err == nil {
		broken = true
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if broken {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "hello %s pid %d", version, os.Getpid())
	})
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
