package liveswap

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
	"github.com/smallhoursorg/hotserve/liveswap/deploytrust/trusttest"
)

// Deploy-auth test keys, generated once per test binary. appTest* backs
// the per-app trust the standard rig configures; globalTest* backs the
// global (unknown-app) trust. A token minted with one key is not valid
// under the other — which is what the per-app-isolation and
// name-non-enumeration tests exercise.
var (
	appTestPriv, appTestPub       = trusttest.GenerateKey()
	globalTestPriv, globalTestPub = trusttest.GenerateKey()
)

// appToken / globalToken mint tokens the standard rig accepts for the
// per-app and global trust respectively.
func appToken(t *testing.T) string    { return trusttest.Mint(t, appTestPriv, "demo", nil) }
func globalToken(t *testing.T) string { return trusttest.Mint(t, globalTestPriv, "global", nil) }

// localSource is a local trust source for a test public key, built as
// an operator's is — from the key's PKIX file — so its label, which
// deployed_by and the journal carry, is local:<path>.
func localSource(t *testing.T, pub ed25519.PublicKey, audience string) deploytrust.Source {
	t.Helper()
	return mustBuild(t, []deploytrust.TrustConfig{{Kind: "local", PublicKey: trusttest.KeyFile(t, pub), Audience: audience}})[0]
}

// oidcSource is an OIDC trust source for a test issuer, pinned to the
// subject ci.
func oidcSource(t *testing.T, issuer string) deploytrust.Source {
	t.Helper()
	return mustBuild(t, []deploytrust.TrustConfig{{Kind: "oidc", Issuer: issuer, Audience: "hotserve", Subject: "ci"}})[0]
}

// mustBuild is deploytrust.Build for a config the test knows is valid.
func mustBuild(t *testing.T, configs []deploytrust.TrustConfig) []deploytrust.Source {
	t.Helper()
	sources, err := deploytrust.Build(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

// githubTrust is an I/O-free config-level trust source for config tests
// (Build validates OIDC presets without touching the network or the
// filesystem; verifier construction is lazy).
func githubTrust() []deploytrust.TrustConfig {
	return []deploytrust.TrustConfig{{Kind: "github", Audience: "hotserve", Claims: map[string]string{"repository": "org/blog"}}}
}

// eventually retries check for a second, for a condition another
// goroutine establishes.
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
