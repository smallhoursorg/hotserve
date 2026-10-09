package liveswap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestLocalVerifier(t *testing.T) {
	priv, pub := mustGenTestKey()
	_, otherPub := mustGenTestKey()
	want := map[string]string{"repository": "org/app"}
	base := func() *localVerifier {
		return &localVerifier{audience: "aud1", pub: pub, claims: want}
	}
	ctx := context.Background()

	// The happy path: right key, audience and claims.
	if _, err := base().verify(ctx, mintTestToken(t, priv, "aud1", want)); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	cases := []struct {
		name  string
		build func() (verifier, string)
	}{
		{"wrong key", func() (verifier, string) {
			v := base()
			v.pub = otherPub
			return v, mintTestToken(t, priv, "aud1", want)
		}},
		{"wrong audience", func() (verifier, string) {
			return base(), mintTestToken(t, priv, "other", want)
		}},
		{"claim mismatch", func() (verifier, string) {
			return base(), mintTestToken(t, priv, "aud1", map[string]string{"repository": "evil/app"})
		}},
		{"missing claim", func() (verifier, string) {
			return base(), mintTestToken(t, priv, "aud1", nil)
		}},
		{"expired", func() (verifier, string) {
			return base(), mintExpiredToken(t, priv, "aud1", want)
		}},
		{"garbage", func() (verifier, string) {
			return base(), "not-a-jwt"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, tok := tc.build()
			if _, err := v.verify(ctx, tok); err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
		})
	}
}

func TestOIDCPresetRequiresIdentityClaim(t *testing.T) {
	// Audience alone must be rejected — any repo/project can mint a token
	// for an arbitrary audience.
	for _, tc := range []TrustConfig{
		{Kind: "github", Audience: "hotserve"},
		{Kind: "gitlab", Audience: "hotserve"},
		{Kind: "oidc", Issuer: "https://idp.example", Audience: "hotserve"},
	} {
		if _, err := buildTrust([]TrustConfig{tc}, nil); err == nil {
			t.Errorf("%s with only an audience must be rejected", tc.Kind)
		}
	}
	// With an identity claim it is accepted.
	ok := []TrustConfig{
		{Kind: "github", Audience: "hotserve", Claims: map[string]string{"repository": "o/r"}},
		{Kind: "gitlab", Audience: "hotserve", Claims: map[string]string{"project_path": "o/r"}},
		{Kind: "oidc", Issuer: "https://idp.example", Audience: "hotserve", Claims: map[string]string{"sub": "ci"}},
	}
	for _, tc := range ok {
		if _, err := buildTrust([]TrustConfig{tc}, nil); err != nil {
			t.Errorf("%s with an identity claim rejected: %v", tc.Kind, err)
		}
	}
}

