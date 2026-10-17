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
		reachFile(nil, "", "\ttimeouts {\n\t\tread_timeout 1s 65537\n\t}\n", ""),
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
	// The request limits, in both of a duration's shapes, request_body's
	// two settings and timeouts' two.
	f.Add(connJSON(`"read_header_timeout":"1s","max_header_bytes":7999,`), "deploy.example.com", "")
	f.Add(connJSON(`"read_timeout":1000000000,"read_min_rate":65537,`), "deploy.example.com", "")
	f.Add(reachJSON(`{"handle":[{"handler":"request_body","set":"x","max_size":1}]},`, ""), "deploy.example.com", "")
	f.Add(reachJSON(`{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"request_body","max_size":1048576}]},`, ""), "deploy.example.com", "")
	f.Add(reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":10000000000,"read_min_rate":65537}]},`, ""), "deploy.example.com", "")
	f.Add(reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"timeouts","read_timeout":1}]},`, ""), "deploy.example.com", "")
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
		first := reach(adapted, host, d)
		again := reach(adapted, host, d)
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
// connection policy and what it does, a request limit under its floor
// — each limit's text in full, since the floors are fixed words), or
// what stands in the way of the request (a module name, with its
// setting for request_body and timeouts, an earlier site, a terminal
// route) and one of the six verbs.
var catalogueReach = regexp.MustCompile(`^(` + regexp.QuoteMeta(msgListenMoved) + `|` +
	`the new Caddyfile does not reach box_webhook on /: (` +
	`a listener's protocols include neither h1 nor h2|` +
	`the .+ listener wrapper may refuse the connection|` +
	`the server's strict_sni_host refuses a request to an IP address, which carries no SNI|` +
	`the server's max_header_bytes is under 8 KB, the floor for a push's headers and token|` +
	`the server's read_header_timeout is under 5s, the floor for a push's handshake and headers|` +
	`the server's read_timeout is under 30s, the floor for a push's upload|` +
	`the server's read_idle_timeout is under 10s, the floor for a push's upload|` +
	`the server's read_min_rate is over 64 KiB/s, the ceiling for a push's upload|` +
	`the server's write_timeout is under 5s, the floor for a push's handshake|` +
	`a TLS connection policy for .* ` + policyWhat + `|` +
	`a TLS connection policy that may apply to .* ` + policyWhat + ` \(its .+ matcher is decided per connection\)|` +
	`(.+) (answers first|changes the request first|caps the body under 16 MiB first|cuts ` + cutWhat + ` first|may (answer|change the request|cap the body under 16 MiB|cut ` + cutWhat + `) first \(its .+ matcher is decided per request\)|stands behind a .+ matcher decided per request|may fail \(decided per request\))))$`)

const cutWhat = `an upload (that stalls under 10s|under 64 KiB/s)`

const policyWhat = `(requires a client certificate|drops the connection|offers neither h2 nor http/1\.1 by ALPN|sets .+, which may refuse the handshake)`
