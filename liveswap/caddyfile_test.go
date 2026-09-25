package liveswap

import (
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestCaddyfileUnmarshalFullConfig(t *testing.T) {
	d := caddyfile.NewTestDispenser(`liveswap {
		root /srv/liveswap
		deploy_trust github {
			audience hotserve
			claim repository smallhoursorg/site
		}
		allow_insecure_http
		artifact_allowlist github.com/smallhoursorg/ artifacts.corp

		app blog {
			command node server.js
			pre_start node migrate.js
			env NODE_ENV production
			env DB sqlite:{shared_dir}/blog.db
			env_file /etc/liveswap/blog.env
			deploy_trust local {
				public_key /etc/hotserve/blog.pub
				subject ci@smallhours
			}
			health_path /healthz
			health_interval 3s
			health_timeout 1s
			soak 10s
			deadline 2m
			drain 2s
			grace 20s
			watchdog off
			watchdog_failures 4
			watchdog_grace 45s
			watchdog_restarts 7
			watchdog_window 15m
			keep 3
			deploy_log_lines 12
			max_artifact_size 50MB
			max_artifact_entries 20000
		}

		app api {
			command ./server --config config.yaml
		}
	}`)

	var a App
	if err := a.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Root != "/srv/liveswap" || !a.AllowInsecureHTTP {
		t.Fatalf("globals wrong: %+v", a)
	}
	if len(a.DeployTrust) != 1 || a.DeployTrust[0].Kind != "github" ||
		a.DeployTrust[0].Audience != "hotserve" ||
		a.DeployTrust[0].Claims["repository"] != "smallhoursorg/site" {
		t.Fatalf("global deploy_trust wrong: %+v", a.DeployTrust)
	}
	if len(a.ArtifactAllowlist) != 2 {
		t.Fatalf("artifact allowlist: %v", a.ArtifactAllowlist)
	}

	blog := a.Apps["blog"]
	if blog == nil {
		t.Fatal("blog app missing")
	}
	if got := strings.Join(blog.Command, " "); got != "node server.js" {
		t.Fatalf("command = %q", got)
	}
	if got := strings.Join(blog.PreStart, " "); got != "node migrate.js" {
		t.Fatalf("pre_start = %q", got)
	}
	if blog.Env["NODE_ENV"] != "production" || blog.Env["DB"] != "sqlite:{shared_dir}/blog.db" {
		t.Fatalf("env = %v", blog.Env)
	}
	if blog.EnvFile != "/etc/liveswap/blog.env" {
		t.Fatalf("env_file wrong: %+v", blog)
	}
	if len(blog.DeployTrust) != 1 || blog.DeployTrust[0].Kind != "local" ||
		blog.DeployTrust[0].PublicKey != "/etc/hotserve/blog.pub" ||
		blog.DeployTrust[0].Claims["sub"] != "ci@smallhours" {
		t.Fatalf("per-app deploy_trust wrong: %+v", blog.DeployTrust)
	}
	if blog.HealthPath != "/healthz" ||
		blog.HealthInterval != caddy.Duration(3*time.Second) ||
		blog.HealthTimeout != caddy.Duration(time.Second) ||
		blog.Soak != caddy.Duration(10*time.Second) ||
		blog.Deadline != caddy.Duration(2*time.Minute) ||
		blog.Drain != caddy.Duration(2*time.Second) ||
		blog.Grace != caddy.Duration(20*time.Second) {
		t.Fatalf("durations wrong: %+v", blog)
	}
	if blog.Keep != 3 || blog.MaxArtifactSize != 50_000_000 || blog.MaxArtifactEntries != 20_000 {
		t.Fatalf("keep/max wrong: %+v", blog)
	}
	if blog.DeployLogLines == nil || *blog.DeployLogLines != 12 {
		t.Fatalf("deploy_log_lines wrong: %+v", blog.DeployLogLines)
	}
	if blog.Watchdog != "off" ||
		blog.WatchdogFailures != 4 ||
		blog.WatchdogGrace != caddy.Duration(45*time.Second) ||
		blog.WatchdogRestarts != 7 ||
		blog.WatchdogWindow != caddy.Duration(15*time.Minute) {
		t.Fatalf("watchdog options wrong: %+v", blog)
	}

	api := a.Apps["api"]
	if api == nil || strings.Join(api.Command, " ") != "./server --config config.yaml" {
		t.Fatalf("api app wrong: %+v", api)
	}
}

// The parser must leave every unset field at its zero value: defaults
// belong to Provision, never to parsing.
func TestCaddyfileUnmarshalEmptyAppBlockLeavesDefaultsToProvision(t *testing.T) {
	d := caddyfile.NewTestDispenser(`liveswap {
		app blog {
			command node server.js
		}
	}`)
	var a App
	if err := a.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	blog := a.Apps["blog"]
	if blog.HealthPath != "" || blog.HealthInterval != 0 || blog.Soak != 0 ||
		blog.Deadline != 0 || blog.Drain != 0 || blog.Grace != 0 ||
		blog.Keep != 0 || blog.MaxArtifactSize != 0 || blog.MaxArtifactEntries != 0 || len(blog.DeployTrust) != 0 ||
		blog.DeployLogLines != nil {
		t.Fatalf("parser applied defaults it must not: %+v", blog)
	}
	if blog.Watchdog != "" || blog.WatchdogFailures != 0 || blog.WatchdogGrace != 0 ||
		blog.WatchdogRestarts != 0 || blog.WatchdogWindow != 0 {
		t.Fatalf("parser applied watchdog defaults it must not: %+v", blog)
	}
	if a.Root != "" {
		t.Fatalf("root should be zero, got %q", a.Root)
	}
}

func TestCaddyfileUnmarshalErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"unknown global", "liveswap {\n\tbogus\n}", "unknown subdirective"},
		{"unknown app key", "liveswap {\n\tapp a {\n\t\tbogus x\n\t}\n}", "unknown app subdirective"},
		{"duplicate app", "liveswap {\n\tapp a {\n\t\tcommand x\n\t}\n\tapp a {\n\t\tcommand y\n\t}\n}", "duplicate app"},
		{"duplicate env", "liveswap {\n\tapp a {\n\t\tenv K 1\n\t\tenv K 2\n\t}\n}", "duplicate env"},
		{"duplicate command", "liveswap {\n\tapp a {\n\t\tcommand x\n\t\tcommand y\n\t}\n}", "duplicate command"},
		{"duplicate env_file", "liveswap {\n\tapp a {\n\t\tenv_file /a\n\t\tenv_file /b\n\t}\n}", "duplicate env_file"},
		{"duplicate keep", "liveswap {\n\tapp a {\n\t\tkeep 3\n\t\tkeep 4\n\t}\n}", "duplicate keep"},
		{"duplicate soak", "liveswap {\n\tapp a {\n\t\tsoak 1s\n\t\tsoak 2s\n\t}\n}", "duplicate soak"},
		{"duplicate watchdog", "liveswap {\n\tapp a {\n\t\twatchdog on\n\t\twatchdog off\n\t}\n}", "duplicate watchdog"},
		{"duplicate root", "liveswap {\n\troot /a\n\troot /b\n}", "duplicate root"},
		{"duplicate audience in a trust block", "liveswap {\n\tdeploy_trust github {\n\t\taudience a\n\t\taudience b\n\t}\n}", "duplicate audience"},
		{"duplicate public_key in a trust block", "liveswap {\n\tapp a {\n\t\tdeploy_trust local {\n\t\t\tpublic_key /k1\n\t\t\tpublic_key /k2\n\t\t}\n\t}\n}", "duplicate public_key"},
		{"duplicate allow_insecure_http", "liveswap {\n\tallow_insecure_http\n\tallow_insecure_http\n}", "duplicate allow_insecure_http"},
		{"bad duration", "liveswap {\n\tapp a {\n\t\tsoak banana\n\t}\n}", "invalid soak"},
		{"bad keep", "liveswap {\n\tapp a {\n\t\tkeep many\n\t}\n}", "invalid keep"},
		{"bad size", "liveswap {\n\tapp a {\n\t\tmax_artifact_size huge\n\t}\n}", "invalid max_artifact_size"},
		{"bad entries", "liveswap {\n\tapp a {\n\t\tmax_artifact_entries lots\n\t}\n}", "invalid max_artifact_entries"},
		{"command missing args", "liveswap {\n\tapp a {\n\t\tcommand\n\t}\n}", "wrong argument count"},
		{"env missing value", "liveswap {\n\tapp a {\n\t\tenv K\n\t}\n}", "wrong argument count"},
		{"root missing arg", "liveswap {\n\troot\n}", "wrong argument count"},
		{"positional arg", "liveswap positional", "wrong argument count"},
		{"root extra arg", "liveswap {\n\troot /a /b\n}", "wrong argument count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a App
			err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCaddyfileRepeatedAdditiveSubdirectivesAccumulate(t *testing.T) {
	// The subdirectives that add an entry per line may repeat, at both
	// levels; every other one sets a single value and a repeat is
	// refused rather than silently overriding the earlier line.
	var a App
	err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`liveswap {
		artifact_allowlist github.com/a/
		artifact_allowlist github.com/b/
		deploy_trust github {
			audience one
			claim repository a/b
			claim ref refs/heads/main
		}
		deploy_trust github {
			audience two
		}
		app x {
			command x
			artifact_allowlist github.com/c/
			artifact_allowlist github.com/d/
			env A 1
			env B 2
			deploy_trust local {
				public_key /k1
			}
			deploy_trust local {
				public_key /k2
			}
		}
	}`))
	if err != nil {
		t.Fatalf("repeated additive subdirectives must accumulate: %v", err)
	}
	if got := a.ArtifactAllowlist; len(got) != 2 || len(a.DeployTrust) != 2 {
		t.Fatalf("global lines did not accumulate: allowlist %v, trust %d", got, len(a.DeployTrust))
	}
	x := a.Apps["x"]
	if len(x.ArtifactAllowlist) != 2 || len(x.Env) != 2 || len(x.DeployTrust) != 2 {
		t.Fatalf("app lines did not accumulate: allowlist %v, env %v, trust %d", x.ArtifactAllowlist, x.Env, len(x.DeployTrust))
	}
}

func TestCaddyfileWebhookDirective(t *testing.T) {
	var h Handler
	if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap_webhook")); err != nil {
		t.Fatalf("bare directive: %v", err)
	}
	if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap_webhook extra")); err == nil {
		t.Fatal("expected error for stray argument")
	}
}

func TestCaddyfileDynamicUpstreams(t *testing.T) {
	var u Upstreams
	if err := u.UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap blog")); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.App != "blog" {
		t.Fatalf("app = %q", u.App)
	}
	if err := new(Upstreams).UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap")); err == nil {
		t.Fatal("expected error for missing app name")
	}
	if err := new(Upstreams).UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap a b")); err == nil {
		t.Fatal("expected error for extra argument")
	}
}

func TestCaddyfileOldAllowlistDirectiveIsHelpful(t *testing.T) {
	d := caddyfile.NewTestDispenser(`liveswap {
		allowed_artifact_hosts github.com
	}`)
	a := new(App)
	err := a.UnmarshalCaddyfile(d)
	if err == nil || !strings.Contains(err.Error(), "artifact_allowlist") {
		t.Fatalf("want rename hint, got %v", err)
	}
}
