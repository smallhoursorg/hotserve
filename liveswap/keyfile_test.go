package liveswap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
	"github.com/smallhoursorg/hotserve/liveswap/deploytrust/trusttest"
)

// TestLocalKeyFileRoundtrip proves the on-disk PEM formats produced by
// `deploy-keygen` load back — the private half through
// loadEd25519PrivateKey here, the public half through a `deploy_trust
// local` source — and interoperate: a token minted with the loaded
// private key is accepted against the loaded public one.
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

	loadedPriv, err := loadEd25519PrivateKey(privPath)
	if err != nil {
		t.Fatalf("load private key: %v", err)
	}
	sources, err := deploytrust.Build([]deploytrust.TrustConfig{{
		Kind: "local", PublicKey: pubPath, Audience: "hotserve", Claims: map[string]string{"repository": "org/app"},
	}}, nil)
	if err != nil {
		t.Fatalf("load public key: %v", err)
	}
	tok := trusttest.Mint(t, loadedPriv, "hotserve", map[string]string{"repository": "org/app"})
	req := httptest.NewRequest(http.MethodGet, "/demo", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	who, ref := deploytrust.NewLimiter(newFakeClock()).Authenticate(req, deploytrust.Verifiers(sources, nil), zap.NewNop(), "", "")
	if ref != nil || who.By != "local:"+pubPath {
		t.Fatalf("round-trip token: who = %+v, refusal = %+v; want accepted under the key file's label", who, ref)
	}
}
