package liveswap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Deploy authentication is asymmetric: the box stores only PUBLIC
// material (an OIDC issuer's JWKS, or a local public key), never a
// symmetric secret an app could read out of the supervisor's
// environment. A deploy request carries `Authorization: Bearer <JWT>`;
// it is authorized if ANY configured trust source verifies the token's
// signature and standard claims AND every claim constraint matches
// (AND within a source, OR across sources — so "OIDC primary, local
// fallback" is simply two sources).

// githubIssuer is the GitHub Actions OIDC issuer (the `github` preset).
const githubIssuer = "https://token.actions.githubusercontent.com"

// gitlabIssuerDefault is the SaaS GitLab issuer; the `gitlab` preset
// accepts an `issuer` override for self-hosted instances.
const gitlabIssuerDefault = "https://gitlab.com"

// oidcLeeway tolerates modest clock skew between the box and the token
// issuer when validating exp/nbf/iat.
const oidcLeeway = 60 * time.Second

// TrustConfig is one `deploy_trust` block in Caddyfile/JSON form. Kind
// is the preset named on the block (`github`, `gitlab`, `oidc`,
// `local`); the remaining fields are resolved and validated into a
// trustSource by buildTrust.
type TrustConfig struct {
	Kind      string            `json:"kind,omitempty"`
	Issuer    string            `json:"issuer,omitempty"`     // oidc/gitlab override
	Audience  string            `json:"audience,omitempty"`   // required for OIDC presets
	PublicKey string            `json:"public_key,omitempty"` // local: path to a PKIX PEM ed25519 key
	Subject   string            `json:"subject,omitempty"`    // sugar for claim `sub`
	Claims    map[string]string `json:"claims,omitempty"`     // exact-match claim constraints
}

// trustSource is a validated, resolved trust declaration: presets
// mapped to issuers, local public keys loaded, claims folded. It holds
// no network state — verifiers are built from it in resolveVerifiers.
type trustSource struct {
	kind     string // "oidc" | "local"
	issuer   string // oidc
	audience string
	pubKey   ed25519.PublicKey // local
	keyPath  string            // local (for error messages)
	claims   map[string]string
}

// resolveTrustPlaceholders expands {env.*} (and other known Caddy
// placeholders) in a slice of trust configs, in place. Kind is a
// literal block token and is never a placeholder.
func resolveTrustPlaceholders(repl *caddy.Replacer, tcs []TrustConfig) {
	for i := range tcs {
		tcs[i].Issuer = repl.ReplaceKnown(tcs[i].Issuer, "")
		tcs[i].Audience = repl.ReplaceKnown(tcs[i].Audience, "")
		tcs[i].PublicKey = repl.ReplaceKnown(tcs[i].PublicKey, "")
		tcs[i].Subject = repl.ReplaceKnown(tcs[i].Subject, "")
		for k, v := range tcs[i].Claims {
			tcs[i].Claims[k] = repl.ReplaceKnown(v, "")
		}
	}
}

// buildTrust resolves the effective trust sources for one app. Per-app
// sources override the global default wholesale (same semantics as
// artifact_allowlist), never append. It loads local public keys and
// validates presets here so a bad key path or missing audience fails
// config load — fail-closed, like the rest of liveswap's config.
func buildTrust(global, perApp []TrustConfig) ([]trustSource, error) {
	src := global
	if len(perApp) > 0 {
		src = perApp
	}
	out := make([]trustSource, 0, len(src))
	for i, tc := range src {
		ts, err := resolveTrustConfig(tc)
		if err != nil {
			return nil, fmt.Errorf("deploy_trust[%d]: %w", i, err)
		}
		out = append(out, ts)
	}
	return out, nil
}

func resolveTrustConfig(tc TrustConfig) (trustSource, error) {
	claims := make(map[string]string, len(tc.Claims)+1)
	for k, v := range tc.Claims {
		claims[k] = v
	}
	if tc.Subject != "" {
		claims["sub"] = tc.Subject
	}

	switch tc.Kind {
	case "github", "gitlab", "oidc":
		issuer := tc.Issuer
		switch tc.Kind {
		case "github":
			if issuer != "" {
				return trustSource{}, fmt.Errorf("github preset does not take an issuer (it is %s)", githubIssuer)
			}
			issuer = githubIssuer
		case "gitlab":
			if issuer == "" {
				issuer = gitlabIssuerDefault
			}
		case "oidc":
			if issuer == "" {
				return trustSource{}, fmt.Errorf("oidc requires an issuer")
			}
		}
		if tc.Audience == "" {
			return trustSource{}, fmt.Errorf("%s requires an audience (never trust an unaudienced token)", tc.Kind)
		}
		if tc.PublicKey != "" {
			return trustSource{}, fmt.Errorf("%s does not take a public_key", tc.Kind)
		}
		if err := requireIdentityClaim(tc.Kind, claims); err != nil {
			return trustSource{}, err
		}
		return trustSource{kind: "oidc", issuer: issuer, audience: tc.Audience, claims: claims}, nil

	case "local":
		if tc.PublicKey == "" {
			return trustSource{}, fmt.Errorf("local requires a public_key path")
		}
		if tc.Issuer != "" {
			return trustSource{}, fmt.Errorf("local does not take an issuer")
		}
		key, err := loadEd25519PublicKey(tc.PublicKey)
		if err != nil {
			return trustSource{}, fmt.Errorf("local public_key: %w", err)
		}
		return trustSource{kind: "local", audience: tc.Audience, pubKey: key, keyPath: tc.PublicKey, claims: claims}, nil

	case "":
		return trustSource{}, fmt.Errorf("missing preset (use `deploy_trust github|gitlab|oidc|local { ... }`)")
	default:
		return trustSource{}, fmt.Errorf("unknown preset %q (use github|gitlab|oidc|local)", tc.Kind)
	}
}

