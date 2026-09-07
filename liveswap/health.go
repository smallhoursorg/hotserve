package liveswap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// healthConfig is the deploy gate: the new instance must answer 2xx on
// path continuously for soak before traffic cuts over, and must get
// there within deadline. An empty path means the app has no HTTP
// health endpoint (health_path off): the gate is then simply "the
// process is still alive after soak".
type healthConfig struct {
	path     string
	interval time.Duration
	timeout  time.Duration
	soak     time.Duration
	deadline time.Duration
}

// prober is the health seam; the deploy pipeline's unit tests use a
// fake, httpProber is the real thing. waitHealthy is the
// converge-then-return deploy gate; probeOnce is the steady-state
// single shot the watchdog paces itself.
type prober interface {
	waitHealthy(ctx context.Context, sock *socketRef, alive func() bool, hc healthConfig) error
	probeOnce(ctx context.Context, sock *socketRef, path string, timeout time.Duration) error
}

// httpProber polls the new instance over its unix socket until it has
// been continuously healthy for the soak period. All pacing goes
// through the injected clock so tests advance time instantly.
type httpProber struct {
	clock clock
}

func (p *httpProber) waitHealthy(ctx context.Context, sock *socketRef, alive func() bool, hc healthConfig) error {
	start := p.clock.Now()
	deadline := start.Add(hc.deadline)
	var healthySince time.Time
	lastErr := errors.New("no probe completed")

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("deploy canceled while waiting for health: %w", err)
		}
		if !alive() {
			return fmt.Errorf("process exited before becoming healthy")
		}
		now := p.clock.Now()
		if now.After(deadline) {
			return fmt.Errorf("not healthy within deadline %v: %w", hc.deadline, lastErr)
		}

		if hc.path == "" {
			// No HTTP check: soak on process liveness alone.
			if healthySince.IsZero() {
				healthySince = now
			}
			lastErr = nil
		} else if err := p.probeOnce(ctx, sock, hc.path, hc.timeout); err != nil {
			healthySince = time.Time{} // health must be continuous
			lastErr = err
		} else if healthySince.IsZero() {
			healthySince = now
			lastErr = nil
		}

		if !healthySince.IsZero() && now.Sub(healthySince) >= hc.soak {
			return nil
		}
		p.clock.Sleep(hc.interval)
	}
}

// probeOnce issues one GET over the instance's socket and demands a
// 2xx. The per-probe timeout is a real wall-clock context — an
// unresponsive app must not wedge the prober loop.
//
// A client per probe, with keep-alives off: every probe is a fresh
// connect (cheap on a unix socket), and no idle connection to an
// instance that has since died can be handed to a later probe of a
// different one. The probe never follows redirects: it is a control
// signal from supervisor to app, and an app answering 3xx must read as
// "not 2xx", not steer the supervisor's request elsewhere.
func (p *httpProber) probeOnce(ctx context.Context, sock *socketRef, path string, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{DialContext: dialUnix(sock), DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// dialUnix ignores the host; the app sees it as the Host header,
	// so it is the one value host-allowlisting apps already accept.
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// Read a bounded slice of the body before closing: a huge body is
	// discarded, never buffered.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

var _ prober = (*httpProber)(nil)
