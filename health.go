package hotswap

import (
	"context"
	"errors"
	"fmt"
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

// prober is the health gate seam; the deploy pipeline's unit tests use
// a fake, httpProber is the real thing.
type prober interface {
	waitHealthy(ctx context.Context, baseURL string, alive func() bool, hc healthConfig) error
}

// httpProber polls the new instance until it has been continuously
// healthy for the soak period. All pacing goes through the injected
// clock so tests advance time instantly.
type httpProber struct {
	client *http.Client
	clock  clock
}

func (p *httpProber) waitHealthy(ctx context.Context, baseURL string, alive func() bool, hc healthConfig) error {
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
			return fmt.Errorf("not healthy within deadline %v: %v", hc.deadline, lastErr)
		}

		if hc.path == "" {
			// No HTTP check: soak on process liveness alone.
			if healthySince.IsZero() {
				healthySince = now
			}
			lastErr = nil
		} else if err := p.probe(ctx, baseURL+hc.path, hc.timeout); err != nil {
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

// probe issues one GET and demands a 2xx. The per-probe timeout is a
// real wall-clock context — an unresponsive app must not wedge the
// prober loop.
func (p *httpProber) probe(ctx context.Context, url string, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

var _ prober = (*httpProber)(nil)