// identityClaims lists, per preset, the claims that scope a token to a
// specific deployer. An audience alone is NOT identity: both GitHub and
// GitLab let any project mint a token for any audience, so a source with
// only an audience would authorize every repository on the issuer.
var identityClaims = map[string][]string{
	"github": {"repository", "repository_id", "repository_owner", "repository_owner_id", "sub"},
	"gitlab": {"project_path", "project_id", "namespace_path", "namespace_id", "sub"},
}

// requireIdentityClaim fails closed when an OIDC source pins no identity.
func requireIdentityClaim(kind string, claims map[string]string) error {
	if kind == "oidc" {
		if _, ok := claims["sub"]; ok {
			return nil
		}
		return fmt.Errorf("oidc requires an identity constraint — pin `subject`/`claim sub` (every OIDC token has one); an audience alone lets any workload of this issuer deploy")
	}
	for _, name := range identityClaims[kind] {
		if _, ok := claims[name]; ok {
			return nil
		}
	}
	return fmt.Errorf("%s requires an identity claim (one of: %s) — an audience alone authorizes every %s project", kind, strings.Join(identityClaims[kind], ", "), kind)
}

// newJWKSClient builds the HTTP client the OIDC verifier uses for
// discovery and JWKS fetches. It refuses plain-http requests (and
// https→http redirects) so a network attacker cannot swap the
// verification keys and forge deploy tokens — unless allow_insecure_http
// is set, the documented escape hatch for test rigs and LANs.
func newJWKSClient(allowInsecure bool) *http.Client {
	var rt http.RoundTripper = &http.Transport{Proxy: http.ProxyFromEnvironment}
	if !allowInsecure {
		rt = httpsOnlyTransport{base: rt}
	}
	// A small control request, so unlike the artifact download it takes
	// a bounded overall timeout.
	return &http.Client{Timeout: 30 * time.Second, Transport: rt}
}

type httpsOnlyTransport struct{ base http.RoundTripper }

func (t httpsOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-https OIDC request to %s (set allow_insecure_http for test rigs)", r.URL.Redacted())
	}
	return t.base.RoundTrip(r)
}

// loadEd25519PublicKey reads a PKIX PEM file (as emitted by
// `hotserve deploy-keygen`) and returns the ed25519 public key.
func loadEd25519PublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-configured public-key path, not request input
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key (%T)", path, pub)
	}
	return key, nil
}

// verifier authenticates a raw bearer JWT for one trust source.
type verifier interface {
	// verify returns nil iff the token's signature and standard claims
	// are valid and every configured claim constraint matches.
	verify(ctx context.Context, rawToken string) error
	// label identifies the source in logs (never includes secrets —
	// there are none).
	label() string
}

// resolveVerifiers turns validated trust sources into live verifiers.
// It is infallible: key loading and preset validation already happened
// in buildTrust, so a config reload can rewire auth without error.
func resolveVerifiers(sources []trustSource, jwksClient *http.Client) []verifier {
	out := make([]verifier, 0, len(sources))
	for _, ts := range sources {
		switch ts.kind {
		case "oidc":
			out = append(out, &oidcVerifier{
				issuer: ts.issuer, audience: ts.audience,
				claims: ts.claims, client: jwksClient,
			})
		case "local":
			out = append(out, &localVerifier{
				audience: ts.audience, pub: ts.pubKey,
				keyPath: ts.keyPath, claims: ts.claims,
			})
		}
	}
	return out
}

// warmVerifiers best-effort pre-fetches OIDC discovery/JWKS in the
// background so the first real deploy — and, importantly, the first
// verification of a *known* app — does not pay the discovery latency
// that would otherwise distinguish it (by timing) from an unknown app.
// Errors are ignored; a real request retries.
func warmVerifiers(verifierSets ...[]verifier) {
	for _, set := range verifierSets {
		for _, v := range set {
			if ov, ok := v.(*oidcVerifier); ok {
				go func(ov *oidcVerifier) {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_, _ = ov.ensure(ctx)
				}(ov)
			}
		}
	}
}

