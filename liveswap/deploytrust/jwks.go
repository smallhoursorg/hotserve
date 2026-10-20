package deploytrust

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// RefetchInterval is the most often a token makes the box try an
// issuer's discovery, until one succeeds, or fetch its keys. Both run
// in the background, and no token waits on either (#306): a token's
// verdict comes from what the process holds for the issuer
// (issuerState, shared by every source that trusts it), in the time of
// a local check whether or not its app exists. A start or a reload
// (Warm) runs the discovery and fetches the keys whatever this
// interval says; after that the keys are fetched only for a token
// whose kid the set holds no key for: so a key rotation — a new key
// under a new kid — is refused for the first token under the new key,
// which begins the fetch when this long has passed since the last,
// and the deploy's retry once the fetch has ended goes through (the
// templates' scripts send a refused Actions token once more, 3s
// later). A key replaced under the same kid does not reach it: its
// tokens fail under the held key, which never fetches, until a fetch
// for another kid or a reload.
const RefetchInterval = time.Minute

// fetchTimeout bounds one discovery or one fetch of an issuer's keys.
// No caller waits on either, so no caller's context ends it.
const fetchTimeout = 30 * time.Second

// signingAlgs are the algorithms a token may be signed with: go-oidc's
// supported list, every one asymmetric. An issuer's advertised
// algorithms are narrowed to these (oidcVerifier.provide), and a key
// that names another is left out of its set (parseKeySet).
var signingAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// keySet is the oidc.KeySet an issuer's sources check signatures with
// (one per issuerState), in place of go-oidc's RemoteKeySet, which fetches the issuer's keys
// again for every signature that does not verify, its kid known or
// not, and waits on the fetch: a forged token naming the issuer cost
// the box an outbound round trip each, and its timing said the app
// exists. Here a signature is judged against the keys the set holds,
// on the spot. A token whose kid the set holds no key for begins a
// fetch in the background, at most once per RefetchInterval and one at
// a time, and is refused without waiting on it. Until the interval has
// passed, the last fetch's outcome stands: its keys, or, when it
// failed, its error — so during an outage a token the box holds no key
// for is still the outage's (unavailable), not a refusal of its own.
type keySet struct {
	url    string
	client *http.Client
	clock  Clock

	mu       sync.Mutex
	keys     []jose.JSONWebKey // replaced whole by a fetch, never changed in place
	fetched  time.Time         // when the last fetch began; zero before the first
	err      error             // the last fetch's error; nil when it succeeded
	inflight chan struct{}     // closed when the fetch under way ends; nil when none is
}

// VerifySignature is the payload of the token raw when a key the set
// holds verifies its signature. The IDTokenVerifier calling it has
// already refused an algorithm the source does not accept and a token
// with other than one signature. It never waits on the issuer, so the
// context is not used.
func (ks *keySet) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, signingAlgs)
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, fmt.Errorf("%d signatures, want one", len(jws.Signatures))
	}
	header := jws.Signatures[0].Header
	kid, alg := header.KeyID, jose.SignatureAlgorithm(header.Algorithm)
	ks.mu.Lock()
	keys := ks.keys
	ks.mu.Unlock()
	if kid == "" {
		// Without a kid, go-oidc tries every key the set holds: as many
		// signature checks as the issuer publishes keys, which no floor
		// is sized for (RefusalFloor). One check is: an issuer with one
		// key for the algorithm needs no kid. GitHub's and GitLab's
		// tokens name one. No fetch: a set that grows is no reason to.
		var only []jose.JSONWebKey
		for _, key := range keys {
			if fits(key, alg) {
				only = append(only, key)
			}
		}
		if len(only) != 1 {
			return nil, fmt.Errorf("the token names no key (kid), and the issuer's keys hold %d for %s; a source checks one key per token", len(only), alg)
		}
		if p, err := jws.Verify(&only[0]); err == nil {
			return p, nil
		}
		return nil, errors.New("signature does not match the issuer's one key, and the token names no kid")
	}
	payload, held := verifyWith(jws, kid, alg, keys)
	if payload != nil {
		return payload, nil
	}
	if held {
		// The one refusal no later token fetches its way out of: a held
		// kid never fetches, so an issuer that replaced the key and kept
		// its kid is picked up only by a reload (a new key set). Said
		// here, in the journal's refused field, in the README's words.
		return nil, fmt.Errorf("signature does not match the key held for kid %q; if the issuer replaced that key, systemctl reload hotserve", kid)
	}
	fetching, next, err := ks.refresh(false)
	if err != nil {
		return nil, err
	}
	if fetching {
		return nil, fmt.Errorf("the issuer's keys hold none that verifies it (kid %q); fetching them, so a retry once that ends may go through", kid)
	}
	// Fetched within the interval: say so and when the next may be, so
	// a refused rotation is not read as a trust mismatch.
	return nil, fmt.Errorf("the issuer's keys hold none that verifies it (kid %q); they were fetched under a minute ago, so a token from %s on fetches them again, as does systemctl reload hotserve now", kid, next.UTC().Format(time.RFC3339))
}

