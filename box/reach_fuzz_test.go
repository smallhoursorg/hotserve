package box

import (
	"errors"
	"regexp"
	"testing"
)

// FuzzReach: the reachability walk over any bytes said to be an
// adaptation and any host. It never panics, answers the same twice, and
// a refusal is always one of the catalogue's reachability texts. The
// seeds are real adaptations of the table's shapes and hand-written
// ones, so the corpus starts on both sides of the verdict.
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
		f.Add(cfg, "deploy.example.com")
		f.Add(cfg, "other.example.com")
	}
	f.Add(reachJSON(`{"match":[{"not":[{"path":["/"]}]}],"handle":[{"handler":"static_response"}]},`, ""), "deploy.example.com")
	f.Add(reachJSON(`{"handle":[{"handler":"headers","request":{}}]},`, ""), "deploy.example.com")
	f.Add(connJSON(`"protocols":["h3"],`), "deploy.example.com")
	f.Add(connJSON(`"listener_wrappers":[{"wrapper":"proxy_protocol","fallback_policy":"REQUIRE"},{"wrapper":"tls"}],`), "deploy.example.com")
	f.Add(connJSON(`"tls_connection_policies":[{"match":{"remote_ip":{}}},{"match":{"sni":["*.example.com"]},"client_authentication":{"mode":"require"}}],`), "deploy.example.com")
	f.Add([]byte("{"), "x")
	f.Add([]byte(`{"apps":{"http":{"servers":{"a":{"routes":[{"match":[{"host":["{x}"]}],"handle":[{"handler":"box_webhook"}],"group":"g","terminal":true}]}}}}}`), "h")
	f.Fuzz(func(t *testing.T, adapted []byte, host string) {
		first := reach(adapted, host)
		again := reach(adapted, host)
		if (first == nil) != (again == nil) || (first != nil && first.Error() != again.Error()) {
			t.Fatalf("two answers: %v then %v", first, again)
		}
		var r *refusal
		if errors.As(first, &r) && !catalogueReach.MatchString(r.msg) {
			t.Fatalf("a refusal outside the catalogue: %q", r.msg)
		}
	})
}

// catalogueReach is the 422 row's reachability texts: what refuses the
// connection (the protocols, a listener wrapper, a TLS connection
// policy and what it does), or what stands in the way of the request
// (a module name, an earlier site, a terminal route) and one of the
// four verbs.
var catalogueReach = regexp.MustCompile(`^the new Caddyfile does not reach box_webhook on /: (` +
	`a listener's protocols include neither h1 nor h2|` +
	`the .+ listener wrapper may refuse the connection|` +
	`a TLS connection policy for .+ ` + policyWhat + `|` +
	`a TLS connection policy that may apply to .+ ` + policyWhat + ` \(its .+ matcher is decided per connection\)|` +
	`(.+) (answers first|changes the request first|may (answer|change the request) first \(its .+ matcher is decided per request\)|stands behind a .+ matcher decided per request|may fail \(decided per request\)))$`)

const policyWhat = `(requires a client certificate|drops the connection|offers neither h2 nor http/1\.1 by ALPN|sets .+, which may refuse the handshake)`