// authorize returns the label of the first verifier that accepts the
// token (for the deploy audit log), and whether any did.
func authorize(ctx context.Context, verifiers []verifier, rawToken string) (string, bool) {
	if rawToken == "" {
		return "", false
	}
	for _, v := range verifiers {
		if err := v.verify(ctx, rawToken); err == nil {
			return v.label(), true
		}
	}
	return "", false
}

// oidcVerifier validates a provider-issued OIDC token against the
// issuer's published JWKS. The provider (JWKS + discovery) is built
// lazily on first use, so a network blip fails a deploy — which is
// safe (the old version keeps serving) — rather than failing config
// load for the whole server.
type oidcVerifier struct {
	issuer   string
	audience string
	claims   map[string]string
	client   *http.Client

	mu  sync.Mutex
	idv *oidc.IDTokenVerifier
}

func (v *oidcVerifier) label() string { return "oidc:" + v.issuer }

func (v *oidcVerifier) ensure(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.idv != nil {
		return v.idv, nil
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, v.client), v.issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", v.issuer, err)
	}
	v.idv = provider.Verifier(&oidc.Config{ClientID: v.audience})
	return v.idv, nil
}

func (v *oidcVerifier) verify(ctx context.Context, rawToken string) error {
	idv, err := v.ensure(ctx)
	if err != nil {
		return err
	}
	tok, err := idv.Verify(oidc.ClientContext(ctx, v.client), rawToken)
	if err != nil {
		return err
	}
	// go-oidc unmarshals into json.RawMessage by copying the verified
	// claim bytes; decodeClaims then re-parses them with numbers kept as
	// json.Number (see decodeClaims for why %v on a float64 is unsafe).
	var raw json.RawMessage
	if err := tok.Claims(&raw); err != nil {
		return err
	}
	claims, err := decodeClaims(raw)
	if err != nil {
		return err
	}
	return matchClaims(v.claims, claims)
}

// decodeClaims unmarshals a JWT claim set with numbers preserved as
// json.Number instead of float64. matchClaims stringifies each claim to
// compare it, and %v on a float64 mangles numeric identity claims:
// round-numbered IDs render in scientific notation (100000000 → "1e+08")
// and integers above 2^53 lose precision, so a pinned `claim
// repository_id 100000000` (a documented identity constraint) would
// silently never match. json.Number carries the exact source digits.
func decodeClaims(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// localVerifier validates a JWT signed by the operator's local private
// key against the configured public key. Standard-claim checks (exp,
// nbf, aud) are enforced here because there is no OIDC provider to do
// them.
type localVerifier struct {
	audience string
	pub      ed25519.PublicKey
	keyPath  string
	claims   map[string]string
}

func (v *localVerifier) label() string { return "local:" + v.keyPath }

func (v *localVerifier) verify(_ context.Context, rawToken string) error {
	tok, err := jwt.ParseSigned(rawToken, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return err
	}
	var std jwt.Claims
	var raw json.RawMessage
	// Claims verifies the signature against v.pub, then unmarshals the
	// payload into both the standard-claims struct and the raw bytes.
	if err := tok.Claims(v.pub, &std, &raw); err != nil {
		return err
	}
	// Decode with numbers kept as json.Number, so a numeric claim in a
	// hand-minted local token compares the same way as an OIDC one.
	all, err := decodeClaims(raw)
	if err != nil {
		return err
	}
	// ValidateWithLeeway only checks exp when present, so a local token
	// with no expiry would be accepted forever. Require it — the
	// short-lived-token guarantee depends on it.
	if std.Expiry == nil {
		return fmt.Errorf("local token has no exp claim")
	}
	expected := jwt.Expected{Time: time.Now()}
	if v.audience != "" {
		expected.AnyAudience = jwt.Audience{v.audience}
	}
	if err := std.ValidateWithLeeway(expected, oidcLeeway); err != nil {
		return err
	}
	return matchClaims(v.claims, all)
}

// matchClaims requires every constraint to equal the token's claim
// value (string-compared). Missing or mismatched → error naming the
// first offending claim, so a misconfigured allowlist is diagnosable.
func matchClaims(want map[string]string, got map[string]any) error {
	for _, name := range sortedKeys(want) {
		got, ok := got[name]
		if !ok {
			return fmt.Errorf("claim %q absent from token", name)
		}
		if claimString(got) != want[name] {
			return fmt.Errorf("claim %q mismatch", name)
		}
	}
	return nil
}

// claimString renders a token claim value for exact-string comparison
// against the operator's config. Scalars format canonically —
// json.Number by its source digits (see decodeClaims), never via %v on a
// float64. Non-scalars (arrays/objects) fall through to %v: they are not
// meaningful identity constraints, so this only makes them not-match.
func claimString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// Defensive: a claim set reached here without UseNumber. Format
		// as a plain decimal, never scientific notation.
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
