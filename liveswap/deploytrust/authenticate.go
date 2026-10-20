package deploytrust

import (
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Refusal is what the preamble answers a refused request with, for the
// caller to write (Write) through its own response filter: the flat
// 401 and its fixed body, the same whatever the reason and however
// often the address has failed, so a caller learns neither which apps
// exist, nor what a source pins, nor what the throttle's budgets say.
// Nothing in it comes from the request or from the reason; the reason
// went to the journal, if the budgets let it.
type Refusal struct {
	Status  int
	Message string
}

// Write answers the refused request with the fixed body through write
// — the caller's filtered writer, so every body a webhook sends passes
// its filter.
func (ref *Refusal) Write(write func(code int, body any) error) error {
	return write(ref.Status, map[string]string{"error": ref.Message})
}

// unauthorizedMessage is the flat 401: the same sentence whatever the
// reason, so a caller learns neither which apps exist nor what a
// source pins.
const unauthorizedMessage = "invalid or missing deploy token (Authorization: Bearer <jwt>)"

// RefusalFloor is the soonest a refused token is answered after the
// webhook began handling it (#306). Judging is local, but not equal:
// each source the name resolves to parses the token and checks a
// signature, and a name with no source does neither, so the time to
// the 401 alone would say how many sources a name has, and so whether
// the app exists. Measured on an arm64 machine (2026-10-20), an OIDC
// source refuses a 2 KiB token (about a real GitHub one) in 0.11 ms
// and a 16 KiB one in 0.33 ms, so one of MaxTokenLen in between; a
// local source in a tenth of that; a 1 MiB token, which the header
// limit allows, took 16 ms. The floor covers MaxSources checks of a
// MaxTokenLen token on a box ten times slower (an estimate, not
// measured); Build refuses a set of more, authorize a longer token,
// and a source checks one key per token (verifyWith). A refusal that
// took longer anyway, on a loaded box, is held to the next multiple of
// the floor and the overrun logged (holdRefusal). An accepted token is
// not held.
const RefusalFloor = 25 * time.Millisecond

// MaxTokenLen is the longest bearer token judged; a longer one is
// refused before any source parses it. A GitHub or GitLab OIDC token
// is 1–2 KiB, a `hotserve deploy-token` one smaller. An accepted token
// over three quarters of it is logged (nearTokenCapLabel), so one that
// grows toward it is seen before it is refused.
const MaxTokenLen = 8 << 10

// MaxSources is the most deploy_trust sources one name resolves to
// (Build); more than three quarters of it warns at config load
// (WarnSourceCount).
const MaxSources = 8

// overrunLabel and nearTokenCapLabel key Authenticate's once-per-window
// warnings in the limiter's outage table.
const (
	overrunLabel      = "refusal floor overrun"
	nearTokenCapLabel = "token near MaxTokenLen"
)

// holdRefusal returns at the first multiple of floor after start that
// judging has not passed: floor itself, unless judging took longer, so
// an overrun shows only in floor-sized steps and is reported (overran).
// A caller that went away is held all the same: an HTTP/1.1 client can
// half-close its side, which ends the request's context, and still
// read the answer. A floor of 0 holds nothing (a test's limiter).
func holdRefusal(start time.Time, floor time.Duration) (took time.Duration, overran bool) {
	if floor <= 0 {
		return 0, false
	}
	took = time.Since(start)
	steps := took/floor + 1
	time.Sleep(time.Until(start.Add(steps * floor)))
	return took, steps > 1
}

// WarnSourceCount warns, at config load, about a set of deploy_trust
// sources over three quarters of MaxSources, before a config that adds
// one more is refused. where names the set (an app, the global blocks,
// the box).
func WarnSourceCount(logger *zap.Logger, n int, where ...zap.Field) {
	if n > MaxSources*3/4 {
		logger.Warn("deploy_trust: many sources for one name; a config with more than the limit does not load",
			append(where, zap.Int("sources", n), zap.Int("limit", MaxSources))...)
	}
}

// bearerToken extracts the deploy JWT from `Authorization: Bearer
// <jwt>`. Bearer is the only accepted transport: Caddy redacts the
// Authorization header from access logs automatically (the retired
// X-Liveswap-Secret custom header did not, which leaked it).
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	if auth := r.Header.Get("Authorization"); len(auth) > len(prefix) &&
		strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}
	return ""
}

