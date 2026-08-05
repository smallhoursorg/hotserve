package hotswap

import (
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestCaddyfileUnmarshalFullConfig(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hotswap {
		root /srv/hotswap
		webhook_secret {env.HOTSWAP_SECRET}
		allow_insecure_http
		allowed_artifact_hosts github.com objects.githubusercontent.com

		app blog {
			command node server.js
			pre_start node migrate.js
			env NODE_ENV production
			env DB sqlite:{shared_dir}/blog.db
			env_file /etc/hotswap/blog.env
			webhook_secret blog-secret
			health_path /healthz
			health_interval 3s
			health_timeout 1s
			soak 10s
			deadline 2m
			drain 2s
			grace 20s
			keep 3
			max_artifact_size 50MB
		}

		app api {
			command ./server --config config.yaml
		}
	}`)

	var a App
	if err := a.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Root != "/srv/hotswap" || a.WebhookSecret != "{env.HOTSWAP_SECRET}" || !a.AllowInsecureHTTP {
		t.Fatalf("globals wrong: %+v", a)
	}
	if len(a.AllowedArtifactHosts) != 2 {
		t.Fatalf("allowed hosts: %v", a.AllowedArtifactHosts)
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
	if blog.EnvFile != "/etc/hotswap/blog.env" || blog.WebhookSecret != "blog-secret" {
		t.Fatalf("env_file/secret wrong: %+v", blog)
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
	if blog.Keep != 3 || blog.MaxArtifactSize != 50_000_000 {
		t.Fatalf("keep/max wrong: %+v", blog)
	}

	api := a.Apps["api"]
	if api == nil || strings.Join(api.Command, " ") != "./server --config config.yaml" {
		t.Fatalf("api app wrong: %+v", api)
	}
}

// The parser must leave every unset field at its zero value: defaults
// belong to Provision, never to parsing.
func TestCaddyfileUnmarshalEmptyAppBlockLeavesDefaultsToProvision(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hotswap {
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
		blog.Keep != 0 || blog.MaxArtifactSize != 0 || blog.WebhookSecret != "" {
		t.Fatalf("parser applied defaults it must not: %+v", blog)
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
		{"unknown global", "hotswap {\n\tbogus\n}", "unknown subdirective"},
		{"unknown app key", "hotswap {\n\tapp a {\n\t\tbogus x\n\t}\n}", "unknown app subdirective"},
		{"duplicate app", "hotswap {\n\tapp a {\n\t\tcommand x\n\t}\n\tapp a {\n\t\tcommand y\n\t}\n}", "duplicate app"},
		{"duplicate env", "hotswap {\n\tapp a {\n\t\tenv K 1\n\t\tenv K 2\n\t}\n}", "duplicate env"},
		{"bad duration", "hotswap {\n\tapp a {\n\t\tsoak banana\n\t}\n}", "invalid soak"},
		{"bad keep", "hotswap {\n\tapp a {\n\t\tkeep many\n\t}\n}", "invalid keep"},
		{"bad size", "hotswap {\n\tapp a {\n\t\tmax_artifact_size huge\n\t}\n}", "invalid max_artifact_size"},
		{"command missing args", "hotswap {\n\tapp a {\n\t\tcommand\n\t}\n}", "wrong argument count"},
		{"env missing value", "hotswap {\n\tapp a {\n\t\tenv K\n\t}\n}", "wrong argument count"},
		{"root missing arg", "hotswap {\n\troot\n}", "wrong argument count"},
		{"positional arg", "hotswap positional", "wrong argument count"},
		{"root extra arg", "hotswap {\n\troot /a /b\n}", "wrong argument count"},
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

func TestCaddyfileWebhookDirective(t *testing.T) {
	var h Handler
	if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser("hotswap_webhook")); err != nil {
		t.Fatalf("bare directive: %v", err)
	}
	if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser("hotswap_webhook extra")); err == nil {
		t.Fatal("expected error for stray argument")
	}
}

func TestCaddyfileDynamicUpstreams(t *testing.T) {
	var u Upstreams
	if err := u.UnmarshalCaddyfile(caddyfile.NewTestDispenser("hotswap blog")); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.App != "blog" {
		t.Fatalf("app = %q", u.App)
	}
	if err := new(Upstreams).UnmarshalCaddyfile(caddyfile.NewTestDispenser("hotswap")); err == nil {
		t.Fatal("expected error for missing app name")
	}
	if err := new(Upstreams).UnmarshalCaddyfile(caddyfile.NewTestDispenser("hotswap a b")); err == nil {
		t.Fatal("expected error for extra argument")
	}
}
