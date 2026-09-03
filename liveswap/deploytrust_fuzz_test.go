// Fuzz target for the deploy-token claim matching: the one place a
// token's identity claims meet the operator's pins. The bytes reaching
// decodeClaims are signature-verified (both verifiers check the
// signature first), but they are still whatever the signer wrote — an
// OIDC provider's claim set, or a local token minted by hand — and #31
// was exactly a claim shape (a round-numbered repository_id) that the
// matcher mishandled. The property asserted is fail-closed matching:
// a pin is satisfied exactly when the claim is present, scalar and
// byte-equal to it, and the decision is rebuilt from the raw JSON
// independently so a future "helpful" coercion in claimScalar cannot
// pass unnoticed. decodeClaims never yielding a float64 is pinned too:
// that is what makes claimScalar's float64 refusal unreachable from
// production.
package liveswap

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
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

		// The oracle: a pin matches exactly when its claim is present,
		// scalar, and renders byte-equal. Neither direction may drift —
		// a match beyond this is a coercion, a refusal within it is
		// fail-closed turned fail-always.
		v, present := got[pinName]
		s, scalar := claimScalar(v)
		if ok := present && scalar && s == pinValue; (merr == nil) != ok {
			t.Fatalf("matchClaims(%q=%q, %s) = %v; present=%t scalar=%t rendering=%q value=%#v",
				pinName, pinValue, raw, merr, present, scalar, s, v)
		}
		if merr == nil {
			assertRawClaimEquals(t, raw, pinName, pinValue)
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

// assertRawClaimEquals rebuilds the match decision from the raw JSON
// without claimScalar: the claim's own bytes must be a JSON string
// whose decoded value is the pin, or a bare number/bool literal whose
// source text is the pin. Anything else that matched is a coercion.
func assertRawClaimEquals(t *testing.T, raw []byte, name, pin string) {
	t.Helper()
	// Same decoder shape as decodeClaims (a Decoder, so trailing bytes
	// after the object are tolerated identically; last duplicate wins
	// identically).
	var top map[string]json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&top); err != nil {
		t.Fatalf("raw re-decode of %s failed after decodeClaims succeeded: %v", raw, err)
	}
	rv := bytes.TrimSpace(top[name])
	if len(rv) == 0 {
		t.Fatalf("claim %q matched but has no raw value in %s", name, raw)
	}
	switch rv[0] {
	case '"':
		var s string
		if err := json.Unmarshal(rv, &s); err != nil {
			t.Fatalf("raw string claim %s: %v", rv, err)
		}
		if s != pin {
			t.Fatalf("pin %q matched string claim %s", pin, rv)
		}
	case '[', '{', 'n':
		t.Fatalf("pin %q matched composite/null claim %s", pin, rv)
	default:
		// number, true, false: the source text is the canonical form.
		if string(rv) != pin {
			t.Fatalf("pin %q matched literal claim %s (only its exact source text may match)", pin, rv)
		}
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
