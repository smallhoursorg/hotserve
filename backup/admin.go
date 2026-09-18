package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
	// The admin API serves the config as loaded, not as provisioned,
	// so both of liveswap's own steps have to be repeated here: its
	// default when `root` is unset, and the {env.*} it resolves at
	// load (liveswap.go, `a.Root = repl.ReplaceKnown(...)`).
	root := cfg.Root
	if root == "" {
		root = DefaultLiveswapRoot
	}
	root, err = resolveEnvPlaceholders(root)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(root, "/") {
		return nil, fmt.Errorf("liveswap's root reads as %q, which is not an absolute path — backups cannot tell where any app's data is", root)
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

// resolveEnvPlaceholders expands the `{env.NAME}` form Caddy resolves
// at config load, because the admin API hands back what was written
// rather than what liveswap made of it.
//
// A name this process cannot see is an error, not an empty string:
// hotserve is started by its own unit and may have variables this one
// does not, and silently resolving to "" would send every backup at a
// path that does not exist — which now reads as "never deployed" and
// skips the app entirely. Better to say which variable is missing.
func resolveEnvPlaceholders(s string) (string, error) {
	for {
		start := strings.Index(s, "{env.")
		if start < 0 {
			return s, nil
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			return "", fmt.Errorf("liveswap's root has an unterminated placeholder: %q", s)
		}
		end += start
		name := s[start+len("{env.") : end]
		value, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("liveswap's root is %q and %s is not set for this command: hotserve resolves it from its own environment, so set it for hotserve-backup.service too (a drop-in with Environment=%s=…), or write the path out in the Caddyfile", s, name, name)
		}
		s = s[:start] + value + s[end+1:]
	}
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
	defer resp.Body.Close() //nolint:errcheck // a response body this function has finished reading
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
