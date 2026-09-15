package liveswap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
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
		by, ok := authorize(ctx, local, mintTestToken(t, priv, "aud1", tc.claims))
		if !ok || by != tc.want {
			t.Errorf("local %s: authorize = %q, %v; want %q", tc.name, by, ok, tc.want)
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
	if by, ok := authorize(ctx, gh, tok); !ok || by != want {
		t.Errorf("github: authorize = %q, %v; want %q", by, ok, want)
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   iss.url,
			"jwks_uri": iss.url + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
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