func TestWarnUnboundTrust(t *testing.T) {
	claims := func(kv ...string) map[string]string {
		m := make(map[string]string, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	github := func(kv ...string) TrustConfig {
		return TrustConfig{Kind: "github", Audience: "a", Claims: claims(kv...)}
	}
	gitlab := func(kv ...string) TrustConfig {
		return TrustConfig{Kind: "gitlab", Audience: "a", Claims: claims(kv...)}
	}
	// resolved is a block after Provision's placeholder pass, with the
	// variable the block names set empty so the value resolves to
	// nothing.
	resolved := func(t *testing.T, tc TrustConfig) []TrustConfig {
		t.Setenv("HOTSERVE_TEST_UNSET", "")
		tcs := []TrustConfig{tc}
		resolveTrustPlaceholders(caddy.NewReplacer(), tcs)
		return tcs
	}
	type warning struct{ msg, app, fix, preset string } // substrings; app "" = the global block; preset "" = a local block
	unbound := "pins no branch:"
	empty := "resolved empty"
	noAud := "local has no audience"
	// apps nil = one app that inherits the global blocks, so they are read.
	cases := []struct {
		name   string
		global []TrustConfig
		apps   map[string]*AppConfig
		want   []warning
	}{
		{"a github block pinning only a repository warns, and the fix names the ref form",
			[]TrustConfig{github("repository", "o/r")}, nil, []warning{{unbound, "", "claim ref refs/heads/main", "github"}}},
		{"ref binds", []TrustConfig{github("repository", "o/r", "ref", "refs/heads/main")}, nil, nil},
		{"sha binds: one commit is the tightest pin there is", []TrustConfig{github("repository", "o/r", "sha", "0123abcd")}, nil, nil},
		{"environment alone is not a warning (it is an info line, checked below)", []TrustConfig{github("repository", "o/r", "environment", "prod")}, nil, nil},
		{"the JSON subject field binds in its default form",
			[]TrustConfig{{Kind: "github", Audience: "a", Subject: "repo:o/r:ref:refs/heads/main", Claims: claims("repository", "o/r")}}, nil, nil},
		{"claim sub binds (what the Caddyfile's subject becomes)",
			[]TrustConfig{github("repository", "o/r", "sub", "repo:o/r:ref:refs/heads/main")}, nil, nil},
		{"a sub without a ref or environment (the pull_request form, or a customised template) binds nothing",
			[]TrustConfig{github("repository", "o/r", "sub", "repo:o/r:pull_request")}, nil, []warning{{unbound, "", "", "github"}}},
		{"a binding claim whose placeholder resolved empty admits no token: the empty claim is the warning, not the width",
			resolved(t, github("repository", "o/r", "ref", "{env.HOTSERVE_TEST_UNSET}")), nil, []warning{{empty, "", "placeholder", "github"}}},
		{"an identity claim whose placeholder resolved empty admits no token either",
			resolved(t, github("repository", "{env.HOTSERVE_TEST_UNSET}")), nil, []warning{{empty, "", "placeholder", "github"}}},
		{"a JSON subject placeholder that resolved empty is a fail-closed sub constraint, not a vanished one",
			resolved(t, TrustConfig{Kind: "github", Audience: "a", Subject: "{env.HOTSERVE_TEST_UNSET}", Claims: claims("repository", "o/r")}), nil, []warning{{empty, "", "placeholder", "github"}}},
		{"ref_protected binds when pinned true",
			[]TrustConfig{github("repository", "o/r", "ref_protected", "true")}, nil, nil},
		{"ref_protected pinned to any other literal matches no token, so it is not the warning's case either",
			[]TrustConfig{github("repository", "o/r", "ref_protected", "True")}, nil, nil},
		{"ref_protected pinned false admits every unprotected branch",
			[]TrustConfig{github("repository", "o/r", "ref_protected", "false")}, nil, []warning{{unbound, "", "", "github"}}},
		{"workflow_ref embeds the caller's ref and binds",
			[]TrustConfig{github("repository", "o/r", "workflow_ref", "o/r/.github/workflows/deploy.yml@refs/heads/main")}, nil, nil},
		{"job_workflow_ref names the reusable workflow, not the caller's branch: still warns",
			[]TrustConfig{github("repository", "o/r", "job_workflow_ref", "o/shared/.github/workflows/deploy.yml@refs/heads/main")}, nil, []warning{{unbound, "", "", "github"}}},
		{"a gitlab block pinning only a project warns, and the fix names GitLab's qualified ref",
			[]TrustConfig{gitlab("project_path", "o/r")}, nil, []warning{{unbound, "", "claim ref_path refs/heads/main", "gitlab"}}},
		{"gitlab ref_path binds",
			[]TrustConfig{gitlab("project_path", "o/r", "ref_path", "refs/heads/main")}, nil, nil},
		{"a bare gitlab ref is a name a branch and a tag can share: still warns",
			[]TrustConfig{gitlab("project_path", "o/r", "ref", "main")}, nil, []warning{{unbound, "", "ref_path", "gitlab"}}},
		{"a gitlab ref with its ref_type binds",
			[]TrustConfig{gitlab("project_path", "o/r", "ref", "main", "ref_type", "branch")}, nil, nil},
		{"gitlab ci_config_ref_uri names the CI configuration's ref, not the pipeline's: still warns",
			[]TrustConfig{gitlab("project_path", "o/r", "ci_config_ref_uri", "gitlab.com/o/r//.gitlab-ci.yml@refs/heads/main")}, nil, []warning{{unbound, "", "", "gitlab"}}},
		{"gitlab environment_protected pinned false binds nothing",
			[]TrustConfig{gitlab("project_path", "o/r", "environment_protected", "false")}, nil, []warning{{unbound, "", "", "gitlab"}}},
		{"gitlab environment_protected pinned true is an environment binding (an info line, checked below), not a branch",
			[]TrustConfig{gitlab("project_path", "o/r", "environment_protected", "true")}, nil, nil},
		{"gitlab environment_protected pinned to another literal matches no token: not the warning's case",
			[]TrustConfig{gitlab("project_path", "o/r", "environment_protected", "yes")}, nil, nil},
		{"the oidc preset never warns: sub is required to load",
			[]TrustConfig{{Kind: "oidc", Issuer: "https://idp.example", Audience: "a", Claims: claims("sub", "ci")}}, nil, nil},
		{"a local block without an audience warns",
			[]TrustConfig{{Kind: "local", PublicKey: "/k.pub"}}, nil, []warning{{noAud, "", "--audience", ""}}},
		{"a local block with an audience is quiet",
			[]TrustConfig{{Kind: "local", PublicKey: "/k.pub", Audience: "box1"}}, nil, nil},
		{"an audience placeholder that resolved empty is no audience",
			resolved(t, TrustConfig{Kind: "local", PublicKey: "/k.pub", Audience: "{env.HOTSERVE_TEST_UNSET}"}), nil, []warning{{noAud, "", "", ""}}},
		{"once per block: an app's own block warns under its name; an app inheriting the global block does not repeat it",
			[]TrustConfig{github("repository", "o/r")},
			map[string]*AppConfig{
				"a": {},
				"b": {DeployTrust: []TrustConfig{{Kind: "local", PublicKey: "/b.pub"}}},
			},
			[]warning{{unbound, "", "", "github"}, {noAud, "b", "", ""}}},
		{"a global block no app inherits backs only the unknown-app 404 path, through which nothing deploys: quiet",
			[]TrustConfig{github("repository", "o/r")},
			map[string]*AppConfig{
				"a": {DeployTrust: []TrustConfig{github("repository", "o/a", "ref", "refs/heads/main")}},
				"b": {DeployTrust: []TrustConfig{{Kind: "local", PublicKey: "/b.pub", Audience: "box"}}},
			},
			nil},
		{"with no app at all nothing deploys, so a global block is not read",
			[]TrustConfig{github("repository", "o/r")}, map[string]*AppConfig{}, nil},
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
	warnUnboundTrust(nil, []TrustConfig{{Kind: "local"}}, nil) // a nil logger is skipped, not dereferenced
}

func TestSubjectAndClaimSubAreRefusedTogether(t *testing.T) {
	// The Caddyfile parser refuses the pair; the JSON form must too, or
	// one reader would have to pick a winner and the other could
	// disagree (the warning once did).
	_, err := buildTrust([]TrustConfig{{Kind: "github", Audience: "a", Subject: "repo:o/r:pull_request", Claims: map[string]string{"repository": "o/r", "sub": "repo:o/r:ref:refs/heads/main"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("subject and claim sub together must be refused at load, got %v", err)
	}
}

func TestResolveTrustPlaceholdersFoldsTheJSONSubject(t *testing.T) {
	// The JSON subject becomes the sub claim at resolution, as the
	// Caddyfile parser already makes it, so an empty resolution is a
	// fail-closed constraint rather than a vanished one.
	t.Setenv("HOTSERVE_TEST_SUB", "repo:o/r:ref:refs/heads/main")
	tcs := []TrustConfig{{Kind: "github", Audience: "a", Subject: "{env.HOTSERVE_TEST_SUB}", Claims: map[string]string{"repository": "o/r"}}}
	resolveTrustPlaceholders(caddy.NewReplacer(), tcs)
	if tcs[0].Subject != "" || tcs[0].Claims["sub"] != "repo:o/r:ref:refs/heads/main" {
		t.Fatalf("subject must become the resolved sub claim, got %+v", tcs[0])
	}
	t.Setenv("HOTSERVE_TEST_SUB", "")
	tcs = []TrustConfig{{Kind: "github", Audience: "a", Subject: "{env.HOTSERVE_TEST_SUB}", Claims: map[string]string{"repository": "o/r"}}}
	resolveTrustPlaceholders(caddy.NewReplacer(), tcs)
	if v, ok := tcs[0].Claims["sub"]; !ok || v != "" {
		t.Fatalf("an empty subject must stay a sub=\"\" constraint, got %+v", tcs[0])
	}
	// Beside a claims.sub it is left as written, and the pair is refused.
	tcs = []TrustConfig{{Kind: "github", Audience: "a", Subject: "{env.HOTSERVE_TEST_SUB}", Claims: map[string]string{"repository": "o/r", "sub": "x"}}}
	resolveTrustPlaceholders(caddy.NewReplacer(), tcs)
	if _, err := buildTrust(tcs, nil); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("subject beside claim sub must be refused after resolution, got %v", err)
	}
}

func TestWarnUnboundTrustNotesAnEnvironmentOnlyBinding(t *testing.T) {
	inherit := map[string]*AppConfig{"app": {}}
	for _, tc := range []TrustConfig{
		{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r", "environment": "prod"}},
		{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r", "sub": "repo:o/r:environment:prod"}},
		{Kind: "gitlab", Audience: "a", Claims: map[string]string{"project_path": "o/r", "environment_protected": "true"}},
	} {
		core, logs := observer.New(zap.InfoLevel)
		warnUnboundTrust(zap.New(core), []TrustConfig{tc}, inherit)
		got := logs.All()
		if len(got) != 1 || got[0].Level != zap.InfoLevel || !strings.Contains(got[0].Message, "environment") {
			t.Fatalf("an environment-only binding must be one info line, got %v", got)
		}
	}
	// With a ref as well there is nothing to note.
	core, logs := observer.New(zap.InfoLevel)
	warnUnboundTrust(zap.New(core), []TrustConfig{{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r", "environment": "prod", "ref": "refs/heads/main"}}}, inherit)
	if logs.Len() != 0 {
		t.Fatalf("a ref beside an environment is bound, got %v", logs.All())
	}
}

func TestLocalTokenRequiresExp(t *testing.T) {
	priv, pub := mustGenTestKey()
	noExp := signEdDSA(t, priv, map[string]any{
		"aud": "a", "iat": jwt.NewNumericDate(time.Now()),
	})
	v := &localVerifier{audience: "a", pub: pub}
	if _, err := v.verify(context.Background(), noExp); err == nil {
		t.Fatal("local token without exp must be rejected")
	}
}

func TestNewJWKSClientEnforcesHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close() // plain http

	if _, err := newJWKSClient(false).Get(srv.URL); err == nil {
		t.Error("newJWKSClient(false) must refuse a plain-http request")
	}
	resp, err := newJWKSClient(true).Get(srv.URL)
	if err != nil {
		t.Errorf("newJWKSClient(true) must allow http: %v", err)
	} else {
		_ = resp.Body.Close()
	}
}

// TestLocalKeyFileRoundtrip proves the on-disk PEM formats produced by
// `deploy-keygen` load back through loadEd25519PublicKey /
// loadEd25519PrivateKey and interoperate with the local verifier.
func TestLocalKeyFileRoundtrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privPath := dir + "/deploy.key"
	pubPath := privPath + ".pub"
	privDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	if err := os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		t.Fatal(err)
	}

	loadedPub, err := loadEd25519PublicKey(pubPath)
	if err != nil {
		t.Fatalf("load public key: %v", err)
	}
	loadedPriv, err := loadEd25519PrivateKey(privPath)
	if err != nil {
		t.Fatalf("load private key: %v", err)
	}
	v := &localVerifier{audience: "hotserve", pub: loadedPub, claims: map[string]string{"repository": "org/app"}}
	tok := mintTestToken(t, loadedPriv, "hotserve", map[string]string{"repository": "org/app"})
	if _, err := v.verify(context.Background(), tok); err != nil {
		t.Fatalf("round-trip token rejected: %v", err)
	}
}

func TestOIDCVerifier(t *testing.T) {
	iss := newMockIssuer(t)
	want := map[string]string{"repository": "org/app"}
	base := func() *oidcVerifier {
		return &oidcVerifier{issuer: iss.url, audience: "hotserve", claims: want, client: iss.client}
	}
	ctx := context.Background()

	if _, err := base().verify(ctx, iss.mint(t, iss.priv, "hotserve", want, time.Now().Add(5*time.Minute))); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	cases := []struct {
		name string
		tok  func() string
	}{
		{"wrong audience", func() string {
			return iss.mint(t, iss.priv, "other", want, time.Now().Add(5*time.Minute))
		}},
		{"claim mismatch", func() string {
			return iss.mint(t, iss.priv, "hotserve", map[string]string{"repository": "evil/app"}, time.Now().Add(5*time.Minute))
		}},
		{"missing claim", func() string {
			return iss.mint(t, iss.priv, "hotserve", nil, time.Now().Add(5*time.Minute))
		}},
		{"expired", func() string {
			return iss.mint(t, iss.priv, "hotserve", want, time.Now().Add(-time.Hour))
		}},
		{"unknown signing key", func() string {
			other, _ := rsa.GenerateKey(rand.Reader, 2048)
			return iss.mint(t, other, "hotserve", want, time.Now().Add(5*time.Minute))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := base().verify(ctx, tc.tok()); err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
		})
	}
}

// TestOIDCVerifierNumericClaim pins a numeric identity claim
// (repository_id, one of the documented GitHub identity claims). The
// value arrives in the token as a JSON number, and the verifier must
// compare it by its exact decimal digits. A round-numbered id decoded to
// float64 would stringify as "1e+08" and never match — the S1 bug — so
// this guards the json.Number decode path.
func TestOIDCVerifierNumericClaim(t *testing.T) {
	iss := newMockIssuer(t)
	v := &oidcVerifier{
		issuer: iss.url, audience: "hotserve",
		claims: map[string]string{"repository_id": "100000000"},
		client: iss.client,
	}
	tok := iss.mintClaims(t, "hotserve", time.Now().Add(5*time.Minute),
		map[string]any{"repository_id": 100000000})
	if _, err := v.verify(context.Background(), tok); err != nil {
		t.Fatalf("token with numeric repository_id rejected: %v", err)
	}
	// A different numeric id must still be rejected.
	bad := iss.mintClaims(t, "hotserve", time.Now().Add(5*time.Minute),
		map[string]any{"repository_id": 999})
	if _, err := v.verify(context.Background(), bad); err == nil {
		t.Fatal("wrong repository_id must be rejected")
	}

	// An integer beyond 2^53 (float64's exact-integer ceiling): this only
	// verifies if the claim kept its exact digits through decoding, so it
	// guards the json.Number path against any regression to float64
	// reformatting (which would round 2^53+1 down to 2^53).
	const big = int64(9007199254740993) // 2^53 + 1
	bigV := &oidcVerifier{
		issuer: iss.url, audience: "hotserve",
		claims: map[string]string{"repository_id": "9007199254740993"},
		client: iss.client,
	}
	bigTok := iss.mintClaims(t, "hotserve", time.Now().Add(5*time.Minute),
		map[string]any{"repository_id": big})
	if _, err := bigV.verify(context.Background(), bigTok); err != nil {
		t.Fatalf("token with a >2^53 repository_id rejected: %v", err)
	}
}

// eventually retries check for a short while. go-oidc's key set
// signals a finished fetch to its waiters before it records the
// result under its lock, so the verify right after an issuer's state
// changed can still see the previous fetch; a bounded retry keeps the
// property under test (recovery without a restart) honest.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	var err error
	for range 40 {
		if err = check(); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

// TestOIDCVerifierIssuerDown pins what an issuer the box cannot get
// what it needs from looks like to the caller of verify: an
// unavailable, not a refusal — the token was never judged — for both
// discovery and the key fetch. The key-fetch case holds keyFetchFailed
// against the pinned go-oidc (the text is the only handle on it). It
// also pins what is NOT an outage: a token the cached keys verify
// still deploys, a malformed token is its own refusal, a caller that
// went away is its own failure, and an issuer answering as someone
// else is the config's.
func TestOIDCVerifierIssuerDown(t *testing.T) {
	iss := newMockIssuer(t)
	want := map[string]string{"repository": "org/app"}
	fresh := func() *oidcVerifier {
		return &oidcVerifier{issuer: iss.url, audience: "hotserve", claims: want, client: iss.client}
	}
	valid := func() string { return iss.mint(t, iss.priv, "hotserve", want, time.Now().Add(5*time.Minute)) }
	ctx := context.Background()
	wantDown := func(t *testing.T, err error, what string) unavailable {
		t.Helper()
		var u unavailable
		if !errors.As(err, &u) {
			t.Fatalf("%s: err = %v, want an unavailable", what, err)
		}
		if u.label != "oidc:"+iss.url {
			t.Fatalf("%s: label = %q, want the source's", what, u.label)
		}
		return u
	}
	wantPlain := func(t *testing.T, err error, what string) {
		t.Helper()
		if err == nil || errors.As(err, &unavailable{}) {
			t.Fatalf("%s: err = %v, want a plain refusal", what, err)
		}
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()

	iss.discoveryDown.Store(true)
	_, err := fresh().verify(ctx, valid())
	u := wantDown(t, err, "discovery 503")
	if !strings.Contains(u.Error(), "oidc discovery for") {
		t.Fatalf("discovery 503: reason = %q, want it to say discovery", u.Error())
	}
	_, err = fresh().verify(gone, valid())
	wantPlain(t, err, "discovery with the caller gone")
	iss.discoveryDown.Store(false)

	iss.discoveredAs.Store("https://someone-else.example")
	_, err = fresh().verify(ctx, valid())
	wantPlain(t, err, "discovery naming another issuer")
	if !strings.Contains(err.Error(), "someone-else.example") {
		t.Fatalf("issuer mismatch: reason = %q, want it to name the issuer found", err.Error())
	}
	iss.discoveredAs.Store("")

	iss.jwksDown.Store(true)
	v := fresh()
	_, err = v.verify(ctx, valid())
	u = wantDown(t, err, "JWKS 503 on first use")
	if !strings.HasPrefix(u.Error(), keyFetchFailed) {
		t.Fatalf("JWKS 503: reason = %q, want the keyFetchFailed prefix %q", u.Error(), keyFetchFailed)
	}
	_, err = v.verify(gone, valid())
	wantPlain(t, err, "key fetch with the caller gone")

	// Recovery needs no restart: the provider is cached, the key set
	// fetches again.
	iss.jwksDown.Store(false)
	eventually(t, "issuer back: valid token rejected", func() error {
		_, err := v.verify(ctx, valid())
		return err
	})

	// Down again, keys cached: a token those keys verify still deploys
	// — an outage only reaches tokens the box has no key for.
	iss.jwksDown.Store(true)
	if _, err := v.verify(ctx, valid()); err != nil {
		t.Fatalf("issuer down with keys cached: valid token rejected: %v", err)
	}
	// A malformed token never reaches the fetch: a refusal of its own.
	_, err = v.verify(ctx, "not-a-jwt")
	wantPlain(t, err, "malformed token during an outage")
	// A bad signature under a known kid refetches (go-oidc's rotation
	// strategy), so during an outage it cannot be told from a rotated
	// key: unavailable, deliberately.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	_, err = v.verify(ctx, iss.mint(t, other, "hotserve", want, time.Now().Add(5*time.Minute)))
	_ = wantDown(t, err, "bad signature during an outage")
}

// TestAuthorizeNamesEverySourceItCouldNotConsult: down lists the
// sources that could not be consulted, in config order, whether the
// token was then refused (and the journal text still carries every
// source's reason) or accepted by a source after them — a fallback
// that keeps deploys going must not hide the outage.
func TestAuthorizeNamesEverySourceItCouldNotConsult(t *testing.T) {
	iss := newMockIssuer(t)
	iss.discoveryDown.Store(true)
	priv, pub := mustGenTestKey()
	local := &localVerifier{audience: "hotserve", pub: pub, keyPath: "/k.pem"}
	down := &oidcVerifier{issuer: iss.url, audience: "hotserve", client: iss.client}
	labels := func(down []unavailable) string {
		var out []string
		for _, u := range down {
			out = append(out, u.label)
		}
		return strings.Join(out, " ")
	}
	bad := mintTestToken(t, priv, "other", nil) // the local source refuses it: wrong audience
	for _, order := range [][]verifier{{local, down}, {down, local}} {
		_, got, err := authorize(context.Background(), order, bad)
		if err == nil || labels(got) != down.label() {
			t.Fatalf("order %v: err = %v, down = %q, want a refusal naming %s", order, err, labels(got), down.label())
		}
		if !strings.Contains(err.Error(), "local:/k.pem: ") || !strings.Contains(err.Error(), "oidc:"+iss.url+": ") {
			t.Fatalf("order %v: reasons = %q, want both sources named", order, err.Error())
		}
	}
	// Two sources down: both named, in config order.
	iss2 := newMockIssuer(t)
	iss2.discoveryDown.Store(true)
	down2 := &oidcVerifier{issuer: iss2.url, audience: "hotserve", client: iss2.client}
	if _, got, err := authorize(context.Background(), []verifier{down, local, down2}, bad); err == nil || labels(got) != down.label()+" "+down2.label() {
		t.Fatalf("two sources down: err = %v, down = %q, want both", err, labels(got))
	}
	// A source after the down one accepts the token: authorized, and
	// the down one is still named.
	good := mintTestToken(t, priv, "hotserve", nil)
	if by, got, err := authorize(context.Background(), []verifier{down, local}, good); err != nil || by != local.label() || labels(got) != down.label() {
		t.Fatalf("accepted past a down source: by = %q, down = %q, err = %v", by, labels(got), err)
	}
	// Both up: nothing is down, and the bad token is a plain refusal.
	iss.discoveryDown.Store(false)
	if _, got, err := authorize(context.Background(), []verifier{local, down}, bad); err == nil || len(got) != 0 {
		t.Fatalf("both sources up: err = %v, down = %q, want a plain refusal", err, labels(got))
	}
}

// TestAttribute pins the form a deploy is recorded under: the label,
// then each named claim in the order named, a value that could be read
// as more than one field quoted.
func TestAttribute(t *testing.T) {
	names := []string{"repository", "ref", "actor"}
	for _, tc := range []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"all present, in list order", map[string]any{"actor": "alice", "ref": "refs/heads/main", "repository": "org/blog"},
			"L repository=org/blog ref=refs/heads/main actor=alice"},
		{"absent claims left out", map[string]any{"ref": "refs/heads/main"}, "L ref=refs/heads/main"},
		{"none present", map[string]any{"sub": "x"}, "L"},
		{"non-scalar and null left out", map[string]any{"repository": []any{"a"}, "ref": map[string]any{}, "actor": nil}, "L"},
		{"number by its digits", map[string]any{"actor": json.Number("9007199254740993")}, "L actor=9007199254740993"},
		{"space quoted", map[string]any{"actor": "alice actor=bob"}, `L actor="alice actor=bob"`},
		{"quote and backslash quoted", map[string]any{"actor": `a"b\c`}, `L actor="a\"b\\c"`},
		{"control rune quoted", map[string]any{"actor": "a\nb"}, `L actor="a\nb"`},
		{"non-ASCII space quoted", map[string]any{"actor": "a" + string(rune(0xa0)) + "b"}, `L actor="a\` + "u00a0b\""},
		{"empty quoted", map[string]any{"actor": ""}, `L actor=""`},
		{"printable unicode as is", map[string]any{"actor": "zoë"}, "L actor=zoë"},
		{"= inside a value as is", map[string]any{"ref": "refs/heads/a=b"}, "L ref=refs/heads/a=b"},
	} {
		if got := attribute("L", names, tc.claims); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPresetAttribution pins which claims each preset records, carried
// past the fold of github/gitlab into kind "oidc".
func TestPresetAttribution(t *testing.T) {
	dir := t.TempDir()
	_, pub := mustGenTestKey()
	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	pubPath := dir + "/k.pub"
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tc   TrustConfig
		want string
	}{
		{TrustConfig{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r"}}, "repository ref actor"},
		{TrustConfig{Kind: "gitlab", Audience: "a", Claims: map[string]string{"project_path": "o/r"}}, "project_path ref user_login"},
		{TrustConfig{Kind: "oidc", Issuer: "https://idp.example", Audience: "a", Subject: "ci"}, "sub"},
		{TrustConfig{Kind: "local", PublicKey: pubPath}, "sub"},
	} {
		srcs, err := buildTrust([]TrustConfig{tc.tc}, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.tc.Kind, err)
		}
		if got := strings.Join(srcs[0].attribution, " "); got != tc.want {
			t.Errorf("%s records %q, want %q", tc.tc.Kind, got, tc.want)
		}
	}
}

// TestAuthorizeAttributes pins that authorize hands back the accepting
// verifier's label with the token's claims, for either kind — the
// string deployed_by and the `deploy authorized` line carry.
func TestAuthorizeAttributes(t *testing.T) {
	ctx := context.Background()
	priv, pub := mustGenTestKey()
	local := resolveVerifiers([]trustSource{localTrust(pub, "aud1")}, nil)
	for _, tc := range []struct {
		name   string
		claims map[string]string
		want   string
	}{
		{"with a subject", map[string]string{"sub": "alice"}, "local:test-key sub=alice"},
		{"without one", nil, "local:test-key"},
	} {
		by, _, err := authorize(ctx, local, mintTestToken(t, priv, "aud1", tc.claims))
		if err != nil || by != tc.want {
			t.Errorf("local %s: authorize = %q, %v; want %q", tc.name, by, err, tc.want)
		}
	}

	iss := newMockIssuer(t)
	gh := resolveVerifiers([]trustSource{{
		kind: "oidc", issuer: iss.url, audience: "hotserve",
		claims: map[string]string{"repository": "org/blog"}, attribution: attributionClaims["github"],
	}}, iss.client)
	tok := iss.mint(t, iss.priv, "hotserve", map[string]string{
		"repository": "org/blog", "ref": "refs/heads/main", "actor": "alice",
		"sub": "repo:org/blog:ref:refs/heads/main",
	}, time.Now().Add(5*time.Minute))
	want := "oidc:" + iss.url + " repository=org/blog ref=refs/heads/main actor=alice"
	if by, _, err := authorize(ctx, gh, tok); err != nil || by != want {
		t.Errorf("github: authorize = %q, %v; want %q", by, err, want)
	}
}

// TestAuthorizeSaysWhy pins what a refused token leaves for the
// operator's journal: which source refused it and why, in config
// order, with the identity a verified token presented — and that the
// two cases where no source is asked say so. What a caller sees is
// pinned in handler_test (the flat 401, whatever the reason).
func TestAuthorizeSaysWhy(t *testing.T) {
	ctx := context.Background()
	priv, pub := mustGenTestKey()
	local := resolveVerifiers([]trustSource{localTrust(pub, "aud1")}, nil)
	iss := newMockIssuer(t)
	gh := resolveVerifiers([]trustSource{{
		kind: "oidc", issuer: iss.url, audience: "hotserve",
		claims: map[string]string{"repository": "org/blog"}, attribution: attributionClaims["github"],
	}}, iss.client)
	both := append(append([]verifier{}, gh...), local...)
	otherRSA, _ := rsa.GenerateKey(rand.Reader, 2048)
	mint := func(aud string, claims map[string]string) string {
		return iss.mint(t, iss.priv, aud, claims, time.Now().Add(5*time.Minute))
	}
	for _, tc := range []struct {
		name string
		vs   []verifier
		tok  string
		want []string // each in the refusal, in this order
	}{
		{"no token", local, "", []string{"no bearer token"}},
		{"no source", nil, mintTestToken(t, priv, "aud1", nil), []string{"no deploy_trust source"}},
		{"garbage", local, "not-a-jwt", []string{"local:test-key: "}},
		{"wrong audience", local, mintTestToken(t, priv, "other", nil), []string{"local:test-key: ", "aud"}},
		{"expired", local, mintExpiredToken(t, priv, "aud1", nil), []string{"local:test-key: ", "exp"}},
		{"unknown signer", gh, iss.mint(t, otherRSA, "hotserve", nil, time.Now().Add(time.Minute)), []string{"oidc:" + iss.url + ": "}},
		{"claim mismatch names who tried", gh,
			mint("hotserve", map[string]string{"repository": "org/other", "ref": "refs/heads/main", "actor": "alice"}),
			[]string{"oidc:" + iss.url + ": claim \"repository\" mismatch, presented repository=org/other ref=refs/heads/main actor=alice"}},
		{"claim absent, nothing presented", gh, mint("hotserve", nil), []string{"claim \"repository\" absent from token, presented nothing"}},
		{"every source, config order", both, "not-a-jwt", []string{"oidc:" + iss.url + ": ", "; local:test-key: "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			by, _, err := authorize(ctx, tc.vs, tc.tok)
			if err == nil {
				t.Fatalf("authorized as %q", by)
			}
			got := err.Error()
			at := 0
			for _, w := range tc.want {
				i := strings.Index(got[at:], w)
				if i < 0 {
					t.Fatalf("refusal %q lacks %q (after offset %d)", got, w, at)
				}
				at += i + len(w)
			}
		})
	}

	// A refusal is bounded however long the token's own text is — the
	// audience go-oidc quotes back, or the identity a verified token
	// presents, in ASCII or not — it stays valid UTF-8 on one line, and
	// the cut takes the identity, never the reason in front of it.
	for name, tok := range map[string]string{
		"long audience":       mint(strings.Repeat("a", 10_000), nil),
		"long ascii identity": mint("hotserve", map[string]string{"repository": "org/" + strings.Repeat("r", 400)}),
		"long emoji identity": mint("hotserve", map[string]string{"repository": "org/" + strings.Repeat("😀", 200), "ref": "refs/heads/é"}),
		"control in identity": mint("hotserve", map[string]string{"repository": "org/" + strings.Repeat("a\n", 200)}),
	} {
		by, _, err := authorize(ctx, gh, tok)
		if err == nil {
			t.Fatalf("%s: authorized as %q", name, by)
		}
		got := err.Error()
		if len(got) > len(gh[0].label())+2+maxRefusalLen+len("...") {
			t.Errorf("%s: refusal is %d bytes; the reason must be cut at %d", name, len(got), maxRefusalLen)
		}
		if !utf8.ValidString(got) || strings.ContainsRune(got, '\n') {
			t.Errorf("%s: refusal is not one line of UTF-8: %q", name, got)
		}
		if strings.Contains(name, "identity") && !strings.Contains(got, `claim "repository" mismatch`) {
			t.Errorf("%s: the cut took the reason: %q", name, got)
		}
	}
}

// TestBoundRefusal pins what a journal line needs of a refusal that
// may carry request text: one line, valid UTF-8, and at most
// maxRefusalLen bytes plus the ellipsis — whatever the bytes.
func TestBoundRefusal(t *testing.T) {
	if got := boundRefusal("plain: claim \"repository\" mismatch"); got != "plain: claim \"repository\" mismatch" {
		t.Errorf("a plain refusal must pass through, got %q", got)
	}
	if got := boundRefusal("a\nb"); got != `"a\nb"` {
		t.Errorf("a control rune must leave the refusal Go-quoted, got %q", got)
	}
	if got := boundRefusal("a\xffb"); got != `"a\xffb"` {
		t.Errorf("a byte that is not UTF-8 must leave the refusal Go-quoted, got %q", got)
	}
	long := strings.Repeat("x", maxRefusalLen+50)
	if got := boundRefusal(long); got != long[:maxRefusalLen]+"..." {
		t.Errorf("an overlong refusal must be cut, got %d bytes", len(got))
	}
	for name, in := range map[string]string{
		"emoji":        strings.Repeat("😀", maxRefusalLen),
		"emoji at cut": strings.Repeat("x", maxRefusalLen-1) + "😀😀",
		"newlines":     strings.Repeat("\n", maxRefusalLen),
		"bad bytes":    strings.Repeat("\xff", maxRefusalLen),
	} {
		got := boundRefusal(in)
		if len(got) > maxRefusalLen+len("...") {
			t.Errorf("%s: %d bytes, must be at most %d", name, len(got), maxRefusalLen+3)
		}
		if !utf8.ValidString(got) || strings.ContainsFunc(got, func(r rune) bool { return !strconv.IsPrint(r) && r != ' ' }) {
			t.Errorf("%s: not one printable line of UTF-8: %q", name, got)
		}
	}
}

// TestMatchClaimsNumeric exercises the stringification directly across
// the number representations a claim set can carry.
func TestMatchClaimsNumeric(t *testing.T) {
	want := map[string]string{"repository_id": "100000000"}
	// The decodeClaims path yields json.Number.
	if err := matchClaims(want, map[string]any{"repository_id": json.Number("100000000")}); err != nil {
		t.Errorf("json.Number claim rejected: %v", err)
	}
	// A float64 claim means the set was decoded without UseNumber and
	// its digits are no longer trustworthy: refused, never formatted.
	if err := matchClaims(want, map[string]any{"repository_id": float64(100000000)}); err == nil {
		t.Error("float64 claim must be refused, not formatted and matched")
	}
	if err := matchClaims(want, map[string]any{"repository_id": json.Number("99")}); err == nil {
		t.Error("mismatched numeric claim must be rejected")
	}
	// A 2^53+1 integer survives json.Number exactly (float64 would not).
	big := map[string]string{"repository_id": "9007199254740993"}
	if err := matchClaims(big, map[string]any{"repository_id": json.Number("9007199254740993")}); err != nil {
		t.Errorf("exact big-int claim rejected: %v", err)
	}
	// A composite (array) claim must never match a configured string, even
	// one spelled like its %v rendering.
	if err := matchClaims(map[string]string{"groups": "[admin]"},
		map[string]any{"groups": []any{"admin"}}); err == nil {
		t.Error("array-valued claim must not match a string constraint")
	}
}

// mockIssuer is a minimal OIDC provider: a discovery document and a
// JWKS, backed by an RSA key, so oidcVerifier can be exercised offline.
type mockIssuer struct {
	url    string
	client *http.Client
	priv   *rsa.PrivateKey
	kid    string
	// discoveryDown / jwksDown make that endpoint answer 503: the
	// issuer is up but the box cannot get what it needs from it.
	discoveryDown, jwksDown atomic.Bool
	// discoveredAs, when set, is the issuer the discovery document
	// claims — someone other than the URL it was fetched from.
	discoveredAs atomic.Value
}

func newMockIssuer(t *testing.T) *mockIssuer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &mockIssuer{priv: priv, kid: "test-key"}
	pubJWK := jose.JSONWebKey{Key: priv.Public(), KeyID: iss.kid, Algorithm: "RS256", Use: "sig"}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pubJWK}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if iss.discoveryDown.Load() {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		issuer := iss.url
		if as, ok := iss.discoveredAs.Load().(string); ok && as != "" {
			issuer = as
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   issuer,
			"jwks_uri": iss.url + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		if iss.jwksDown.Load() {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(jwks)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.url = srv.URL
	iss.client = srv.Client()
	return iss
}

// mint signs a token as this issuer (or with an off-key private key, to
// exercise the unknown-signing-key path).
func (iss *mockIssuer) mint(t *testing.T, priv *rsa.PrivateKey, audience string, claims map[string]string, exp time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: priv, KeyID: iss.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(signer).Claims(claimMap(iss.url, audience, time.Now(), exp, claims)).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// mintClaims signs a token as this issuer with arbitrary custom claims,
// so a test can carry a JSON number (not just string claims) in the
// payload.
func (iss *mockIssuer) mintClaims(t *testing.T, audience string, exp time.Time, custom map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: iss.priv, KeyID: iss.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{
		"iss": iss.url,
		"aud": audience,
		"iat": jwt.NewNumericDate(time.Now()),
		"exp": jwt.NewNumericDate(exp),
	}
	for k, v := range custom {
		m[k] = v
	}
	tok, err := jwt.Signed(signer).Claims(m).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
