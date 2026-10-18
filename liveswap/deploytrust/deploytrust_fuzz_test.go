// Fuzz target for the deploy-token claim matching: the one place a
// token's identity claims meet the operator's pins. The bytes reaching
// decodeClaims are signature-verified (both verifiers check the
// signature first), but they are still whatever the signer wrote — an
// OIDC provider's claim set, or a local token minted by hand — and #31
// was exactly a claim shape (a round-numbered repository_id) that the
// matcher mishandled. The property asserted is fail-closed matching:
// a pin is satisfied exactly when the claim is present, scalar and
// byte-equal to it — decided from the raw JSON alone, so that neither
// a "helpful" coercion in claimScalar nor a refusal of a valid scalar
// can pass unnoticed. decodeClaims never yielding a float64 is pinned too:
// that is what makes claimScalar's float64 refusal unreachable from
// production.
package deploytrust

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust/trusttest"
)

func FuzzMatchClaims(f *testing.F) {
	for _, s := range [][3]string{
		// The #31 shapes: a round number and one past 2^53, as number
		// and as string, plus the renderings a float path would produce.
		{`{"repository_id":100000000}`, "repository_id", "100000000"},
		{`{"repository_id":1e8}`, "repository_id", "100000000"},
		{`{"repository_id":1E8}`, "repository_id", "100000000"},
		{`{"repository_id":100000000.0}`, "repository_id", "100000000"},
		{`{"repository_id":"100000000"}`, "repository_id", "100000000"},
		{`{"repository_id":9007199254740993}`, "repository_id", "9007199254740993"},
		{`{"repository_id":9007199254740992}`, "repository_id", "9007199254740993"},
		{`{"repository_id":-0}`, "repository_id", "0"},
		{`{"repository_id":99}`, "repository_id", "100000000"},
		// Composite and null claims must never satisfy a string pin,
		// even one spelled like their %v rendering.
		{`{"groups":["admin"]}`, "groups", "[admin]"},
		{`{"groups":{"a":"admin"}}`, "groups", "map[a:admin]"},
		{`{"sub":null}`, "sub", "<nil>"},
		{`{"sub":null}`, "sub", ""},
		{`{"ok":true}`, "ok", "true"},
		{`{"ok":true}`, "ok", "1"},
		// Absent pin, wrong case, duplicate key (last wins), nesting.
		{`{"sub":"x"}`, "repository_id", "x"},
		{`{"Sub":"x"}`, "sub", "x"},
		{`{"sub":"a","sub":"b"}`, "sub", "a"},
		{`{"sub":{"sub":"x"}}`, "sub", "x"},
		// Escapes decode before comparison: "\u0078" is "x" and must
		// match a pin "x"; an embedded NUL is part of the value.
		{`{"sub":"\u0078"}`, "sub", "x"},
		{`{"sub":"x\u0000"}`, "sub", "x"},
		// Not an object / not JSON / trailing data.
		{`[{"sub":"x"}]`, "sub", "x"},
		{`null`, "sub", "x"},
		{`{"sub":"x"} trailing`, "sub", "x"},
		{`{"sub":"x"`, "sub", "x"},
		{``, "sub", "x"},
	} {
		f.Add([]byte(s[0]), s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, raw []byte, pinName, pinValue string) {
		got, err := decodeClaims(raw)
		if err != nil {
			return // refusing to decode is always the safe outcome
		}
		// UseNumber is the invariant that fixed #31: no float64 may
		// exist anywhere in a decoded claim set, or %v-style rendering
		// can creep back in.
		assertNoFloat(t, "", got)

		merr := matchClaims(map[string]string{pinName: pinValue}, got)

		// The oracle is derived from the raw JSON alone — not from
		// claimScalar, which is under test — so neither direction can
		// drift unseen: a match the raw bytes do not justify is a
		// coercion, a refusal they do justify is fail-closed turned
		// fail-always.
		want := rawClaimMatches(t, raw, pinName, pinValue)
		if (merr == nil) != want {
			t.Fatalf("matchClaims(%q=%q, %s) = %v, raw JSON says match=%t (decoded value %#v)",
				pinName, pinValue, raw, merr, want, got[pinName])
		}

		// No pins: nothing to fail. Any absent pin: always a failure,
		// whatever else matched.
		if err := matchClaims(nil, got); err != nil {
			t.Fatalf("matchClaims with no pins failed: %v", err)
		}
		absent := "absent"
		for {
			if _, ok := got[absent]; !ok {
				break
			}
			absent += "_"
		}
		if err := matchClaims(map[string]string{pinName: pinValue, absent: ""}, got); err == nil {
			t.Fatalf("matchClaims matched with pin %q absent from %s", absent, raw)
		}
	})
}

// FuzzCaddyStripsKey holds caddyStripsKey to Caddy's real
// RemoveMetaFields: the strip changes a marshalled claims map exactly
// when its key is one caddyStripsKey refuses, whatever the value. A key
// the strip touches but the parser let through is a constraint dropped
// unseen; one refused that the strip leaves alone is a claim that could
// have been pinned. The value is fuzzed too, so a value that triggers
// the strip would show the parser checking the wrong thing.
func FuzzCaddyStripsKey(f *testing.F) {
	for _, s := range [][2]string{
		{"@id", "v"}, {`x"@id`, "v"}, {`"@id`, "v"}, {"@idx", "v"}, {"x@id", "v"},
		{"@ID", "v"}, {`@id"`, "v"}, {"sub", "@id"}, {"sub", `"@id":"x"`}, {"sub", `x"@id`},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, key, value string) {
		raw, err := json.Marshal(TrustConfig{Claims: map[string]string{key: value}})
		if err != nil {
			t.Fatal(err)
		}
		if stripped := !bytes.Equal(caddy.RemoveMetaFields(raw), raw); stripped != caddyStripsKey(key) {
			t.Fatalf("claim %q=%q in %s: the strip changes it %t, caddyStripsKey says %t", key, value, raw, stripped, caddyStripsKey(key))
		}
	})
}

