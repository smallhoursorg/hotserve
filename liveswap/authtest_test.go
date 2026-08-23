package liveswap

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Deploy-auth test keys, generated once per test binary. appTest* backs
// the per-app trust the standard rig configures; globalTest* backs the
// global (unknown-app) trust. A token minted with one key is not valid
// under the other — which is what the per-app-isolation and
// name-non-enumeration tests exercise.
var (
	appTestPriv, appTestPub       = mustGenTestKey()
	globalTestPriv, globalTestPub = mustGenTestKey()
)

func mustGenTestKey() (ed25519.PrivateKey, ed25519.PublicKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv, pub
}

// claimMap builds the JWT payload as a single object: go-jose's builder
// rejects a second .Claims() merge of a map[string]string, so standard
// and custom claims are combined here and passed in one call.
func claimMap(issuer, audience string, iat, exp time.Time, custom map[string]string) map[string]any {
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

func signEdDSA(t *testing.T, priv ed25519.PrivateKey, m map[string]any) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(sig).Claims(m).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// mintTestToken signs a valid (5-minute) deploy JWT — the test-side
// equivalent of `deploy-token`.
func mintTestToken(t *testing.T, priv ed25519.PrivateKey, audience string, claims map[string]string) string {
	t.Helper()
	now := time.Now()
	return signEdDSA(t, priv, claimMap("", audience, now, now.Add(5*time.Minute), claims))
}

// mintExpiredToken signs a token whose expiry is an hour in the past —
// well beyond oidcLeeway.
func mintExpiredToken(t *testing.T, priv ed25519.PrivateKey, audience string, claims map[string]string) string {
	t.Helper()
	now := time.Now()
	return signEdDSA(t, priv, claimMap("", audience, now.Add(-2*time.Hour), now.Add(-time.Hour), claims))
}

// appToken / globalToken mint tokens the standard rig accepts for the
// per-app and global trust respectively.
func appToken(t *testing.T) string    { return mintTestToken(t, appTestPriv, "demo", nil) }
func globalToken(t *testing.T) string { return mintTestToken(t, globalTestPriv, "global", nil) }

// localTrust builds a local trust source for a test public key.
func localTrust(pub ed25519.PublicKey, audience string) trustSource {
	return trustSource{kind: "local", audience: audience, pubKey: pub, keyPath: "test-key"}
}

// githubTrust is an I/O-free config-level trust source for config tests
// (buildTrust validates OIDC presets without touching the network or
// the filesystem; verifier construction is lazy).
func githubTrust() []TrustConfig {
	return []TrustConfig{{Kind: "github", Audience: "hotserve", Claims: map[string]string{"repository": "org/blog"}}}
}
