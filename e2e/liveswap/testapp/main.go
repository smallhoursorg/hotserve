// The e2e demo app: what an indie hacker's service looks like to the
// orchestrator. Reports its version (from version.txt in the release)
// and pid; /health goes 500 while a "broken" marker file exists in the
// release dir (shipped in the artifact for the broken-deploy scenario,
// or created live via /break for the watchdog scenario). /boom exits
// the process — the crash the watchdog must recover from.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	version := "unknown"
	if b, err := os.ReadFile("version.txt"); err == nil {
		version = strings.TrimSpace(string(b))
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		// Per request, not once at boot: the watchdog scenario breaks
		// and heals a running instance.
		if _, err := os.Stat("broken"); err == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(50 * time.Millisecond) // let the response flush
			os.Exit(1)
		}()
	})
	http.HandleFunc("/break", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.WriteFile("broken", []byte("1"), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/heal", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.Remove("broken"); err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "hello %s pid %d", version, os.Getpid())
	})
	srv := &http.Server{
		Addr:              "127.0.0.1:" + os.Getenv("PORT"),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