// verifyWith is the payload of jws when the first of keys under kid
// that fits its algorithm verifies it, and whether one does fit: one
// signature check per token, however many keys the issuer publishes
// under the kid. A set may hold keys of other types, or for
// encryption, under the same kid (RFC 7517 §4.5); they are passed
// over, not checked.
func verifyWith(jws *jose.JSONWebSignature, kid string, alg jose.SignatureAlgorithm, keys []jose.JSONWebKey) (payload []byte, held bool) {
	for _, key := range keys {
		if key.KeyID != kid || !fits(key, alg) {
			continue
		}
		if p, err := jws.Verify(&key); err == nil {
			return p, true
		}
		return nil, true
	}
	return nil, false
}

// fits says whether key can verify a signature under alg: a signing key
// (no `use`, or `sig`), naming alg or no algorithm, of alg's key type.
func fits(key jose.JSONWebKey, alg jose.SignatureAlgorithm) bool {
	if (key.Use != "" && key.Use != "sig") || (key.Algorithm != "" && jose.SignatureAlgorithm(key.Algorithm) != alg) {
		return false
	}
	switch k := key.Key.(type) {
	case *rsa.PublicKey:
		return alg == jose.RS256 || alg == jose.RS384 || alg == jose.RS512 || alg == jose.PS256 || alg == jose.PS384 || alg == jose.PS512
	case *ecdsa.PublicKey:
		return (alg == jose.ES256 && k.Curve == elliptic.P256()) || (alg == jose.ES384 && k.Curve == elliptic.P384()) || (alg == jose.ES512 && k.Curve == elliptic.P521())
	case ed25519.PublicKey:
		return alg == jose.EdDSA
	}
	return false
}

// refresh begins a fetch when none is under way and either force (a
// discovery's, at start or a reload) or RefetchInterval has passed
// since the last began, or none ever has, and returns at once: the
// last fetch's error when it failed, else whether a fetch is under
// way, whose keys a retry may find, and when, if none is, a token may
// begin the next.
func (ks *keySet) refresh(force bool) (fetching bool, next time.Time, err error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if now := ks.clock.Now(); ks.inflight == nil && (force || ks.fetched.IsZero() || now.Sub(ks.fetched) >= RefetchInterval) {
		ks.inflight, ks.fetched = make(chan struct{}), now
		go ks.fetch(ks.inflight)
	}
	if ks.err != nil {
		return false, time.Time{}, ks.err
	}
	return ks.inflight != nil, ks.fetched.Add(RefetchInterval), nil
}

// held is the keys the set holds now.
func (ks *keySet) held() []jose.JSONWebKey {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.keys
}

// underway is the fetch under way's done channel, nil when none is:
// for Prime, which waits on it.
func (ks *keySet) underway() chan struct{} {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.inflight
}

// fetch records one fetch's outcome — the keys, or the error with the
// keys before it kept — and only then closes done, so whoever is woken
// by it reads this fetch's outcome. The error's text begins "fetching
// keys", which keyFetchFailed matches.
func (ks *keySet) fetch(done chan struct{}) {
	keys, err := ks.get()
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err != nil {
		ks.err = fmt.Errorf("fetching keys: %w", err)
	} else {
		ks.keys, ks.err = keys, nil
	}
	ks.inflight = nil
	close(done)
}

// get reads the issuer's JWK Set from its jwks_uri, on the source's
// JWKS client (https only unless allow_insecure_http).
func (ks *keySet) get() ([]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ks.url, nil)
	if err != nil {
		return nil, err
	}
	// A fetch means a kid is missing; a cache between the box and the
	// issuer would answer with the set that lacks it.
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := ks.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", ks.url, resp.Status)
	}
	return parseKeySet(body)
}

// parseKeySet reads a JWK Set, leaving out each key go-jose cannot
// read — a key type or curve it does not support, a member missing —
// or that names an algorithm other than signingAlgs, as RFC 7517 §5
// asks and go-oidc does: such a key verifies nothing here, and the
// rest of the set still does.
func parseKeySet(body []byte) ([]jose.JSONWebKey, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("decoding the key set: %w", err)
	}
	keys := make([]jose.JSONWebKey, 0, len(set.Keys))
	for _, raw := range set.Keys {
		var key jose.JSONWebKey
		if key.UnmarshalJSON(raw) != nil {
			continue
		}
		if key.Algorithm != "" && !slices.Contains(signingAlgs, jose.SignatureAlgorithm(key.Algorithm)) {
			continue
		}
		keys = append(keys, key)
	}
	return keys, nil
}
