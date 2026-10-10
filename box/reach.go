package box

// Step 6's reachability (DESIGN-box.md, "Reachability: what may stand
// before box_webhook"): of the pushed file's adaptation — Caddy's JSON
// as `hotserve adapt` prints it — whether a GET and a POST for / on the
// box_webhook host meet box_webhook before any handler that could
// answer them. Pure: the JSON in, a verdict out, no module loaded but
// the five matcher types it decides with. Fail-closed both ways: a
// handler the table has not weighed answers, and a matcher the table
// cannot decide may match.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/smallhoursorg/hotserve/box/proof"
)

const msgAdaptationTooLarge = "the new Caddyfile's adaptation is larger than 16 MiB"

// msgUnreachable is the catalogue's prefix for every reachability
// refusal; what is the block's text.
func msgUnreachable(what string) string {
	return "the new Caddyfile does not reach box_webhook on /: " + what
}

// The adaptation's shape, as far as the walk reads it: the http app's
// servers, each a route list; a route's group, matcher sets, handlers
// and terminal flag; a handler's module name, a subroute's routes and a
// headers handler's request section. Everything else is left where it
// is.
type adaptedRoute struct {
	Group    string                       `json:"group"`
	Match    []map[string]json.RawMessage `json:"match"`
	Handle   []adaptedHandler             `json:"handle"`
	Terminal bool                         `json:"terminal"`
}

type adaptedHandler struct {
	Handler string `json:"handler"`
	// Routes and Request are read only for the handler they belong to
	// (a subroute's, a headers handler's): another module's key of the
	// same name, whatever its shape, is not this walk's to decode.
	Routes  json.RawMessage `json:"routes"`
	Request json.RawMessage `json:"request"`
	// sub is a subroute's Routes, decoded once by resolve, so the walk
	// reads a tree with no error path of its own.
	sub []adaptedRoute
}

// resolve decodes every subroute's routes in the tree.
func resolve(routes []adaptedRoute) error {
	for i := range routes {
		for j := range routes[i].Handle {
			h := &routes[i].Handle[j]
			if h.Handler != "subroute" || len(h.Routes) == 0 {
				continue
			}
			if json.Unmarshal(h.Routes, &h.sub) != nil {
				return errAdaptation
			}
			if err := resolve(h.sub); err != nil {
				return err
			}
		}
	}
	return nil
}

// errAdaptation is the box's error for an adaptation the walk cannot
// read: a fixed text, since a decoder's words quote the input.
var errAdaptation = errors.New("the adaptation does not parse")

