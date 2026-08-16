package liveswap

import (
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// defaultedApp mirrors what Provision produces for an app with only a
// command set, without touching the UsagePool.
func defaultedApp(t *testing.T) *AppConfig {
	t.Helper()
	cfg := &AppConfig{Command: []string{"node", "server.js"}}
	cfg.applyDefaults(caddy.NewReplacer(), "global-secret")
	return cfg
}

func TestApplyDefaults(t *testing.T) {
	cfg := defaultedApp(t)
	if cfg.HealthPath != "/health" {
		t.Errorf("health_path default = %q", cfg.HealthPath)
	}
	if cfg.HealthInterval != caddy.Duration(5*time.Second) ||
		cfg.HealthTimeout != caddy.Duration(2*time.Second) ||
		cfg.Soak != caddy.Duration(15*time.Second) ||
		cfg.Deadline != caddy.Duration(5*time.Minute) ||
		cfg.Drain != caddy.Duration(5*time.Second) ||
		cfg.Grace != caddy.Duration(10*time.Second) {
		t.Errorf("duration defaults wrong: %+v", cfg)
	}
	if cfg.Keep != 5 || cfg.MaxArtifactSize != 100_000_000 {
		t.Errorf("keep/size defaults wrong: %+v", cfg)
	}
	if cfg.WebhookSecret != "global-secret" {
		t.Errorf("secret should inherit the global default, got %q", cfg.WebhookSecret)
	}
}

func TestApplyDefaultsKeepsExplicitValues(t *testing.T) {
	cfg := &AppConfig{
		Command:       []string{"x"},
		WebhookSecret: "own",
		HealthPath:    "off",
		Soak:          caddy.Duration(time.Second),
	}
	cfg.applyDefaults(caddy.NewReplacer(), "global")
	if cfg.WebhookSecret != "own" || cfg.HealthPath != "off" || cfg.Soak != caddy.Duration(time.Second) {
		t.Fatalf("explicit values overwritten: %+v", cfg)
	}
}

func TestBuildSpecTranslatesConfig(t *testing.T) {
	a := &App{
		Root:                 "/var/lib/liveswap",
		AllowInsecureHTTP:    true,
		AllowedArtifactHosts: []string{"github.com"},
	}
	cfg := defaultedApp(t)
	cfg.HealthPath = "off"
	spec := a.buildSpec("blog", cfg)
	if spec.healthPath != "" {
		t.Errorf("health_path off must become empty, got %q", spec.healthPath)
	}
	if !spec.allowInsecure {
		t.Error("allowInsecure not carried over")
	}
	if _, ok := spec.allowedHosts["github.com"]; !ok {
		t.Error("allowed hosts not carried over")
	}
	if spec.dirs.releases != "/var/lib/liveswap/blog/releases" {
		t.Errorf("dirs wrong: %+v", spec.dirs)
	}
	if spec.soak != 15*time.Second || spec.keep != 5 {
		t.Errorf("spec values wrong: %+v", spec)
	}
}

func TestValidate(t *testing.T) {
	valid := func() *App {
		return &App{
			Root: "/var/lib/liveswap",
			Apps: map[string]*AppConfig{"blog": defaultedApp(t)},
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*App)
		want   string
	}{
		{"relative root", func(a *App) { a.Root = "var/liveswap" }, "root must be an absolute path"},
		{"bad app name", func(a *App) { a.Apps["Bad_Name"] = defaultedApp(t) }, "app name"},
		{"missing command", func(a *App) { a.Apps["blog"].Command = nil }, "command is required"},
		{"missing secret", func(a *App) { a.Apps["blog"].WebhookSecret = "" }, "no webhook secret"},
		{"bad health path", func(a *App) { a.Apps["blog"].HealthPath = "health" }, "health_path must start with /"},
		{"zero interval", func(a *App) { a.Apps["blog"].HealthInterval = 0 }, "health_interval must be positive"},
		{"negative soak", func(a *App) { a.Apps["blog"].Soak = caddy.Duration(-time.Second) }, "soak and drain"},
		{"zero keep", func(a *App) { a.Apps["blog"].Keep = 0 }, "keep must be at least 1"},
		{"zero size", func(a *App) { a.Apps["blog"].MaxArtifactSize = 0 }, "max_artifact_size must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := valid()
			tc.mutate(a)
			err := a.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestHealthPathOffIsValid(t *testing.T) {
	a := &App{Root: "/x", Apps: map[string]*AppConfig{"w": defaultedApp(t)}}
	a.Apps["w"].HealthPath = "off"
	if err := a.Validate(); err != nil {
		t.Fatalf("health_path off must validate: %v", err)
	}
}

// versionPathComponent must be the identity for every tag validVersion
// accepts (no behavior change for real deploys) and must mechanically
// neutralize traversal for anything hostile (defense-in-depth beneath
// the validVersion gate).
func TestVersionPathComponent(t *testing.T) {
	for _, v := range []string{"v1.2.3", "2026-08-16", "a_b-c.d", "...", "v1..2", "A"} {
		if !validVersion(v) {
			t.Fatalf("test premise broken: %q should be a valid version", v)
		}
		if got := versionPathComponent(v); got != v {
			t.Errorf("versionPathComponent(%q) = %q, want identity", v, got)
		}
	}
	for in, want := range map[string]string{
		"../../etc/passwd": "etc/passwd",
		"..":               "",
		".":                "",
		"/abs":             "abs",
		"a/../../b":        "b",
	} {
		if got := versionPathComponent(in); got != want {
			t.Errorf("versionPathComponent(%q) = %q, want %q (contained)", in, got, want)
		}
		if got := versionPathComponent(in); strings.Contains(got, "..") || strings.HasPrefix(got, "/") {
			t.Errorf("versionPathComponent(%q) = %q escapes", in, got)
		}
	}
}
