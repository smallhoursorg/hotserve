package liveswap

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// This file is liveswap's surface for the box subsystem (box/, the
// channel that writes /etc/hotserve/Caddyfile — box/DESIGN-box.md).
// Its webhook authenticates a push against a `deploy_trust` block
// exactly as a deploy is authenticated here, on the failure budgets
// this webhook charges, and sends every body through the same
// response filter. Each name below is the function the handler or
// Provision already calls, under an exported name, so the two
// webhooks cannot drift apart: nothing here has behaviour of its own,
// and export_test.go holds each to its original. Verifier, Identity
// (deploytrust.go), Redactor (redact.go) and ParseDeployTrust
// (caddyfile.go) are exported where they live.

// Authenticate is the webhook preamble (authenticate, handler.go) on
// the process-wide failure budget every webhook mount shares, so one
// address has one budget whichever webhook a flood aims at. It judges
// the bearer against verifiers, answers a refused request itself —
// the flat 401 whatever the reason, or 429 once the address's budget
// is spent — and returns ok=false with the response write's error; on
// ok, who the token is, with its claims behind Identity.Claim. scope
// is what the journal lines carry besides the address and the reason:
// this webhook names the app, and a caller names what identifies its
// request, bounded as loggedAppName bounds an app name, because the
// limiter bounds how many lines a caller can write and not how long
// each is.
func Authenticate(w http.ResponseWriter, r *http.Request, verifiers []Verifier, logger *zap.Logger, scope ...zap.Field) (Identity, bool, error) {
	return authenticate(w, r, verifiers, logger, webhookAuthLimiter, scope...)
}

// NewTrust is the verifiers for a set of `deploy_trust` blocks, built
// the way App.Provision builds liveswap's global set: {env.*}
// placeholders resolved in place (resolveTrustPlaceholders), presets
// validated and local keys loaded (buildTrust — a bad key path or an
// unaudienced OIDC source is a config error, fail-closed), JWKS
// fetched over https only unless allowInsecure (newJWKSClient), and
// OIDC discovery warmed in the background (warmVerifiers) so the first
// request does not pay for it.
func NewTrust(configs []TrustConfig, allowInsecure bool) ([]Verifier, error) {
	resolveTrustPlaceholders(caddy.NewReplacer(), configs)
	sources, err := buildTrust(configs, nil)
	if err != nil {
		return nil, err
	}
	vs := resolveVerifiers(sources, newJWKSClient(allowInsecure))
	warmVerifiers(vs)
	return vs, nil
}

// NewEnvRedactor is the response filter with environ's values as its
// known secrets — KEY=VALUE pairs as os.Environ gives them — for text
// that may echo them: `caddy validate` quotes expanded values in its
// errors, and the box webhook runs it in the service's environment.
// An empty environ gives the two heuristic layers alone, as a nil
// *Redactor does.
func NewEnvRedactor(environ []string) *Redactor {
	return newRedactor(environ, nil)
}

// Redact is s through the filter — the known values, then the shape
// and entropy layers (redact.go). The text form, for what is written
// somewhere other than a response body; a body goes through
// RespondJSON, which also names the keys it found.
func (r *Redactor) Redact(s string) string {
	s, _ = r.redact(s)
	return s
}

// RespondJSON writes v as a JSON body the way every body this
// webhook sends is written (respondJSON): through r, the caller's
// filter, or through the shape and entropy layers alone when r is
// nil. When a known value was found the object gains a `redacted_env`
// field naming the keys.
func RespondJSON(w http.ResponseWriter, code int, v any, r *Redactor) error {
	return respondJSON(w, code, v, r)
}
