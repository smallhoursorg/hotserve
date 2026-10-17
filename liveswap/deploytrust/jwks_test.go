package deploytrust

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust/trusttest"
)

// clocked is an OIDC source for iss whose key set's refetch interval
// runs on c.
func clocked(iss *trusttest.Issuer, c Clock) *oidcVerifier {
	return &oidcVerifier{issuer: iss.URL, audience: "hotserve", claims: map[string]string{"sub": "ci"}, client: iss.Client, clock: c}
}

// ciToken is a valid token from iss for a clocked source, under the
// issuer's current key.
func ciToken(t *testing.T, iss *trusttest.Issuer) string {
	t.Helper()
	return iss.Mint(t, iss.Priv, "hotserve", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
}

// refusedPlainly fails the test unless err is a refusal of the token's
// own, not an unavailable.
func refusedPlainly(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || errors.As(err, &unavailable{}) {
		t.Fatalf("%s: err = %v, want a plain refusal", what, err)
	}
}

// fetches fails the test unless iss's JWKS endpoint has answered want
// requests.
func fetches(t *testing.T, iss *trusttest.Issuer, want int64, what string) {
	t.Helper()
	if n := iss.JWKSFetches.Load(); n != want {
		t.Fatalf("%s: %d JWKS fetches, want %d", what, n, want)
	}
}

// A bad signature under a kid the key set holds is refused on the
// spot, however long since the last fetch: a forged token naming the
// issuer's real kid costs the box no outbound request.
func TestKeySetNeverFetchesForAKidItHolds(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	c := trusttest.NewClock()
	v := clocked(iss, c)
	ctx := context.Background()
	if _, err := v.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	fetches(t, iss, 1, "first token")
	forger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		c.Advance(KeyRefetchInterval)
		_, err := v.verify(ctx, iss.Mint(t, forger, "hotserve", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute)))
		refusedPlainly(t, err, "bad signature under a held kid")
		if !strings.Contains(err.Error(), `does not verify under the issuer's key "`+iss.KID+`"`) {
			t.Fatalf("bad signature under a held kid: reason = %q, want it to name the key", err)
		}
	}
	fetches(t, iss, 1, "bad signatures under a held kid")
}

// A token whose kid the key set holds no key for fetches, but at most
// once per KeyRefetchInterval: the second within it is refused against
// what the first fetched.
func TestKeySetFetchesForAnUnknownKidOncePerInterval(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	c := trusttest.NewClock()
	v := clocked(iss, c)
	ctx := context.Background()
	forger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := func(kid string) error {
		_, err := v.verify(ctx, iss.MintKID(t, forger, kid, "hotserve", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute)))
		return err
	}
	refusedPlainly(t, forged("forged-1"), "first unknown kid")
	fetches(t, iss, 1, "first unknown kid")
	refusedPlainly(t, forged("forged-2"), "second unknown kid, same instant")
	c.Advance(KeyRefetchInterval - time.Second)
	refusedPlainly(t, forged("forged-3"), "unknown kid a second before the interval")
	fetches(t, iss, 1, "unknown kids within the interval")
	c.Advance(time.Second)
	err = forged("forged-4")
	refusedPlainly(t, err, "unknown kid at the interval")
	if !strings.Contains(err.Error(), `kid "forged-4"`) {
		t.Fatalf("unknown kid: reason = %q, want it to name the kid", err)
	}
	fetches(t, iss, 2, "unknown kid at the interval")
}

// A key rotation is picked up by the first token under the new key
// that arrives KeyRefetchInterval after the last fetch; one sooner is
// refused, and its retry deploys.
func TestKeySetPicksUpARotation(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	c := trusttest.NewClock()
	v := clocked(iss, c)
	ctx := context.Background()
	if _, err := v.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	iss.Rotate(t)
	rotated := ciToken(t, iss)
	_, err := v.verify(ctx, rotated)
	refusedPlainly(t, err, "new key within the interval")
	fetches(t, iss, 1, "new key within the interval")
	c.Advance(KeyRefetchInterval)
	if _, err := v.verify(ctx, rotated); err != nil {
		t.Fatalf("new key past the interval: refused: %v", err)
	}
	fetches(t, iss, 2, "new key past the interval")
}

// heldTransport holds every request until hold is closed, counting
// them.
type heldTransport struct {
	base http.RoundTripper
	hold chan struct{}
	n    atomic.Int64
}

func (h *heldTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h.n.Add(1)
	<-h.hold
	return h.base.RoundTrip(r)
}

// Callers that need a fetch while one is under way wait on it: many
// at once make one fetch, and every one of them gets its outcome. One
// that goes away while waiting gets its own error, and the fetch goes
// on for the rest.
func TestKeySetOneFetchForConcurrentCallers(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	held := &heldTransport{base: iss.Client.Transport, hold: make(chan struct{})}
	ks := &keySet{url: iss.URL + "/jwks", client: &http.Client{Transport: held}, clock: trusttest.NewClock()}
	tok := ciToken(t, iss)

	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			_, err := ks.VerifySignature(context.Background(), tok)
			errs <- err
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for held.n.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no fetch began")
		}
		time.Sleep(time.Millisecond)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ks.VerifySignature(gone, tok); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller gone while the fetch is under way: err = %v, want its own context's", err)
	}
	close(held.hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a caller waiting on the fetch was refused: %v", err)
		}
	}
	if n := held.n.Load(); n != 1 {
		t.Fatalf("%d concurrent callers made %d fetches, want one", callers, n)
	}
}

// parseKeySet leaves out what it cannot verify with — a key type
// go-jose does not support, a key missing a member, a key for another
// algorithm — and keeps the rest of the set; a body that is not a key
// set is an error.
func TestParseKeySetLeavesOutKeysItCannotUse(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := func(alg, kid string) string {
		b, err := json.Marshal(jose.JSONWebKey{Key: priv.Public(), KeyID: kid, Algorithm: alg})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	body := `{"keys":[` + strings.Join([]string{
		`{"kty":"OKP","crv":"Ed448","x":"AAAA","kid":"ed448"}`,
		`{"kty":"RSA","kid":"no-modulus"}`,
		jwk("RSA-OAEP", "encryption"),
		jwk("RS256", "signing"),
		jwk("", "no-alg"),
	}, ",") + `]}`
	keys, err := parseKeySet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, k := range keys {
		kids = append(kids, k.KeyID)
	}
	if got := strings.Join(kids, " "); got != "signing no-alg" {
		t.Fatalf("kept %q, want the signing key and the one naming no algorithm", got)
	}
	if _, err := parseKeySet([]byte("<html>")); err == nil {
		t.Fatal("a body that is not a key set must be an error")
	}
}
