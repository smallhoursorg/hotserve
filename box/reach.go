package box

// Step 6's reachability (DESIGN-box.md, "Reachability: what may stand
// before box_webhook"): of the pushed file's adaptation — Caddy's JSON
// as `hotserve adapt` prints it — whether the server that carries
// box_webhook still listens where the push arrived (the door), whether
// the workflow's connection reaches that server's routes, and whether a
// GET and a POST for / on the box_webhook host then meet box_webhook
// before any handler that could answer them. Pure: the JSON and the
// door in, a verdict out, no module loaded but the five matcher types
// it decides with and Caddy's SNI matcher. Fail-closed every way: a
// handler, a listener wrapper or a TLS policy setting the tables have
// not weighed answers or refuses, a matcher they cannot decide may
// match, and one they do not know may fail.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddytls"

	"github.com/smallhoursorg/hotserve/box/proof"
)

const msgAdaptationTooLarge = "the new Caddyfile's adaptation is larger than 16 MiB"

// msgUnreachable is the catalogue's prefix for every reachability
// refusal; what is the block's text.
func msgUnreachable(what string) string {
	return "the new Caddyfile does not reach box_webhook on /: " + what
}

// errAdaptation is the box's error for an adaptation the walk cannot
// read: a fixed text, since a decoder's words quote the input.
var errAdaptation = errors.New("the adaptation does not parse")

// The adaptation is decoded as one JSON value and converted once, in a
// single pass linear in its size, into the shape the walk reads: a
// route's group, matcher sets, handlers and terminal flag; a handler's
// module name, whether a headers handler has a request section, and a
// subroute's routes. Another module's keys are never read, whatever
// they hold; a value of the wrong shape where a route list, a set or a
// handler must be is the adaptation's fault.
type adaptedRoute struct {
	group    string
	match    []map[string]any
	handle   []adaptedHandler
	terminal bool
}

type adaptedHandler struct {
	name    string
	request bool
	sub     []adaptedRoute
}

// routeList converts a decoded "routes" value.
func routeList(v any) ([]adaptedRoute, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errAdaptation
	}
	routes := make([]adaptedRoute, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errAdaptation
		}
		var r adaptedRoute
		r.group, _ = m["group"].(string)
		r.terminal, _ = m["terminal"].(bool)
		if sets := m["match"]; sets != nil {
			l, ok := sets.([]any)
			if !ok {
				return nil, errAdaptation
			}
			for _, s := range l {
				set, ok := s.(map[string]any)
				if !ok {
					return nil, errAdaptation
				}
				r.match = append(r.match, set)
			}
		}
		if hs := m["handle"]; hs != nil {
			l, ok := hs.([]any)
			if !ok {
				return nil, errAdaptation
			}
			for _, item := range l {
				hm, ok := item.(map[string]any)
				if !ok {
					return nil, errAdaptation
				}
				var h adaptedHandler
				h.name, _ = hm["handler"].(string)
				switch h.name {
				case "subroute":
					sub, err := routeList(hm["routes"])
					if err != nil {
						return nil, err
					}
					h.sub = sub
				case "headers":
					h.request = hm["request"] != nil
				}
				r.handle = append(r.handle, h)
			}
		}
		routes = append(routes, r)
	}
	return routes, nil
}

// reach asks the question of adapted for host, the box_webhook host
// the walk found, and door, the listen addresses of the loaded server
// the push arrived through — nil when no Caddy server received it (a
// test), and the door is not compared. nil: the door is kept and both
// requests meet box_webhook first. A *refusal: the
// catalogue text naming what stands in the way. Any other error is
// the box's own: an adaptation this code cannot read, or one with no
// box_webhook routed for the host, which the walk of the same bytes
// rules out.
func reach(adapted []byte, host string, door []string) error {
	var cfg any
	if json.Unmarshal(adapted, &cfg) != nil {
		return errAdaptation
	}
	servers, _ := dig(cfg, "apps", "http", "servers").(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		// The server that carries box_webhook is the one whose
		// listener the workflow reaches; another server's routes for
		// the same host answer on another port.
		routes, err := routeList(dig(servers[name], "routes"))
		if err != nil {
			return err
		}
		if !carriesWebhook(routes) {
			continue
		}
		// The door first: the push arrived through the loaded server's
		// addresses, which are thereby proven to work; a file that
		// moves them is the console's to apply ("The door").
		if door != nil {
			listen, err := listenOf(servers[name])
			if err != nil {
				return err
			}
			if !slices.Equal(canonical(door), canonical(listen)) {
				return &refusal{msg: msgListenMoved}
			}
		}
		// The connection next: what refuses it stands before every
		// route.
		s, err := settingsOf(servers[name])
		if err != nil {
			return err
		}
		if what := s.connection(host); what != "" {
			return &refusal{msg: msgUnreachable(what)}
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			w := &reachWalk{req: reachRequest(method, host), groups: map[string]groupState{}}
			switch end, b := w.routes(routes, matchVerdict{status: matches}, true); end {
			case reachedWebhook:
			case blocked:
				return &refusal{msg: msgUnreachable(b.text(host))}
			default:
				return fmt.Errorf("the adaptation routes %s / on %s past box_webhook", method, proof.Bound(host))
			}
		}
		return nil
	}
	return errors.New("the adaptation has no box_webhook")
}

