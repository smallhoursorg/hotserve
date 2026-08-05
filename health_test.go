package hotswap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testHealthConfig() healthConfig {
	return healthConfig{
		path:     "/health",
		interval: time.Second,
		timeout:  time.Second,
		soak:     3 * time.Second,
		deadline: 30 * time.Second,
	}
}

func alwaysAlive() bool { return true }

func TestProberPassesAfterSoak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	clk := newFakeClock()
	p := &httpProber{client: srv.Client(), clock: clk}
	start := clk.Now()
	if err := p.waitHealthy(context.Background(), srv.URL, alwaysAlive, testHealthConfig()); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	if elapsed := clk.Now().Sub(start); elapsed < 3*time.Second {
		t.Fatalf("returned before the soak elapsed: %v", elapsed)
	}
}

func TestProberFlappingResetsSoak(t *testing.T) {
	// Healthy, healthy, unhealthy, then healthy forever: the flap must
	// restart the soak window.
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	clk := newFakeClock()
	p := &httpProber{client: srv.Client(), clock: clk}
	start := clk.Now()
	if err := p.waitHealthy(context.Background(), srv.URL, alwaysAlive, testHealthConfig()); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	// Flap at probe 3 (t=2s) means soak restarts at t=3s and completes
	// no earlier than t=6s.
	if elapsed := clk.Now().Sub(start); elapsed < 6*time.Second {
		t.Fatalf("soak was not reset by the flap: %v", elapsed)
	}
}

func TestProberDeadlineExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	clk := newFakeClock()
	p := &httpProber{client: srv.Client(), clock: clk}
	hc := testHealthConfig()
	hc.deadline = 5 * time.Second
	err := p.waitHealthy(context.Background(), srv.URL, alwaysAlive, hc)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want deadline error, got %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("deadline error should carry the last probe failure, got %v", err)
	}
}

func TestProberProcessDeath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	clk := newFakeClock()
	p := &httpProber{client: srv.Client(), clock: clk}
	var calls atomic.Int64
	dieAfterTwo := func() bool { return calls.Add(1) <= 2 }
	err := p.waitHealthy(context.Background(), srv.URL, dieAfterTwo, testHealthConfig())
	if err == nil || !strings.Contains(err.Error(), "process exited") {
		t.Fatalf("want process-exit error, got %v", err)
	}
}

func TestProberHealthOffSoaksOnLiveness(t *testing.T) {
	clk := newFakeClock()
	p := &httpProber{client: &http.Client{}, clock: clk}
	hc := testHealthConfig()
	hc.path = "" // health_path off
	start := clk.Now()
	if err := p.waitHealthy(context.Background(), "http://127.0.0.1:1", alwaysAlive, hc); err != nil {
		t.Fatalf("liveness-only soak failed: %v", err)
	}
	if elapsed := clk.Now().Sub(start); elapsed < 3*time.Second {
		t.Fatalf("soak not observed: %v", elapsed)
	}
}

func TestProberContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clk := newFakeClock()
	p := &httpProber{client: &http.Client{}, clock: clk}
	err := p.waitHealthy(ctx, "http://127.0.0.1:1", alwaysAlive, testHealthConfig())
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("want cancellation error, got %v", err)
	}
}