// FuzzLocalTokenLifetime holds max_token_ttl against whatever iat and
// exp a local token's signer writes — integers, fractions, exponents,
// values at go-jose's ±2^62-second bound: a token the local verifier
// accepts carries both, and its exp - iat, read from the raw JSON
// numbers, is under the bound plus the two seconds go-jose's
// truncation of each to whole seconds can hide. The verifier's clock
// is fixed, so a seed can be accepted; the bound is any positive
// duration, the largest included, where exp - iat would saturate.
func FuzzLocalTokenLifetime(f *testing.F) {
	const day = int64(24 * time.Hour)
	for _, s := range []struct {
		iat, exp string
		maxNs    int64
	}{
		// trusttest.NewClock's instant is 1700000000.
		{"1700000000", "1700086400", day}, // exactly the bound: accepted
		{"1700000000", "1700086401", day}, // a second over
		{"1700000000", "2015360000", day}, // ten years
		{"1.7e9", "1.7000864e9", day},
		{"1699999999.9", "1700086400.9", day},
		{"1700000000", "1700086400.99", day},
		{"-0", "1700000000", day},
		{"-4611686018427387903", "4611686018427387903", day},
		{"", "1700000060", day}, // no iat
		{"1700000000", "1700000060", int64(time.Second)},
		{"1700000000", "10900000000", math.MaxInt64}, // inside the largest bound (~292 years)
		{"1700000000", "32503680000", math.MaxInt64}, // the year 3000: past it
	} {
		f.Add(s.iat, s.exp, s.maxNs)
	}
	priv, pub := trusttest.GenerateKey()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: priv}, nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, iat, exp string, maxNs int64) {
		if !jsonNumber(exp) || (iat != "" && !jsonNumber(iat)) {
			return
		}
		limit := max(1, time.Duration(maxNs&math.MaxInt64))
		payload := `{"aud":"a","exp":` + exp
		if iat != "" {
			payload += `,"iat":` + iat
		}
		payload += "}"
		jws, err := signer.Sign([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		tok, err := jws.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		v := &localVerifier{audience: "a", pub: pub, maxTTL: limit, clock: trusttest.NewClock()}
		if _, err := v.verify(context.Background(), tok); err != nil {
			return // refusing is always within the bound
		}
		if iat == "" {
			t.Fatalf("accepted %s, which has no iat", payload)
		}
		life := new(big.Float).Sub(rawNumber(t, exp), rawNumber(t, iat))
		bound := new(big.Float).Quo(new(big.Float).SetInt64(int64(limit)), big.NewFloat(float64(time.Second)))
		bound.Add(bound, big.NewFloat(2))
		if life.Cmp(bound) >= 0 {
			t.Fatalf("accepted %s: exp - iat is %s s, over max_token_ttl %v", payload, life.Text('f', 3), limit)
		}
	})
}

// jsonNumber reports whether s is one JSON number literal.
func jsonNumber(s string) bool {
	return s != "" && (s[0] == '-' || (s[0] >= '0' && s[0] <= '9')) && json.Valid([]byte(s))
}

// rawNumber is a JSON number literal's exact value.
func rawNumber(t *testing.T, s string) *big.Float {
	t.Helper()
	n, _, err := big.ParseFloat(strings.TrimSpace(s), 10, 256, big.ToNearestEven)
	if err != nil {
		t.Fatalf("number %q: %v", s, err)
	}
	return n
}

// rawClaimMatches decides, from the claim's own bytes and without
// claimScalar, whether a pin should match: the claim must be present
// and be either a JSON string whose decoded value is the pin, or a
// bare number/bool literal whose source text is the pin. Composite and
// null claims never match; nothing else is coerced.
func rawClaimMatches(t *testing.T, raw []byte, name, pin string) bool {
	t.Helper()
	// Same decoder shape as decodeClaims (a Decoder, so trailing bytes
	// after the object are tolerated identically; last duplicate wins
	// identically).
	var top map[string]json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&top); err != nil {
		t.Fatalf("raw re-decode of %s failed after decodeClaims succeeded: %v", raw, err)
	}
	rv, ok := top[name]
	if !ok {
		return false
	}
	rv = bytes.TrimSpace(rv)
	if len(rv) == 0 {
		t.Fatalf("claim %q present in %s with no raw value", name, raw)
	}
	switch rv[0] {
	case '"':
		var s string
		if err := json.Unmarshal(rv, &s); err != nil {
			t.Fatalf("raw string claim %s: %v", rv, err)
		}
		return s == pin
	case '[', '{', 'n':
		return false
	default:
		// number, true, false: the source text is the canonical form
		// (json.Number carries the literal verbatim).
		return string(rv) == pin
	}
}

func assertNoFloat(t *testing.T, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case float64:
		t.Fatalf("decodeClaims produced a float64 at %q: %v", path, x)
	case map[string]any:
		for k, e := range x {
			assertNoFloat(t, path+"/"+k, e)
		}
	case []any:
		for i, e := range x {
			assertNoFloat(t, path+"/"+strconv.Itoa(i), e)
		}
	}
}