// scopeField appends the caller's one journal field for the preamble's
// lines — liveswap's app name; whatever the box webhook names its
// request by — when there is one. The value is bounded as a refusal is
// (boundRefusal: one line of at most MaxRefusalLen bytes) whatever the
// caller put in it: the limiter bounds how many lines a caller can
// write, not how long each is, so the length is bounded here, at the
// mechanism, rather than asked of each caller. The key is the caller's
// own constant.
func scopeField(fields []zap.Field, key, value string) []zap.Field {
	if key == "" {
		return fields
	}
	return append(fields, zap.String(key, boundRefusal(value)))
}

// Authenticate is the preamble every webhook request passes before
// anything else is revealed, and the one place a deploy token is
// judged: the bearer against verifiers (authorize), then what a
// refusal costs this limiter and the journal. An accepted token is who
// it is (Identity), and the address's failures are forgotten. A
// refused one is answered by the caller with the Refusal returned —
// the flat 401, whatever the reason and whatever the budgets say —
// through the caller's own response filter, so every body a webhook
// sends passes one. logger is the caller's; scopeKey and
// scopeValue are the one field its lines carry besides the address
// and the reason — liveswap names the app — bounded where a line is
// written (scopeField); an empty key is no field. liveswap's webhook
// and the box's both call this on the limiter Shared returns, so one
// address has one budget whichever webhook a flood aims at. start is
// when the webhook began handling the request, before anything whose
// time depends on what was asked for (liveswap's app lookup and the
// lock a reload holds there): a refusal is returned no sooner than the
// limiter's floor after it (holdRefusal).
func (l *Limiter) Authenticate(r *http.Request, start time.Time, verifiers []Verifier, logger *zap.Logger, scopeKey, scopeValue string) (Identity, *Refusal) {
	token := bearerToken(r)
	who, down, refused := authorize(r.Context(), verifiers, token)
	key := clientKey(r)
	// A source the box could not consult is named here once per window
	// per source, whatever the budgets and whether or not another
	// source then accepted the token; a refusal is still charged below
	// like any other — see unavailable for why both.
	for _, u := range down {
		if l.outage(u.label, scopeKey) {
			fields := scopeField([]zap.Field{zap.String("source", u.label)}, scopeKey, scopeValue)
			logger.Warn("webhook auth could not consult a trust source",
				append(fields, zap.String("remote", key), zap.String("reason", boundRefusal(u.Error())))...)
		}
	}
	if refused != nil {
		// What a failure costs in the journal is the limiter's call
		// (see Limiter): the count of lines, and — the scope and the
		// refusal being request input, both logged bounded — the size
		// of each. The refusal is for this journal only: the response
		// stays the same flat 401 for every reason and every verdict, so
		// a caller learns neither which apps exist, nor what a source
		// pins, nor whether its failures are still being logged.
		v := l.fail(key)
		if v.log {
			logger.Warn("webhook auth failed",
				append(scopeField(nil, scopeKey, scopeValue), zap.String("remote", key), zap.String("refused", refused.Error()))...)
		}
		if v.trippedKey {
			logger.Warn("webhook auth failures from this address throttled: further ones are not logged",
				zap.String("remote", key), zap.Int("failures", FailBudget), zap.Duration("window", FailWindow))
		}
		if v.trippedGlobal {
			logger.Warn("webhook auth failures throttled process-wide: further ones are not logged",
				zap.Int("failures", FailGlobalBudget), zap.Duration("window", FailWindow))
		}
		if took, overran := holdRefusal(start, l.floor); overran && l.outage(overrunLabel, scopeKey) {
			logger.Warn("webhook auth: judging a refused token took longer than the refusal floor; its answer was held to the next multiple, so its timing may say how many sources checked it",
				append(scopeField(nil, scopeKey, scopeValue), zap.Duration("took", took), zap.Duration("floor", l.floor))...)
		}
		return Identity{}, &Refusal{Status: http.StatusUnauthorized, Message: unauthorizedMessage}
	}
	if len(token) > MaxTokenLen*3/4 && l.outage(nearTokenCapLabel, scopeKey) {
		logger.Warn("webhook auth: an accepted token is near the length limit; a longer one is refused",
			append(scopeField(nil, scopeKey, scopeValue), zap.String("deployed_by", who.By), zap.Int("bytes", len(token)), zap.Int("limit", MaxTokenLen))...)
	}
	l.clear(key)
	return who, nil
}
