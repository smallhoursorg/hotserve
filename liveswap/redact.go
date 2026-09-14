package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The webhook's responses land in CI logs — readable by everyone with
// read access to the repository, retained, public for a public repo —
// so nothing leaves the box through them without passing this filter.
// hotserve is the right place for it: it built the app's environment
// from env_file, so it knows the secrets by value, and it knows which
// high-entropy strings of its own (versions, paths) are not secrets.
//
// Today a response carries hotserve's own fields and error text; the
// app's own bytes reach one only through a probe error that quotes a
// malformed response line. The filter is here first because the next
// change puts the app's output into responses on purpose.
//
// Four layers, in order:
//
//  1. Known values: every env_file value of secretMinLen or more, in
//     the forms it could appear in — as written, JSON-escaped, base64
//     (standard and URL alphabets, padded and not), hex, URL-escaped —
//     and, for a value that is a URL with a password, the password on
//     its own in the same forms. Replaced with [redacted:KEY]. Inline
//     `env` values are not secrets by policy (the Caddyfile is in a
//     repo) and are not redacted; SOCKET, HOME and the inherited PATH
//     are paths a diagnostic needs. A value equal to something on the
//     safe list (a version, one of the app's own paths) is not a
//     secret either, whatever file it came from.
//  2. A safe list — the versions the status names, the app's dirs —
//     exempt from the heuristics below, so a 40-character git SHA used
//     as a version survives. Each entry is also split the way layer 4
//     tokenises, so a dotted version's generated segment is safe too.
//  3. Shape rules: credentials with a recognisable form (JWTs, bearer
//     tokens, URL userinfo and query strings, provider key prefixes,
//     PEM private keys, password=… assignments).
//  4. Entropy: a token of entropyMinLen or more characters from the
//     base64 or hex alphabet, mixed enough to look generated and with
//     Shannon entropy above the class's threshold, replaced with
//     [masked, N chars]. Never a fingerprint: a hash of a secret is a
//     small leak of it.
//
// Best effort past layer 1, and documented as such in
// DESIGN-threat-model.md: an encoding not in the list, a value under
// secretMinLen, a secret split across lines, all pass.
const (
	secretMinLen  = 8
	entropyMinLen = 20
	// Bits per character. Random base64 is ~6, random hex ~4; English
	// identifiers sit around 3.5–4 and never clear the base64 bar.
	entropyBase64 = 4.5
	entropyHex    = 3.0
)

type redactor struct {
	// secrets holds every form of every known value, longest first so
	// a value that contains another is replaced whole.
	secrets []secretForm
	safe    map[string]bool
}

type secretForm struct{ key, text string }

// newRedactor takes env_file KEY=VALUE pairs and the strings that must
// survive the heuristics. A nil *redactor is usable: layers 3 and 4
// only.
func newRedactor(envFile []string, safe []string) *redactor {
	r := &redactor{safe: make(map[string]bool, len(safe))}
	for _, s := range safe {
		if s == "" {
			continue
		}
		r.safe[s] = true
		for _, tok := range entropyTokenRe.FindAllString(s, -1) {
			r.safe[tok] = true
		}
	}
	seen := map[string]bool{}
	for _, kv := range envFile {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < secretMinLen || r.safe[value] {
			continue
		}
		forms := secretForms(value)
		// A connection URL's password is a secret on its own: a driver
		// prints it without the rest of the URL.
		if pw := urlPassword(value); len(pw) >= secretMinLen {
			forms = append(forms, secretForms(pw)...)
		}
		for _, form := range forms {
			if len(form) < secretMinLen || seen[form] {
				continue
			}
			seen[form] = true
			r.secrets = append(r.secrets, secretForm{key: key, text: form})
		}
	}
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i].text) > len(r.secrets[j].text) })
	return r
}