// dig follows keys through decoded objects; nil when the path is not
// there or runs through something else.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// msgListenMoved is the catalogue text for a push that moves the box
// server's listen addresses. It names no address: an address may hold
// a value the adapter expanded from the service's environment, and
// the signer has the file's diff.
const msgListenMoved = "this push changes where the box's server listens (its listen addresses differ from the loaded config's): " +
	"a push cannot move the address it arrives on; make that change at the console"

// listenOf is a server's `listen` list as Caddy keeps it once loaded:
// each address with its placeholders replaced by the global replacer
// (`{env.*}`, `{system.*}`), as App.Provision replaces them before the
// loaded server holds them, so the two sides are spelt alike. The
// adapter writes every server a list, so a server without one, or
// with one of another shape, is the adaptation's fault, as in the
// routes; so is a placeholder that does not resolve, since Provision
// refuses it and validate ran Provision on the same bytes in the same
// environment.
func listenOf(server any) ([]string, error) {
	m, _ := server.(map[string]any)
	items, ok := m["listen"].([]any)
	if !ok {
		return nil, errAdaptation
	}
	repl := caddy.NewReplacer()
	listen := make([]string, 0, len(items))
	for _, item := range items {
		addr, ok := item.(string)
		if !ok {
			return nil, errAdaptation
		}
		addr, err := repl.ReplaceOrErr(addr, true, true)
		if err != nil {
			return nil, errAdaptation
		}
		listen = append(listen, addr)
	}
	return listen, nil
}

