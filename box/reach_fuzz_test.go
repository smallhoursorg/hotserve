package box

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// FuzzReach: the reachability walk over any bytes said to be an
// adaptation, any host and any door (its addresses separated by
// spaces; none is no door). It never panics, answers the same twice,
// and a refusal is always one of the catalogue's reachability texts,
// with no line break in it. The seeds are real adaptations of the
// table's shapes and hand-written ones, so the corpus starts on both
// sides of the verdict.
func FuzzReach(f *testing.F) {
	for _, in := range []string{
		reachFile(nil, "", "", ""),
		reachFile(nil, "deploy.example.com/* {\n\trespond x\n}\n", "", ""),
		reachFile([]string{"respond first"}, "", "\t@m header X-A b\n\trespond @m ok\n", ""),
		reachFile([]string{"handle first"}, "", "\thandle {\n\t\theader X-A b\n\t}\n\thandle {\n\t\trespond no\n\t}\n", ""),
		reachFile([]string{"rewrite first"}, "", "\trewrite * /x\n", ""),
	} {
		cfg, _, err := adapt(f, in)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(cfg, "deploy.example.com", "")
		f.Add(cfg, "deploy.example.com", ":443")
		f.Add(cfg, "deploy.example.com", ":8443")
		f.Add(cfg, "other.example.com", "")
	}
	f.Add(reachJSON(`{"match":[{"not":[{"path":["/"]}]}],"handle":[{"handler":"static_response"}]},`, ""), "deploy.example.com", "")
	f.Add(reachJSON(`{"handle":[{"handler":"headers","request":{}}]},`, ""), "deploy.example.com", "")
	f.Add(connJSON(`"protocols":["h3"],`), "deploy.example.com", "")
	f.Add(connJSON(`"listener_wrappers":[{"wrapper":"proxy_protocol","fallback_policy":"REQUIRE"},{"wrapper":"tls"}],`), "deploy.example.com", "")
	f.Add(connJSON(`"tls_connection_policies":[{"match":{"remote_ip":{}}},{"match":{"sni":["*.example.com"]},"client_authentication":{"mode":"require"}}],`), "deploy.example.com", "")
	f.Add(connJSON(`"strict_sni_host":true,`), "203.0.113.5", "")
	// The door: kept in another spelling, moved, a placeholder, a port
	// range, an address that does not parse, a listen of another shape.
	f.Add(connJSON(""), "deploy.example.com", "tcp/:443 :443")
	f.Add(connJSON(""), "deploy.example.com", "127.0.0.1:443 [::1]:443")
	f.Add(connJSON(""), "deploy.example.com", "{env.HOME}")
	f.Add(connJSON(""), "deploy.example.com", ":443-444")
	f.Add(connJSON(""), "deploy.example.com", "x:y")
	f.Add([]byte(strings.Replace(string(connJSON("")), `"listen":[":443"],`, `"listen":":443",`, 1)), "deploy.example.com", ":443")
	f.Add([]byte(strings.Replace(string(connJSON("")), `"listen":[":443"],`, `"listen":[":443\n"],`, 1)), "deploy.example.com", ":443")
	// Names the bytes leave empty: the host (the walk never gives one),
	// a setting, a matcher.
	f.Add(connJSON(`"tls_connection_policies":[{"drop":true}],`), "", "")
	f.Add(connJSON(`"tls_connection_policies":[{"":1}],`), "deploy.example.com", "")
	f.Add(connJSON(`"tls_connection_policies":[{"match":{"":1},"drop":true}],`), "deploy.example.com", "")
	f.Add([]byte("{"), "x", "")
	f.Add([]byte(`{"apps":{"http":{"servers":{"a":{"routes":[{"match":[{"host":["{x}"]}],"handle":[{"handler":"box_webhook"}],"group":"g","terminal":true}]}}}}}`), "h", "")
	f.Fuzz(func(t *testing.T, adapted []byte, host, door string) {
		var d []string
		if door != "" {
			d = strings.Split(door, " ")
		}
		first := reach(adapted, host, doorOf(d))
		again := reach(adapted, host, doorOf(d))
		if (first == nil) != (again == nil) || (first != nil && first.Error() != again.Error()) {
			t.Fatalf("two answers: %v then %v", first, again)
		}
		var r *refusal
		if errors.As(first, &r) && (!catalogueReach.MatchString(r.msg) || strings.ContainsAny(r.msg, "\n\r")) {
			t.Fatalf("a refusal outside the catalogue: %q", r.msg)
		}
	})
}

// catalogueReach is the 422 rows' reachability texts: a moved door, or
// what refuses the connection (the protocols, a listener wrapper, a TLS
// connection policy and what it does), or what stands in the way of the
// request (a module name, an earlier site, a terminal route) and one of
// the four verbs.
var catalogueReach = regexp.MustCompile(`^(this push changes where the box's server listens \(.+ → .+\): a push cannot move the address it arrives on; make that change at the console|` +
	`the new Caddyfile does not reach box_webhook on /: (` +
	`a listener's protocols include neither h1 nor h2|` +
	`the .+ listener wrapper may refuse the connection|` +
	`the server's strict_sni_host refuses a request to an IP address, which carries no SNI|` +
	`a TLS connection policy for .* ` + policyWhat + `|` +
	`a TLS connection policy that may apply to .* ` + policyWhat + ` \(its .+ matcher is decided per connection\)|` +
	`(.+) (answers first|changes the request first|may (answer|change the request) first \(its .+ matcher is decided per request\)|stands behind a .+ matcher decided per request|may fail \(decided per request\))))$`)

const policyWhat = `(requires a client certificate|drops the connection|offers neither h2 nor http/1\.1 by ALPN|sets .+, which may refuse the handshake)`
