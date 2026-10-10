// Package trusttest mints the deploy tokens tests present to
// deploytrust, and stands up an OIDC issuer in-process for them: a
// local key with its PKIX file as an operator writes one, and an
// issuer that serves discovery and JWKS and signs tokens, standing in
// for GitHub or GitLab. For tests only — liveswap's, deploytrust's and
// the box webhook's, which authenticate the same way — in the shape of
// net/http/httptest, so the three suites share one set of helpers.
package trusttest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// GenerateKey is a fresh ed25519 keypair, private half first.
func GenerateKey() (ed25519.PrivateKey, ed25519.PublicKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv, pub
}

// KeyFile writes pub as the PKIX PEM file `hotserve deploy-keygen`
// emits, in a directory the test owns, and returns its path: what a
// `deploy_trust local { public_key <path> }` block names.
func KeyFile(tb testing.TB, pub ed25519.PublicKey) string {
	tb.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		tb.Fatal(err)
	}
	path := filepath.Join(tb.TempDir(), "deploy.pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}

// Claims builds a JWT payload as one object: go-jose's builder rejects
// a second .Claims() merge of a map[string]string, so standard and
// custom claims are combined here and passed in one call.
func Claims(issuer, audience string, iat, exp time.Time, custom map[string]string) map[string]any {
	m := map[string]any{
		"aud": audience,
		"iat": jwt.NewNumericDate(iat),
		"exp": jwt.NewNumericDate(exp),
	}
	if issuer != "" {
		m["iss"] = issuer
	}
	for k, v := range custom {
		m[k] = v
	}
	return m
}

// sign serialises m as a JWT under key, the one signer every token
// here goes through.
func sign(tb testing.TB, key jose.SigningKey, m map[string]any) string {
	tb.Helper()
	signer, err := jose.NewSigner(key, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		tb.Fatal(err)
	}
	tok, err := jwt.Signed(signer).Claims(m).Serialize()
	if err != nil {
		tb.Fatal(err)
	}
	return tok
}

// SignEdDSA signs the payload m with priv as a JWT.
func SignEdDSA(tb testing.TB, priv ed25519.PrivateKey, m map[string]any) string {
	tb.Helper()
	return sign(tb, jose.SigningKey{Algorithm: jose.EdDSA, Key: priv}, m)
}

// Mint signs a valid (5-minute) deploy JWT with a local key — the
// test-side equivalent of `deploy-token`.
func Mint(tb testing.TB, priv ed25519.PrivateKey, audience string, claims map[string]string) string {
	tb.Helper()
	now := time.Now()
	return SignEdDSA(tb, priv, Claims("", audience, now, now.Add(5*time.Minute), claims))
}

// MintExpired signs a token whose expiry is an hour in the past — well
// beyond the leeway a verifier allows.
func MintExpired(tb testing.TB, priv ed25519.PrivateKey, audience string, claims map[string]string) string {
	tb.Helper()
	now := time.Now()
	return SignEdDSA(tb, priv, Claims("", audience, now.Add(-2*time.Hour), now.Add(-time.Hour), claims))
}

// Issuer is an OIDC issuer in-process: it serves discovery and JWKS
// over an httptest server and signs tokens with its RSA key.
type Issuer struct {
	URL    string
	Client *http.Client
	Priv   *rsa.PrivateKey
	KID    string
	// DiscoveryDown / JWKSDown make that endpoint answer 503: the
	// issuer is up but the box cannot get what it needs from it.
	DiscoveryDown, JWKSDown atomic.Bool
	// DiscoveredAs, when set, is the issuer the discovery document
	// claims — someone other than the URL it was fetched from.
	DiscoveredAs atomic.Value
}

// NewIssuer starts an issuer, closed when the test ends.
func NewIssuer(tb testing.TB) *Issuer {
	tb.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	iss := &Issuer{Priv: priv, KID: "test-key"}
	pubJWK := jose.JSONWebKey{Key: priv.Public(), KeyID: iss.KID, Algorithm: "RS256", Use: "sig"}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pubJWK}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if iss.DiscoveryDown.Load() {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		issuer := iss.URL
		if as, ok := iss.DiscoveredAs.Load().(string); ok && as != "" {
			issuer = as
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   issuer,
			"jwks_uri": iss.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		if iss.JWKSDown.Load() {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(jwks)
	})
	srv := httptest.NewServer(mux)
	tb.Cleanup(srv.Close)
	iss.URL = srv.URL
	iss.Client = srv.Client()
	return iss
}

// Mint signs a token as this issuer (or with an off-key private key, to
// exercise the unknown-signing-key path).
func (iss *Issuer) Mint(tb testing.TB, priv *rsa.PrivateKey, audience string, claims map[string]string, exp time.Time) string {
	tb.Helper()
	return sign(tb, iss.key(priv), Claims(iss.URL, audience, time.Now(), exp, claims))
}

// MintClaims signs a token as this issuer with arbitrary custom claims,
// so a test can carry a JSON number (not just string claims) in the
// payload.
func (iss *Issuer) MintClaims(tb testing.TB, audience string, exp time.Time, custom map[string]any) string {
	tb.Helper()
	m := Claims(iss.URL, audience, time.Now(), exp, nil)
	for k, v := range custom {
		m[k] = v
	}
	return sign(tb, iss.key(iss.Priv), m)
}

// key is the issuer's signing key under its kid, for priv.
func (iss *Issuer) key(priv *rsa.PrivateKey) jose.SigningKey {
	return jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: priv, KeyID: iss.KID}}
}

// Clock is a clock a test advances by hand, for a limiter's window.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{now: time.Unix(1_700_000_000, 0)} }

// Now is the clock's current reading.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