// urlPassword is the password in a value of the form
// scheme://user:password@host…, or "".
func urlPassword(v string) string {
	if !strings.Contains(v, "://") {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

// secretForms is every encoding of a value the filter recognises.
func secretForms(v string) []string {
	forms := []string{v}
	if j, err := json.Marshal(v); err == nil {
		if s := string(j[1 : len(j)-1]); s != v {
			forms = append(forms, s)
		}
	}
	b := []byte(v)
	forms = append(forms,
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
		hex.EncodeToString(b),
	)
	if q := url.QueryEscape(v); q != v {
		forms = append(forms, q)
	}
	if p := url.PathEscape(v); p != v {
		forms = append(forms, p)
	}
	return forms
}

// shapeRules are the recognisable credential forms. Each replacement
// keeps enough context to read the line and names the rule.
var shapeRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private keys, whole block.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[redacted:private-key]"},
	// JWT: three base64url segments, the header starting {"…
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`), "[redacted:jwt]"},
	// Authorization schemes with a token-shaped credential. The
	// placeholder text in hotserve's own 401 body, "Bearer <jwt>", is
	// not token-shaped and passes.
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{20,}`), "$1 [redacted:token]"},
	// URL userinfo: scheme://user:pass@host.
	{regexp.MustCompile(`\b([a-z][a-z0-9+.-]*://)[^/\s"'@:]+:[^/\s"'@]+@`), "$1[redacted:userinfo]@"},
	// URL query strings: where presigned-URL credentials live. The
	// same rule the box applies to its own logs (redactURL).
	{regexp.MustCompile(`\b([a-z][a-z0-9+.-]*://[^\s"'?]+)\?[^\s"']+`), "$1?[redacted:query]"},
	// Provider prefixes.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), "[redacted:aws-key]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), "[redacted:github-token]"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`), "[redacted:github-token]"},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), "[redacted:slack-token]"},
	{regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}\b`), "[redacted:stripe-key]"},
	// KEY=value, key: value and JSON "key":"value" assignments whose
	// key contains a credential word, whatever surrounds it
	// (DB_PASSWORD, AWS_SECRET_ACCESS_KEY, STRIPE_API_KEY). The value
	// must be 8+ characters that are not a delimiter, not a marker
	// layer 1 left there, and not a path or placeholder (an argv flag
	// such as --api-key=/etc/hotserve/app.key names a file, not a
	// secret).
	{regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:password|passwd|secret|api[_-]?key|access[_-]?token|private[_-]?key)[a-z0-9_-]*)("?\s*[=:]\s*"?)[^\s"',;\[/{][^\s"',;]{7,}`), "$1$2[redacted:credential]"},
}

// entropyTokenRe finds candidates for layer 4. `/` is not in the
// alphabet on purpose: a URL path is a long base64-alphabet run with
// slashes, and splitting there keeps paths readable while a base64
// secret with a slash in it still leaves 20+-character pieces. `.` is
// out for the same reason (hostnames, unit names, dotted versions),
// which is why the safe list is split the same way.
var entropyTokenRe = regexp.MustCompile(`[A-Za-z0-9+=_-]{20,}`)

// redact filters s and reports which env keys' values it found.
func (r *redactor) redact(s string) (string, []string) {
	var keys []string
	if r != nil {
		seen := map[string]bool{}
		for _, sec := range r.secrets {
			if !strings.Contains(s, sec.text) {
				continue
			}
			s = strings.ReplaceAll(s, sec.text, "[redacted:"+sec.key+"]")
			if !seen[sec.key] {
				seen[sec.key] = true
				keys = append(keys, sec.key)
			}
		}
		sort.Strings(keys)
	}
	for _, rule := range shapeRules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	s = entropyTokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		if r != nil && r.safe[tok] {
			return tok
		}
		if looksGenerated(tok) {
			return "[masked, " + strconv.Itoa(len(tok)) + " chars]"
		}
		return tok
	})
	return s, keys
}

// redactJSON filters a marshalled JSON body and, when a known value
// was found, adds a `redacted_env` field naming the keys. Layer 1 is a
// plain substring replacement, so a value made of JSON's own
// punctuation could in principle match across a string's delimiters
// and leave the body unparsable; that body is withheld rather than
// sent, with the keys still reported.
func (r *redactor) redactJSON(raw []byte) string {
	body, keys := r.redact(string(raw))
	if !json.Valid([]byte(body)) {
		body = `{"error":"response withheld: a redacted value overlapped the response's own structure"}`
	}
	if len(keys) == 0 {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil || obj == nil {
		return body // an array or scalar body: nowhere to put the field
	}
	k, err := json.Marshal(keys)
	if err != nil {
		return body
	}
	obj["redacted_env"] = k
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return string(out)
}

// looksGenerated is layer 4's test. Hex needs a digit and clears
// entropyHex; anything else needs a digit, an upper- and a lower-case
// letter (identifiers and words rarely have all three) and clears
// entropyBase64.
func looksGenerated(tok string) bool {
	var digit, upper, lower, other bool
	for _, c := range tok {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			if c >= 'a' {
				lower = true
			} else {
				upper = true
			}
		case c >= 'g' && c <= 'z':
			lower, other = true, true
		case c >= 'G' && c <= 'Z':
			upper, other = true, true
		default:
			other = true
		}
	}
	if !digit {
		return false
	}
	h := shannon(tok)
	if !other { // hex alphabet only
		return h >= entropyHex
	}
	return upper && lower && h >= entropyBase64
}

func shannon(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
