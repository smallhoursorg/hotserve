package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// liveswapConfig is the slice of liveswap's config this package
// needs. Decoding a subset is deliberate: the backup path must not
// break when liveswap gains an unrelated field, and it must not
// import liveswap (the dependency runs feature → core, never back).
type liveswapConfig struct {
	Root string `json:"root,omitempty"`
	Apps map[string]struct {
		State []StateEntry `json:"state,omitempty"`
	} `json:"apps,omitempty"`
}

// FetchApps returns the apps that declare state, newest config as the
// running process has it — so an app added by a reload is picked up
// without anything being enabled per app, and an app removed from the
// config stops being backed up.
func FetchApps(ctx context.Context, adminAddr string) ([]App, error) {
	body, err := adminGet(ctx, adminAddr, "/config/apps/liveswap")
	if err != nil {
		return nil, err
	}
	var cfg liveswapConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("admin API returned a config this version cannot read: %w", err)
	}
	root := cfg.Root
	if root == "" {
		// The admin API serves the config as loaded, not as
		// provisioned: a Caddyfile that never wrote `root` leaves it
		// empty here even though liveswap is using its default.
		root = DefaultLiveswapRoot
	}
	names := make([]string, 0, len(cfg.Apps))
	for name := range cfg.Apps {
		names = append(names, name)
	}
	sort.Strings(names) // a stable order: the journal reads like a list, and tests can pin it
	apps := make([]App, 0, len(names))
	for _, name := range names {
		a := cfg.Apps[name]
		if len(a.State) == 0 {
			continue
		}
		apps = append(apps, App{
			Name:   name,
			Shared: strings.TrimSuffix(root, "/") + "/" + name + "/shared",
			State:  a.State,
		})
	}
	return apps, nil
}

// adminGet talks to the admin API over whatever address it is on —
// the packaged one is a unix socket, so this cannot be a plain
// http.Get. Caddy's own address parser is used so the flag accepts
// exactly what the Caddyfile's `admin` directive does.
func adminGet(ctx context.Context, adminAddr, path string) ([]byte, error) {
	addr, err := caddy.ParseNetworkAddress(adminAddr)
	if err != nil {
		return nil, fmt.Errorf("admin address %q: %w", adminAddr, err)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, addr.Network, addr.JoinHostPort(0))
			},
		},
	}
	// The Host is a placeholder: the dialer above decides where the
	// request goes, and the admin API does not route on it.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://hotserve"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admin API at %s: %w (is hotserve running, and does this user have access to the socket?)", adminAddr, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("admin API at %s: reading response: %w", adminAddr, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no liveswap app is configured (admin API %s returned 404): nothing declares state to back up", path)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin API %s returned %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}