// canonical is a listen list as a sorted set of addresses as Caddy
// spells them — the network, host and port range as ParseNetworkAddress
// reads them and String prints them, the default network dropped — so
// that one list written with its network, in another order or with an
// address twice compares equal to itself. Spelling, not sockets: `:443`
// and `[::]:443` differ, as do `:443-444` and `:443, :444`; a port
// range stands as a range (the adapter writes none), never expanded.
// An address Caddy cannot parse stands as written.
func canonical(listen []string) []string {
	set := map[string]bool{}
	for _, addr := range listen {
		if na, err := caddy.ParseNetworkAddress(addr); err == nil {
			addr = na.String()
		}
		set[addr] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// carriesWebhook reports whether a box_webhook handler stands anywhere
// in routes.
func carriesWebhook(routes []adaptedRoute) bool {
	for _, r := range routes {
		for _, h := range r.handle {
			if h.name == "box_webhook" || (h.name == "subroute" && carriesWebhook(h.sub)) {
				return true
			}
		}
	}
	return false
}

// serverSettings is what of a server's own keys the workflow's
// connection meets before any route runs: the protocols its listeners
// serve, its listener wrappers, its TLS connection policies and its
// strict SNI check. Only these keys are encoded again and decoded into
// Caddy's shapes for them (its own type, for client authentication),
// so a value of another shape is the adaptation's fault, as in the
// routes; no other key of the server is read.
type serverSettings struct {
	Protocols        []string          `json:"protocols"`
	ListenProtocols  [][]string        `json:"listen_protocols"`
	ListenerWrappers []map[string]any  `json:"listener_wrappers"`
	StrictSNIHost    *bool             `json:"strict_sni_host"`
	RawPolicies      []json.RawMessage `json:"tls_connection_policies"`
	policies         []tlsPolicy
}

// tlsPolicy is one TLS connection policy: every key it sets, for the
// table's fail-closed default, and the values the table reads.
type tlsPolicy struct {
	keys                 map[string]json.RawMessage
	Match                map[string]any                 `json:"match"`
	ALPN                 []string                       `json:"alpn"`
	Drop                 bool                           `json:"drop"`
	ClientAuthentication *caddytls.ClientAuthentication `json:"client_authentication"`
}

func settingsOf(server any) (serverSettings, error) {
	m, _ := server.(map[string]any)
	keys := map[string]any{}
	for _, k := range []string{"protocols", "listen_protocols", "listener_wrappers", "strict_sni_host", "tls_connection_policies"} {
		if v, ok := m[k]; ok {
			keys[k] = v
		}
	}
	var s serverSettings
	b, err := json.Marshal(keys)
	if err != nil || json.Unmarshal(b, &s) != nil {
		return serverSettings{}, errAdaptation
	}
	for _, raw := range s.RawPolicies {
		var p tlsPolicy
		if json.Unmarshal(raw, &p.keys) != nil || json.Unmarshal(raw, &p) != nil {
			return serverSettings{}, errAdaptation
		}
		s.policies = append(s.policies, p)
	}
	return s, nil
}

// connection is the question asked before the routes, of the
// connection bin/apply's curl makes: HTTP/1.1 or HTTP/2 over TLS, both
// offered by ALPN, the host as its SNI (none for an IP address, to
// which curl sends none), no client certificate, no PROXY header. ""
// when it reaches the routes; otherwise the catalogue text of the first
// thing that refuses it, asked in the order of the tables: the
// listeners, the wrappers, the TLS policies, then the strict SNI check.
func (s serverSettings) connection(host string) string {
	served := map[string]bool{}
	for _, ps := range s.listeners() {
		if !slices.Contains(ps, "h1") && !slices.Contains(ps, "h2") {
			return "a listener's protocols include neither h1 nor h2"
		}
		for _, p := range ps {
			served[p] = true
		}
	}
	tlsAt := slices.IndexFunc(s.ListenerWrappers, func(w map[string]any) bool { return w["wrapper"] == "tls" })
	for i, w := range s.ListenerWrappers {
		name, _ := w["wrapper"].(string)
		fallback, _ := w["fallback_policy"].(string)
		switch {
		case name == "tls":
		case name == "http_redirect" && i < tlsAt:
			// Ahead of the handshake it answers only a connection whose
			// first bytes are plaintext HTTP. Behind it — where every
			// wrapper stands when tls is not listed — it reads the
			// request itself and redirects an HTTP/1.1 one.
		case name == "proxy_protocol" && !strings.EqualFold(fallback, "require"):
			// A connection without a PROXY header is served as sent
			// unless the policy for its address requires one; the
			// address is not the file's to know.
		default:
			return "the " + named(name) + " listener wrapper may refuse the connection"
		}
	}
	sni := host
	if net.ParseIP(host) != nil {
		sni = ""
	}
	for _, p := range s.policies {
		// Caddy hands the handshake to the first policy whose matchers
		// all match; one decided per connection may or may not be it,
		// so it is weighed and the next ones too.
		v := p.match(sni)
		if v.status == noMatch {
			continue
		}
		if what := p.block(served); what != "" {
			if v.status == mayMatch {
				return "a TLS connection policy that may apply to " + proof.Bound(host) + " " + what +
					" (its " + named(v.matcher) + " matcher is decided per connection)"
			}
			return "a TLS connection policy for " + proof.Bound(host) + " " + what
		}
		if v.status == matches {
			break
		}
	}
	// Caddy turns the check on, when the file leaves it unset, for a
	// server where any policy authenticates clients; it then refuses a
	// request whose SNI is not its Host.
	strict := slices.ContainsFunc(s.policies, func(p tlsPolicy) bool {
		return p.ClientAuthentication != nil && p.ClientAuthentication.Active()
	})
	if s.StrictSNIHost != nil {
		strict = *s.StrictSNIHost
	}
	if strict && sni != host {
		return "the server's strict_sni_host refuses a request to an IP address, which carries no SNI"
	}
	return ""
}

// named is a wrapper's, a matcher's or a setting's name for a
// refusal's text, bounded: never empty from Caddy, and "nameless" when
// other bytes give none, so the text still reads.
func named(s string) string {
	if s = proof.Bound(s); s == "" {
		return "nameless"
	}
	return s
}

// listeners is each listener's protocols as Caddy provisions them: the
// server's (h1, h2 and h3 when it lists none), unless listen_protocols
// gives the listener its own, in which "" stands for the server's.
func (s serverSettings) listeners() [][]string {
	server := s.Protocols
	if len(server) == 0 {
		server = []string{"h1", "h2", "h3"}
	}
	if s.ListenProtocols == nil {
		return [][]string{server}
	}
	out := make([][]string, 0, len(s.ListenProtocols))
	for _, ln := range s.ListenProtocols {
		switch {
		case len(ln) == 0:
			out = append(out, server)
		case slices.Contains(ln, ""):
			out = append(out, append(slices.Clone(ln), server...))
		default:
			out = append(out, ln)
		}
	}
	return out
}

// match is the policy's matchers asked of the workflow's ClientHello,
// whose SNI is sni: sni decided statically by Caddy's own type, any
// other matcher (remote_ip, local_ip, sni_regexp, one this code does
// not know) or a name holding a placeholder per connection, so "may".
// No matcher matches, as Caddy's catch-all policy does. Policies are
// walked in order: past 30 of them Caddy looks first at those naming
// the host exactly, which the adapter writes ahead of any other that
// matches it.
func (p tlsPolicy) match(sni string) matchVerdict {
	v := matchVerdict{status: matches}
	for _, k := range slices.Sorted(maps.Keys(p.Match)) {
		names, static := staticPatterns(p.Match[k])
		switch {
		case k == "sni" && static:
			if !caddytls.MatchServerName(names).Match(&tls.ClientHelloInfo{ServerName: sni}) {
				return matchVerdict{status: noMatch}
			}
		case v.status == matches:
			v = matchVerdict{status: mayMatch, matcher: k}
		}
	}
	return v
}

// block is the Reachability table of TLS policy settings: what the
// policy, when it applies to the box host, does to the workflow's
// handshake, served being the protocols the server's listeners serve,
// which bound what ALPN can agree on. "" when the handshake completes;
// a setting the table has not weighed may refuse it. A setting written
// as null is Caddy's zero value for it, as if absent.
func (p tlsPolicy) block(served map[string]bool) string {
	for _, k := range slices.Sorted(maps.Keys(p.keys)) {
		if string(p.keys[k]) == "null" {
			continue
		}
		switch k {
		case "match", "certificate_selection", "protocol_min", "protocol_max",
			"default_sni", "fallback_sni", "insecure_secrets_log":
		case "client_authentication":
			if requiresCertificate(p.ClientAuthentication) {
				return "requires a client certificate"
			}
		case "drop":
			if p.Drop {
				return "drops the connection"
			}
		case "alpn":
			// An empty list negotiates no protocol, and curl speaks
			// HTTP/1.1; otherwise Caddy takes out what no listener
			// serves.
			agrees := (len(p.ALPN) == 0 && served["h1"]) ||
				(slices.Contains(p.ALPN, "h2") && served["h2"]) ||
				(slices.Contains(p.ALPN, "http/1.1") && served["h1"])
			if !agrees {
				return "offers neither h2 nor http/1.1 by ALPN"
			}
		default:
			return "sets " + named(k) + ", which may refuse the handshake"
		}
	}
	return ""
}

// requiresCertificate is whether a client authentication refuses a
// client that sends no certificate: one Caddy counts as active, unless
// in mode request or verify_if_given with no verifier, since Caddy runs
// each verifier on every connection (trusted_leaf_certs becomes one,
// which fails a connection with no certificate).
func requiresCertificate(ca *caddytls.ClientAuthentication) bool {
	if ca == nil || !ca.Active() {
		return false
	}
	lets := (ca.Mode == "request" || ca.Mode == "verify_if_given") && len(ca.VerifiersRaw) == 0 &&
		len(ca.TrustedLeafCerts) == 0 //nolint:staticcheck // deprecated, but Caddy v2.11.7 still makes it a verifier
	return !lets
}

// matchStatus is what a matcher, a set or a route says of the request:
// it does not match, it may (decided per request), it does.
type matchStatus int

const (
	noMatch matchStatus = iota
	mayMatch
	matches
)

// matchVerdict is a matcher's, a set's or a route's answer: its status,
// the matcher it hangs on when that is mayMatch, and whether a matcher
// may fail at request time — Caddy then answers its error and routes no
// further, whatever stands behind.
type matchVerdict struct {
	status  matchStatus
	matcher string
	fails   bool
}

// reachEnd is how a walk of a route list ended: the routes ran out, the
// request met box_webhook, or something stood in the way.
type reachEnd int

const (
	fellThrough reachEnd = iota
	reachedWebhook
	blocked
)

// reachBlock is what stood in the way: a handler by module name, a
// terminal route that ran out of handlers (name ""; a site when at the
// server level), box_webhook itself behind a per-request matcher, or a
// matcher that may fail — under a match that is certain or, when may is
// set, hangs on matcher.
type reachBlock struct {
	name     string
	terminal bool
	site     bool
	changes  bool
	may      bool
	behind   bool
	fails    bool
	matcher  string
}

// text is the block's catalogue text, every input-derived value
// bounded. A handler's and a matcher's name are module names, never
// empty from Caddy; from any other bytes they may be, and the text
// still has to read.
func (b *reachBlock) text(host string) string {
	matcher := proof.Bound(b.matcher)
	if matcher == "" {
		matcher = "nameless"
	}
	if b.fails {
		return "the " + matcher + " matcher ahead of it may fail (decided per request)"
	}
	what := proof.Bound(b.name)
	switch {
	case b.terminal && b.site:
		what = "an earlier site for " + proof.Bound(host)
	case b.terminal:
		what = "a terminal route"
	case what == "":
		what = "a nameless handler"
	}
	switch {
	case b.behind:
		return what + " stands behind a " + matcher + " matcher decided per request"
	case b.may && b.changes:
		return what + " may change the request first (its " + matcher + " matcher is decided per request)"
	case b.may:
		return what + " may answer first (its " + matcher + " matcher is decided per request)"
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
// verdict outer, and ends at the first handler that decides. top is the
// server's own route list, whose routes are sites.
func (w *reachWalk) routes(rs []adaptedRoute, outer matchVerdict, top bool) (reachEnd, *reachBlock) {
	for _, r := range rs {
		// Caddy runs a route's matchers before its group check, so a
		// matcher that may fail fails whether or not the group is
		// already satisfied.
		v := w.match(r.match)
		if v.fails {
			return blocked, &reachBlock{fails: true, matcher: v.matcher}
		}
		if v.status == noMatch {
			continue
		}
		// The enclosing condition first: a member runs only when every
		// route around it matched, and satisfies its group only as far
		// as they did (Caddy's group map is one per request, across
		// nesting).
		if outer.status < v.status {
			v = outer
		}
		if r.group != "" {
			// The group records the member's own match under the
			// enclosing condition, not the downgrade an earlier "may"
			// member imposes on it: a member that matches for sure
			// satisfies the group whether or not an earlier one may
			// have, and later members never run.
			g := w.groups[r.group]
			if g.status == matches {
				continue
			}
			if v.status > g.status {
				w.groups[r.group] = groupState{v.status, v.matcher}
			}
			if g.status == mayMatch && v.status == matches {
				v = matchVerdict{status: mayMatch, matcher: g.matcher}
			}
		}
		may := v.status == mayMatch
		for _, h := range r.handle {
			switch classify(h) {
			case classReached:
				if may {
					// Reached for the requests the matcher admits; the
					// rest skip it, and whatever follows is not its
					// answer. No directive the walk accepts adapts to
					// this (box_webhook takes no matcher and its site
					// is a bare host); the contract is kept anyway.
					return blocked, &reachBlock{name: h.name, behind: true, matcher: v.matcher}
				}
				return reachedWebhook, nil
			case classSubroute:
				if end, b := w.routes(h.sub, v, false); end != fellThrough {
					return end, b
				}
			case classPasses:
			case classChanges:
				return blocked, &reachBlock{name: h.name, changes: true, may: may, matcher: v.matcher}
			default:
				return blocked, &reachBlock{name: h.name, may: may, matcher: v.matcher}
			}
		}
		if r.terminal {
			// Caddy hands the rest of the chain an empty handler: the
			// request ends here with whatever was written, or nothing.
			return blocked, &reachBlock{terminal: true, site: top, may: may, matcher: v.matcher}
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
	switch h.name {
	case "box_webhook":
		return classReached
	case "subroute":
		return classSubroute
	case "vars", "map", "log_append", "tracing", "request_body", "timeouts",
		"encode", "push", "hint_penaltybox":
		return classPasses
	case "headers":
		if h.request {
			return classChanges
		}
		return classPasses
	case "rewrite":
		return classChanges
	}
	return classAnswers
}

// match is a route's matcher sets, tried in order as Caddy tries them,
// until one matches: a set that matches for sure ends it (the sets
// after it never run); one that may fail ends it the other way; one
// that may match is remembered. No set is a match.
func (w *reachWalk) match(sets []map[string]any) matchVerdict {
	if len(sets) == 0 {
		return matchVerdict{status: matches}
	}
	best := matchVerdict{status: noMatch}
	for _, set := range sets {
		v := w.set(set)
		switch {
		case v.fails, v.status == matches:
			return v
		case v.status == mayMatch && best.status == noMatch:
			best = v
		}
	}
	return best
}

// set is one matcher set: every matcher must match. Caddy runs a set's
// matchers in an order fixed only at load, so one that may fail counts
// whatever the others say; otherwise one that does not match decides,
// and one that may, when none says no, leaves "may".
func (w *reachWalk) set(set map[string]any) matchVerdict {
	best := matchVerdict{status: matches}
	for _, k := range slices.Sorted(maps.Keys(set)) {
		v := w.one(k, set[k])
		switch {
		case v.fails:
			return v
		case v.status == noMatch:
			best = v
		case v.status == mayMatch && best.status == matches:
			best = v
		}
	}
	return best
}

// one is the second Reachability table: the five matchers decided
// statically with Caddy's own types; the matchers Caddy decides per
// request but never fails on, "may"; and the rest — `expression`,
// `file`, a matcher this walk does not know — may fail as well.
func (w *reachWalk) one(key string, v any) matchVerdict {
	may := matchVerdict{status: mayMatch, matcher: key}
	switch key {
	case "host":
		list, ok := staticPatterns(v)
		if !ok {
			return may
		}
		m := caddyhttp.MatchHost(list)
		if m.Provision(caddy.Context{}) != nil {
			return may
		}
		return decided(m.Match(w.req))
	case "path":
		list, ok := staticPatterns(v)
		if !ok {
			return may
		}
		m := caddyhttp.MatchPath(list)
		if m.Provision(caddy.Context{}) != nil {
			return may
		}
		return decided(m.Match(w.req))
	case "method":
		list, ok := staticPatterns(v)
		if !ok {
			return may
		}
		return decided(caddyhttp.MatchMethod(list).Match(w.req))
	case "protocol":
		// Only the scheme is the request's to know: http and https by
		// TLS, grpc by a content type the workflow never sends. An HTTP
		// version is the connection's, negotiated per request.
		switch proto, _ := v.(string); proto {
		case "http", "https", "grpc":
			return decided(caddyhttp.MatchProtocol(proto).Match(w.req))
		}
		return may
	case "not":
		list, ok := v.([]any)
		if !ok {
			return may
		}
		if len(list) == 0 {
			return matchVerdict{status: matches} // as Caddy: nothing negated
		}
		sets := make([]map[string]any, 0, len(list))
		for _, s := range list {
			set, ok := s.(map[string]any)
			if !ok {
				return may
			}
			sets = append(sets, set)
		}
		inner := w.match(sets)
		switch {
		case inner.fails:
			return matchVerdict{status: mayMatch, matcher: key, fails: true}
		case inner.status == matches:
			return matchVerdict{status: noMatch}
		case inner.status == noMatch:
			return matchVerdict{status: matches}
		}
		return may
	case "header", "header_regexp", "query", "path_regexp", "vars", "vars_regexp", "tls",
		// remote_ip and client_ip fail only while a TLS handshake is
		// incomplete: 0-RTT early data, which a push never sends.
		"remote_ip", "client_ip":
		return may
	}
	may.fails = true
	return may
}

// staticPatterns reads a list of patterns none of which holds a
// placeholder.
func staticPatterns(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	list := make([]string, 0, len(items))
	for _, item := range items {
		p, ok := item.(string)
		if !ok || strings.Contains(p, "{") {
			return nil, false
		}
		list = append(list, p)
	}
	return list, true
}

func decided(matched bool) matchVerdict {
	if matched {
		return matchVerdict{status: matches}
	}
	return matchVerdict{status: noMatch}
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