// reach asks the question of adapted for host, the box_webhook host
// the walk found. nil: both requests meet box_webhook first. A
// *refusal: the catalogue text naming what stands in the way. Any other
// error is the box's own: an adaptation this code cannot read, or one
// with no box_webhook routed for the host, which the walk of the same
// bytes rules out.
func reach(adapted []byte, host string) error {
	var cfg struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []adaptedRoute `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if json.Unmarshal(adapted, &cfg) != nil {
		return errAdaptation
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Apps.HTTP.Servers)) {
		// The server that carries box_webhook is the one whose
		// listener the workflow reaches; another server's routes for
		// the same host answer on another port.
		routes := cfg.Apps.HTTP.Servers[name].Routes
		if err := resolve(routes); err != nil {
			return err
		}
		if !carriesWebhook(routes) {
			continue
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			w := &reachWalk{req: reachRequest(method, host), groups: map[string]groupState{}}
			switch end, b := w.routes(routes, matches, "", true); end {
			case reachedWebhook:
			case blocked:
				return &refusal{msg: msgUnreachable(b.text(host))}
			default:
				return fmt.Errorf("the adaptation routes %s / on %s past box_webhook", method, proof.Bound(host))
			}
		}
		return nil
	}
	return fmt.Errorf("the adaptation has no box_webhook")
}

// carriesWebhook reports whether a box_webhook handler stands anywhere
// in routes.
func carriesWebhook(routes []adaptedRoute) bool {
	for _, r := range routes {
		for _, h := range r.Handle {
			if h.Handler == "box_webhook" || (h.Handler == "subroute" && carriesWebhook(h.sub)) {
				return true
			}
		}
	}
	return false
}

// matchStatus is what a matcher, a set or a route says of the request:
// it does not match, it may (decided per request), it does.
type matchStatus int

const (
	noMatch matchStatus = iota
	mayMatch
	matches
)

// reachEnd is how a walk of a route list ended: the routes ran out, the
// request met box_webhook, or something stood in the way.
type reachEnd int

const (
	fellThrough reachEnd = iota
	reachedWebhook
	blocked
)

// reachBlock is what stood in the way: a handler by module name, or a
// terminal route that ran out of handlers (name "": a site when it
// stands at the server level), under a match that is certain or, when
// may is set, hangs on matcher.
type reachBlock struct {
	name    string
	site    bool
	changes bool
	may     bool
	matcher string
}

// text is the block's catalogue text, every input-derived value bounded.
func (b *reachBlock) text(host string) string {
	what := proof.Bound(b.name)
	switch {
	case b.name != "":
	case b.site:
		what = "an earlier site for " + proof.Bound(host)
	default:
		what = "a terminal route"
	}
	switch {
	case b.may && b.changes:
		return what + " may change the request first (its " + proof.Bound(b.matcher) + " matcher is decided per request)"
	case b.may:
		return what + " may answer first (its " + proof.Bound(b.matcher) + " matcher is decided per request)"
	case b.changes:
		return what + " changes the request first"
	default:
		return what + " answers first"
	}
}

// groupState is what a route group has matched so far: Caddy skips a
// member once an earlier one matched, so a later member runs for sure
// only while no earlier one did, and at most "may" after one may have.
type groupState struct {
	status  matchStatus
	matcher string
}

type reachWalk struct {
	req    *http.Request
	groups map[string]groupState
}

// routes walks rs in Caddy's order, each route under the enclosing
// match outer (hanging on matcher when outer is mayMatch), and ends at
// the first handler that decides. top is the server's own route list,
// whose routes are sites.
func (w *reachWalk) routes(rs []adaptedRoute, outer matchStatus, matcher string, top bool) (reachEnd, *reachBlock) {
	for _, r := range rs {
		st, m := w.match(r.Match)
		if st == noMatch {
			continue
		}
		if r.Group != "" {
			// The group records the member's own match: a member that
			// matches for sure satisfies the group whether or not an
			// earlier one may have, and later members never run.
			g := w.groups[r.Group]
			if g.status == matches {
				continue
			}
			if st > g.status {
				w.groups[r.Group] = groupState{st, m}
			}
			if g.status == mayMatch && st == matches {
				st, m = mayMatch, g.matcher
			}
		}
		if outer < st {
			st, m = outer, matcher
		}
		for _, h := range r.Handle {
			switch classify(h) {
			case classReached:
				return reachedWebhook, nil
			case classSubroute:
				if end, b := w.routes(h.sub, st, m, false); end != fellThrough {
					return end, b
				}
			case classPasses:
			case classChanges:
				return blocked, &reachBlock{name: h.Handler, changes: true, may: st == mayMatch, matcher: m}
			default:
				return blocked, &reachBlock{name: h.Handler, may: st == mayMatch, matcher: m}
			}
		}
		if r.Terminal {
			// Caddy hands the rest of the chain an empty handler: the
			// request ends here with whatever was written, or nothing.
			return blocked, &reachBlock{site: top, may: st == mayMatch, matcher: m}
		}
	}
	return fellThrough, nil
}

type handlerClass int

const (
	classAnswers handlerClass = iota
	classPasses
	classChanges
	classSubroute
	classReached
)

// classify is the first Reachability table.
func classify(h adaptedHandler) handlerClass {
	switch h.Handler {
	case "box_webhook":
		return classReached
	case "subroute":
		return classSubroute
	case "vars", "map", "log_append", "tracing", "request_body", "timeouts",
		"encode", "push", "templates", "hint_penaltybox":
		return classPasses
	case "headers":
		if len(h.Request) > 0 && string(h.Request) != "null" {
			return classChanges
		}
		return classPasses
	case "rewrite":
		return classChanges
	}
	return classAnswers
}

// match is a route's matcher sets: any set that matches, else any that
// may (named by its first undecided matcher), else none. No set is a
// match.
func (w *reachWalk) match(sets []map[string]json.RawMessage) (matchStatus, string) {
	if len(sets) == 0 {
		return matches, ""
	}
	best, name := noMatch, ""
	for _, set := range sets {
		st, m := w.set(set)
		if st > best {
			best, name = st, m
		}
		if best == matches {
			return matches, ""
		}
	}
	return best, name
}

// set is one matcher set: every matcher must match; one that does not
// decides; one that may, when none says no, leaves "may".
func (w *reachWalk) set(set map[string]json.RawMessage) (matchStatus, string) {
	st, name := matches, ""
	for _, k := range slices.Sorted(maps.Keys(set)) {
		switch w.one(k, set[k]) {
		case noMatch:
			return noMatch, ""
		case mayMatch:
			if st == matches {
				st, name = mayMatch, k
			}
		}
	}
	return st, name
}

// one is the second Reachability table: the five matchers decided
// statically with Caddy's own types, and anything else, or a pattern
// holding a placeholder, "may".
func (w *reachWalk) one(key string, raw json.RawMessage) matchStatus {
	switch key {
	case "host":
		list, ok := staticPatterns(raw)
		if !ok {
			return mayMatch
		}
		m := caddyhttp.MatchHost(list)
		if m.Provision(caddy.Context{}) != nil {
			return mayMatch
		}
		return decided(m.Match(w.req))
	case "path":
		list, ok := staticPatterns(raw)
		if !ok {
			return mayMatch
		}
		m := caddyhttp.MatchPath(list)
		if m.Provision(caddy.Context{}) != nil {
			return mayMatch
		}
		return decided(m.Match(w.req))
	case "method":
		list, ok := staticPatterns(raw)
		if !ok {
			return mayMatch
		}
		return decided(caddyhttp.MatchMethod(list).Match(w.req))
	case "protocol":
		// Only the scheme is the request's to know: http and https by
		// TLS, grpc by a content type the workflow never sends. An HTTP
		// version is the connection's, negotiated per request.
		var proto string
		if json.Unmarshal(raw, &proto) != nil {
			return mayMatch
		}
		switch proto {
		case "http", "https", "grpc":
			return decided(caddyhttp.MatchProtocol(proto).Match(w.req))
		}
		return mayMatch
	case "not":
		var sets []map[string]json.RawMessage
		if json.Unmarshal(raw, &sets) != nil {
			return mayMatch
		}
		if len(sets) == 0 {
			return matches // as Caddy: nothing negated
		}
		switch st, _ := w.match(sets); st {
		case matches:
			return noMatch
		case noMatch:
			return matches
		}
	}
	return mayMatch
}

// staticPatterns reads a list of patterns none of which holds a
// placeholder.
func staticPatterns(raw json.RawMessage) ([]string, bool) {
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return nil, false
	}
	for _, p := range list {
		if strings.Contains(p, "{") {
			return nil, false
		}
	}
	return list, true
}

func decided(matched bool) matchStatus {
	if matched {
		return matches
	}
	return noMatch
}

// reachRequest is the request the question is asked for: method on
// https://host/, no query, no header, over TLS, with the replacer
// Caddy's path matcher expects in the context.
func reachRequest(method, host string) *http.Request {
	r := &http.Request{
		Method: method, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		URL: &url.URL{Scheme: "https", Host: host, Path: "/"}, Host: host, RequestURI: "/",
		Header: http.Header{}, TLS: &tls.ConnectionState{},
	}
	return r.WithContext(context.WithValue(context.Background(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
}
