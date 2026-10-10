package liveswap

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
)

func TestWarnUnboundTrust(t *testing.T) {
	claims := func(kv ...string) map[string]string {
		m := make(map[string]string, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	github := func(kv ...string) deploytrust.TrustConfig {
		return deploytrust.TrustConfig{Kind: "github", Audience: "a", Claims: claims(kv...)}
	}
	gitlab := func(kv ...string) deploytrust.TrustConfig {
		return deploytrust.TrustConfig{Kind: "gitlab", Audience: "a", Claims: claims(kv...)}
	}
	// resolved is a block after Provision's placeholder pass, with the
	// variable the block names set empty so the value resolves to
	// nothing.
	resolved := func(t *testing.T, tc deploytrust.TrustConfig) []deploytrust.TrustConfig {
		t.Setenv("HOTSERVE_TEST_UNSET", "")
		tcs := []deploytrust.TrustConfig{tc}
		deploytrust.ResolvePlaceholders(caddy.NewReplacer(), tcs)
		return tcs
	}
	type warning struct{ msg, app, fix, preset string } // substrings; app "" = the global block; preset "" = a local block
	unbound := "pins no branch:"
	empty := "resolved empty"
	noAud := "local has no audience"
	// apps nil = one app that inherits the global blocks, so they are read.
	cases := []struct {
		name   string
		global []deploytrust.TrustConfig
		apps   map[string]*AppConfig
		want   []warning
	}{
		{"a github block pinning only a repository warns, and the fix names the ref form",
			[]deploytrust.TrustConfig{github("repository", "o/r")}, nil, []warning{{unbound, "", "claim ref refs/heads/main", "github"}}},
		{"ref binds", []deploytrust.TrustConfig{github("repository", "o/r", "ref", "refs/heads/main")}, nil, nil},
		{"sha binds: one commit is the tightest pin there is", []deploytrust.TrustConfig{github("repository", "o/r", "sha", "0123abcd")}, nil, nil},
		{"environment alone is not a warning (it is an info line, checked below)", []deploytrust.TrustConfig{github("repository", "o/r", "environment", "prod")}, nil, nil},
		{"the JSON subject field binds in its default form",
			[]deploytrust.TrustConfig{{Kind: "github", Audience: "a", Subject: "repo:o/r:ref:refs/heads/main", Claims: claims("repository", "o/r")}}, nil, nil},
		{"claim sub binds (what the Caddyfile's subject becomes)",
			[]deploytrust.TrustConfig{github("repository", "o/r", "sub", "repo:o/r:ref:refs/heads/main")}, nil, nil},
		{"a sub without a ref or environment (the pull_request form, or a customised template) binds nothing",
			[]deploytrust.TrustConfig{github("repository", "o/r", "sub", "repo:o/r:pull_request")}, nil, []warning{{unbound, "", "", "github"}}},
		{"a binding claim whose placeholder resolved empty admits no token: the empty claim is the warning, not the width",
			resolved(t, github("repository", "o/r", "ref", "{env.HOTSERVE_TEST_UNSET}")), nil, []warning{{empty, "", "placeholder", "github"}}},
		{"an identity claim whose placeholder resolved empty admits no token either",
			resolved(t, github("repository", "{env.HOTSERVE_TEST_UNSET}")), nil, []warning{{empty, "", "placeholder", "github"}}},
		{"a JSON subject placeholder that resolved empty is a fail-closed sub constraint, not a vanished one",
			resolved(t, deploytrust.TrustConfig{Kind: "github", Audience: "a", Subject: "{env.HOTSERVE_TEST_UNSET}", Claims: claims("repository", "o/r")}), nil, []warning{{empty, "", "placeholder", "github"}}},
		{"ref_protected binds when pinned true",
			[]deploytrust.TrustConfig{github("repository", "o/r", "ref_protected", "true")}, nil, nil},
		{"ref_protected pinned to any other literal matches no token, so it is not the warning's case either",
			[]deploytrust.TrustConfig{github("repository", "o/r", "ref_protected", "True")}, nil, nil},
		{"ref_protected pinned false admits every unprotected branch",
			[]deploytrust.TrustConfig{github("repository", "o/r", "ref_protected", "false")}, nil, []warning{{unbound, "", "", "github"}}},
		{"workflow_ref embeds the caller's ref and binds",
			[]deploytrust.TrustConfig{github("repository", "o/r", "workflow_ref", "o/r/.github/workflows/deploy.yml@refs/heads/main")}, nil, nil},
		{"job_workflow_ref names the reusable workflow, not the caller's branch: still warns",
			[]deploytrust.TrustConfig{github("repository", "o/r", "job_workflow_ref", "o/shared/.github/workflows/deploy.yml@refs/heads/main")}, nil, []warning{{unbound, "", "", "github"}}},
		{"a gitlab block pinning only a project warns, and the fix names GitLab's qualified ref",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r")}, nil, []warning{{unbound, "", "claim ref_path refs/heads/main", "gitlab"}}},
		{"gitlab ref_path binds",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r", "ref_path", "refs/heads/main")}, nil, nil},
		{"a bare gitlab ref is a name a branch and a tag can share: still warns",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r", "ref", "main")}, nil, []warning{{unbound, "", "ref_path", "gitlab"}}},
		{"a gitlab ref with its ref_type binds",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r", "ref", "main", "ref_type", "branch")}, nil, nil},
		{"gitlab ci_config_ref_uri names the CI configuration's ref, not the pipeline's: still warns",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r", "ci_config_ref_uri", "gitlab.com/o/r//.gitlab-ci.yml@refs/heads/main")}, nil, []warning{{unbound, "", "", "gitlab"}}},
		{"gitlab environment_protected pinned false binds nothing",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r", "environment_protected", "false")}, nil, []warning{{unbound, "", "", "gitlab"}}},
		{"gitlab environment_protected pinned true is an environment binding (an info line, checked below), not a branch",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r", "environment_protected", "true")}, nil, nil},
		{"gitlab environment_protected pinned to another literal matches no token: not the warning's case",
			[]deploytrust.TrustConfig{gitlab("project_path", "o/r", "environment_protected", "yes")}, nil, nil},
		{"the oidc preset never warns: sub is required to load",
			[]deploytrust.TrustConfig{{Kind: "oidc", Issuer: "https://idp.example", Audience: "a", Claims: claims("sub", "ci")}}, nil, nil},
		{"a local block without an audience warns",
			[]deploytrust.TrustConfig{{Kind: "local", PublicKey: "/k.pub"}}, nil, []warning{{noAud, "", "--audience", ""}}},
		{"a local block with an audience is quiet",
			[]deploytrust.TrustConfig{{Kind: "local", PublicKey: "/k.pub", Audience: "box1"}}, nil, nil},
		{"an audience placeholder that resolved empty is no audience",
			resolved(t, deploytrust.TrustConfig{Kind: "local", PublicKey: "/k.pub", Audience: "{env.HOTSERVE_TEST_UNSET}"}), nil, []warning{{noAud, "", "", ""}}},
		{"once per block: an app's own block warns under its name; an app inheriting the global block does not repeat it",
			[]deploytrust.TrustConfig{github("repository", "o/r")},
			map[string]*AppConfig{
				"a": {},
				"b": {DeployTrust: []deploytrust.TrustConfig{{Kind: "local", PublicKey: "/b.pub"}}},
			},
			[]warning{{unbound, "", "", "github"}, {noAud, "b", "", ""}}},
		{"a global block no app inherits backs only the unknown-app 404 path, through which nothing deploys: quiet",
			[]deploytrust.TrustConfig{github("repository", "o/r")},
			map[string]*AppConfig{
				"a": {DeployTrust: []deploytrust.TrustConfig{github("repository", "o/a", "ref", "refs/heads/main")}},
				"b": {DeployTrust: []deploytrust.TrustConfig{{Kind: "local", PublicKey: "/b.pub", Audience: "box"}}},
			},
			nil},
		{"with no app at all nothing deploys, so a global block is not read",
			[]deploytrust.TrustConfig{github("repository", "o/r")}, map[string]*AppConfig{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apps := tc.apps
			if apps == nil {
				apps = map[string]*AppConfig{"app": {}}
			}
			core, logs := observer.New(zap.WarnLevel)
			warnUnboundTrust(zap.New(core), tc.global, apps)
			got := logs.All()
			if len(got) != len(tc.want) {
				t.Fatalf("warned %d times, want %d: %v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				fields := got[i].ContextMap()
				app, _ := fields["app"].(string)
				fix, _ := fields["fix"].(string)
				preset, _ := fields["preset"].(string)
				_, indexed := fields["deploy_trust"]
				if !strings.Contains(got[i].Message, w.msg) || app != w.app || !strings.Contains(fix, w.fix) || preset != w.preset || !indexed {
					t.Errorf("warning %d = %q %v, want message containing %q, app %q, fix containing %q, preset %q, and the block's index", i, got[i].Message, fields, w.msg, w.app, w.fix, w.preset)
				}
			}
		})
	}
	warnUnboundTrust(nil, []deploytrust.TrustConfig{{Kind: "local"}}, nil) // a nil logger is skipped, not dereferenced
}

func TestWarnUnboundTrustNotesAnEnvironmentOnlyBinding(t *testing.T) {
	inherit := map[string]*AppConfig{"app": {}}
	for _, tc := range []deploytrust.TrustConfig{
		{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r", "environment": "prod"}},
		{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r", "sub": "repo:o/r:environment:prod"}},
		{Kind: "gitlab", Audience: "a", Claims: map[string]string{"project_path": "o/r", "environment_protected": "true"}},
	} {
		core, logs := observer.New(zap.InfoLevel)
		warnUnboundTrust(zap.New(core), []deploytrust.TrustConfig{tc}, inherit)
		got := logs.All()
		if len(got) != 1 || got[0].Level != zap.InfoLevel || !strings.Contains(got[0].Message, "environment") {
			t.Fatalf("an environment-only binding must be one info line, got %v", got)
		}
	}
	// With a ref as well there is nothing to note.
	core, logs := observer.New(zap.InfoLevel)
	warnUnboundTrust(zap.New(core), []deploytrust.TrustConfig{{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r", "environment": "prod", "ref": "refs/heads/main"}}}, inherit)
	if logs.Len() != 0 {
		t.Fatalf("a ref beside an environment is bound, got %v", logs.All())
	}
}
