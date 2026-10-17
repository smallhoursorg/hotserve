package deploytrust

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// KeyRefetchInterval is the most often one OIDC source fetches its
// issuer's keys. It fetches only for a token whose kid it holds no key
// for, so a key rotation reaches it with the first token under the new
// key that arrives this long after the last fetch; one that arrives
// sooner is refused, and the deploy's retry goes through.
const KeyRefetchInterval = time.Minute

// keyFetchTimeout bounds one fetch of an issuer's keys. The fetch is
// every waiting caller's, not the one that began it, so no caller's
// context ends it.
const keyFetchTimeout = 30 * time.Second

// signingAlgs are the algorithms a token may be signed with: go-oidc's
// supported list, every one asymmetric. An issuer's advertised
// algorithms are narrowed to these (oidcVerifier.ensure), and a key
// that names another is left out of its set (parseKeySet).
var signingAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// keySet is the oidc.KeySet an OIDC source checks signatures with, in
// place of go-oidc's RemoteKeySet, which fetches the issuer's keys
// again for every signature that does not verify, its kid known or
// not: a forged token naming the issuer cost the box an outbound round
// trip each. Here a signature that does not verify under the key its
// kid names is refused on the spot; only a token whose kid the set
// holds no key for fetches, at most once per KeyRefetchInterval, one
// fetch at a time, with every caller that needs it waiting on that
// one. Until the interval has passed, the last fetch's outcome stands:
// its keys, or, when it failed, its error — so during an outage a
// token the box holds no key for is still the outage's (unavailable),
// not a refusal of its own.
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

// VerifySignature is the payload of the token raw when a key of the
// issuer's verifies its signature. The IDTokenVerifier calling it has
// already refused an algorithm the source does not accept and a token
// with other than one signature.
func (ks *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, signingAlgs)
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, fmt.Errorf("%d signatures, want one", len(jws.Signatures))
	}
	kid := jws.Signatures[0].Header.KeyID
	ks.mu.Lock()
	keys := ks.keys
	ks.mu.Unlock()
	payload, held := verifyWith(jws, kid, keys)
	if payload == nil && !held {
		if keys, err = ks.refresh(ctx); err != nil {
			return nil, err
		}
		payload, held = verifyWith(jws, kid, keys)
	}
	switch {
	case payload != nil:
		return payload, nil
	case held:
		return nil, fmt.Errorf("signature does not verify under the issuer's key %q", kid)
	}
	return nil, fmt.Errorf("the issuer's keys hold none that verifies it (kid %q)", kid)
}

// verifyWith is the payload of the first of keys that verifies jws —
// one under the token's kid, or any, for a token that names none, as
// go-oidc matches them — and whether the token names a kid that one
// of keys carries.
func verifyWith(jws *jose.JSONWebSignature, kid string, keys []jose.JSONWebKey) (payload []byte, held bool) {
	for _, key := range keys {
		if kid != "" && key.KeyID != kid {
			continue
		}
		held = kid != ""
		if p, err := jws.Verify(&key); err == nil {
			return p, held
		}
	}
	return nil, held
}

// refresh is the issuer's keys as the last fetch left them, after
// beginning a fetch when none is under way and KeyRefetchInterval has
// passed since the last began, or none ever has. A caller waits on the
// fetch under way, whoever began it, until it ends or the caller's ctx
// does.
func (ks *keySet) refresh(ctx context.Context) ([]jose.JSONWebKey, error) {
	ks.mu.Lock()
	done := ks.inflight
	if now := ks.clock.Now(); done == nil && (ks.fetched.IsZero() || now.Sub(ks.fetched) >= KeyRefetchInterval) {
		done = make(chan struct{})
		ks.inflight, ks.fetched = done, now
		go ks.fetch(done)
	}
	ks.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, fmt.Errorf("fetching keys: %w", ctx.Err())
		}
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.err != nil {
		return nil, ks.err
	}
	return ks.keys, nil
}

// fetch records one fetch's outcome — the keys, or the error with the
// keys before it kept — and only then ends the wait on done, so a
// caller woken by done reads this fetch's outcome. The error's text
// begins "fetching keys", which keyFetchFailed matches.
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
	ctx, cancel := context.WithTimeout(context.Background(), keyFetchTimeout)
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
