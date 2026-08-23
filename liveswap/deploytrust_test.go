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
	if err := base().verify(ctx, mintTestToken(t, priv, "aud1", want)); err != nil {
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
			if err := v.verify(ctx, tok); err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
		})
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
	if err := v.verify(context.Background(), tok); err != nil {
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

	if err := base().verify(ctx, iss.mint(t, iss.priv, "hotserve", want, time.Now().Add(5*time.Minute))); err != nil {
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
			if err := base().verify(ctx, tc.tok()); err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
		})
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
