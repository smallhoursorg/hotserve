package liveswap

import (
	"errors"
	"maps"
	"net/http"
	"slices"

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
// webhooks cannot drift apart: nothing here has behaviour of its own
// beyond what NewTrust says, and export_test.go holds each to its
// original. Verifier, Identity (deploytrust.go), Redactor (redact.go)
// and ParseDeployTrust (caddyfile.go) are exported where they live.

// Authenticate is the webhook preamble (authenticate, handler.go) on
// the process-wide failure budget every webhook mount shares, so one
// address has one budget whichever webhook a flood aims at. It judges
// the bearer against verifiers, answers a refused request itself —
// the flat 401 whatever the reason, or 429 once the address's budget
// is spent — and returns ok=false with the response write's error; on
// ok, who the token is, with its claims behind Identity.Claim. scope
// is what the journal lines carry besides the address and the reason
// — this webhook names the app — and a string in it is bounded as a
// refusal is (boundScope: one line, maxRefusalLen bytes) whatever the
// caller put there, because the limiter bounds how many lines a
// caller can write and not how long each is.
func Authenticate(w http.ResponseWriter, r *http.Request, verifiers []Verifier, logger *zap.Logger, scope ...zap.Field) (Identity, bool, error) {
	return authenticate(w, r, verifiers, logger, webhookAuthLimiter, scope...)
}

// NewTrust is the verifiers for a set of `deploy_trust` blocks, built
// the way App.Provision builds liveswap's global set: {env.*}
// placeholders resolved (resolveTrustPlaceholders), presets validated
// and local keys loaded (buildTrust — a bad key path or an
// unaudienced OIDC source is a config error, fail-closed), JWKS
// fetched over https only unless allowInsecure (newJWKSClient), and
// OIDC discovery warmed in the background (warmVerifiers) so the first
// request does not pay for it. Two things Provision does not do,
// because its caller is itself: the placeholders are resolved on a
// copy, so configs still read as parsed — a caller that renders or
// diffs its config later writes {env.NAME}, never the value — and no
// source at all is refused here, at config load, rather than as a 401
// for every token at request time.
func NewTrust(configs []TrustConfig, allowInsecure bool) ([]Verifier, error) {
	if len(configs) == 0 {
		return nil, errors.New("deploy_trust: no source configured; a webhook with none would refuse every token")
	}
	own := slices.Clone(configs)
	for i := range own {
		own[i].Claims = maps.Clone(own[i].Claims)
	}
	resolveTrustPlaceholders(caddy.NewReplacer(), own)
	sources, err := buildTrust(own, nil)
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
// The pairs are the caller's choice, as an app's are liveswap's: it
// primes an app's filter with env_file alone and leaves SOCKET, HOME
// and PATH readable, because a diagnostic needs its paths, and a
// caller priming with a process environment will want the same for
// the variables that are paths (PATH, HOME, XDG_*, RUNTIME_DIRECTORY).
// Build it once, at Provision, not per response: every form of every
// value is computed here, and a running process's environment does
// not change. An empty environ gives the two heuristic layers alone,
// as a nil *Redactor does.
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
