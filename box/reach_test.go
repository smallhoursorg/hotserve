package box

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// reachFile assembles a Caddyfile around the design's example box site
// (DESIGN-box.md, "The shape"): the fixture's global block with `order`
// lines added, the example app site, then before, the box site with
// inside added to it, and after. Position inside a site decides
// nothing: Caddy orders directives by its list, so `inside` may name a
// matcher and use it wherever it stands.
func reachFile(order []string, before, inside, after string) string {
	global := good[:strings.Index(good, "\n}\n")+3]
	if len(order) > 0 {
		var b strings.Builder
		for _, o := range order {
			b.WriteString("\torder " + o + "\n")
		}
		global = strings.Replace(global, "{\n", "{\n"+b.String(), 1)
	}
	site := "deploy.example.com {\n" + inside + "\tliveswap_webhook\n\tbox_webhook\n}\n"
	return global + "\nexample.com {\n\treverse_proxy {\n\t\tdynamic liveswap example\n\t}\n}\n\n" + before + site + after
}

// reachOf adapts a Caddyfile and asks the question of its adaptation.
func reachOf(t *testing.T, in string) error {
	t.Helper()
	cfg, _, err := adapt(t, in)
	if err != nil {
		t.Fatalf("adapt: %v\n%s", err, in)
	}
	return reach(cfg, "deploy.example.com")
}

// wantReach checks a verdict: "" is reachable; otherwise the exact
// refusal text, as the catalogue has it.
func wantReach(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		return
	}
	var r *refusal
	if !errors.As(err, &r) {
		t.Fatalf("got %v, want the refusal %q", err, want)
	}
	if r.msg != msgUnreachable(want) {
		t.Fatalf("got  %q\nwant %q", r.msg, msgUnreachable(want))
	}
}

// TestReach is the Reachability tables (DESIGN-box.md) on adapted
// Caddyfiles: what the default order lets stand before box_webhook,
// what an `order` option or a second site for the host puts in front,
// and how each matcher is decided.
func TestReach(t *testing.T) {
	const (
		respondFirst = "static_response answers first"
		mayHeader    = "static_response may answer first (its header matcher is decided per request)"
	)
	for name, c := range map[string]struct {
		order                 []string
		before, inside, after string
		want                  string
	}{
		// Reachable.
		"the design's example":                  {},
		"the directives a box site might carry": {inside: "\trespond /x ok\n\thandle /h {\n\t\trespond h\n\t}\n\troute /r {\n\t\trespond r\n\t}\n\tredir /old /new\n\treverse_proxy /p localhost:9\n"},
		"a response header":                     {inside: "\theader X-Frame-Options DENY\n"},
		"another path on the host":              {before: "deploy.example.com/api/* {\n\trespond api\n}\n"},
		"a wildcard host":                       {after: "*.example.com {\n\trespond wild\n}\n"},
		"a longer host":                         {before: "xdeploy.example.com {\n\trespond other\n}\n"},
		"an ordered responder on another path":  {order: []string{"respond first"}, inside: "\trespond /x ok\n"},
		"not path /":                            {order: []string{"respond first"}, inside: "\t@m not path /\n\trespond @m ok\n"},
		"protocol http":                         {order: []string{"respond first"}, inside: "\t@m protocol http\n\trespond @m ok\n"},
		"method PUT":                            {order: []string{"respond first"}, inside: "\t@m method PUT\n\trespond @m ok\n"},
		"path and method that both decide no":   {order: []string{"respond first"}, inside: "\t@m path /x\n\trespond @m ok\n\t@n method DELETE\n\trespond @n ok\n"},
		"a handle group, the first member matched and passes": {order: []string{"handle first"},
			inside: "\thandle /* {\n\t\theader X-A b\n\t}\n\thandle {\n\t\trespond no\n\t}\n"},
		// Member 1 may match, member 2 matches for sure: either way the
		// group is satisfied before member 3, which Caddy never runs.
		"a handle group satisfied by its second member": {order: []string{"handle first"},
			inside: "\t@m header X-A b\n\thandle @m {\n\t\theader X-B c\n\t}\n\thandle /* {\n\t\theader X-C d\n\t}\n\thandle {\n\t\trespond no\n\t}\n"},
		"an ordered handle on another path": {order: []string{"handle first"}, inside: "\thandle /x {\n\t\trespond no\n\t}\n"},
		"a header matcher on a handler that passes": {order: []string{"header first"},
			inside: "\t@m header X-A b\n\theader @m X-B c\n"},

		// Refused: a second site for the host, sorted ahead whatever
		// the file order.
		"a site for host/* before":                 {before: "deploy.example.com/* {\n\trespond x\n}\n", want: respondFirst},
		"a site for host/* after":                  {after: "deploy.example.com/* {\n\trespond x\n}\n", want: respondFirst},
		"an empty site for host/":                  {before: "deploy.example.com/ {\n}\n", want: "an earlier site for deploy.example.com answers first"},
		"a site for host/ that only sets a header": {before: "deploy.example.com/ {\n\theader X-A b\n}\n", want: "an earlier site for deploy.example.com answers first"},

		// Refused: an `order` option moved a responder ahead.
		"an ordered responder": {order: []string{"respond first"}, inside: "\trespond ok\n", want: respondFirst},
		"method GET":           {order: []string{"respond first"}, inside: "\t@m method GET\n\trespond @m ok\n", want: respondFirst},
		"method POST":          {order: []string{"respond first"}, inside: "\t@m method POST\n\trespond @m ok\n", want: respondFirst},
		"protocol http/2":      {order: []string{"respond first"}, inside: "\t@m protocol http/2\n\trespond @m ok\n", want: "static_response may answer first (its protocol matcher is decided per request)"},
		"protocol http/1.1":    {order: []string{"respond first"}, inside: "\t@m protocol http/1.1\n\trespond @m ok\n", want: "static_response may answer first (its protocol matcher is decided per request)"},
		// A second block naming the bare host is "ambiguous site
		// definition" to the adapter (validate refuses it first); one
		// naming it with the port is another address string, the same
		// host matcher, and sorts by file order.
		"a site naming the host with its port before": {before: "deploy.example.com:443 {\n\trespond x\n}\n", want: respondFirst},
		"a site naming the host with its port after":  {after: "deploy.example.com:443 {\n\trespond x\n}\n"},
		"protocol https":        {order: []string{"respond first"}, inside: "\t@m protocol https\n\trespond @m ok\n", want: respondFirst},
		"path /*":               {order: []string{"respond first"}, inside: "\t@m path /*\n\trespond @m ok\n", want: respondFirst},
		"not path /x":           {order: []string{"respond first"}, inside: "\t@m not path /x\n\trespond @m ok\n", want: respondFirst},
		"a reverse_proxy":       {order: []string{"reverse_proxy first"}, inside: "\treverse_proxy localhost:9\n", want: "reverse_proxy answers first"},
		"liveswap_webhook":      {order: []string{"liveswap_webhook first"}, want: "liveswap_webhook answers first"},
		"a handle that answers": {order: []string{"handle first"}, inside: "\thandle {\n\t\trespond no\n\t}\n", want: respondFirst},
		"a route that answers":  {order: []string{"route first"}, inside: "\troute {\n\t\trespond no\n\t}\n", want: respondFirst},
		// Two handle blocks with the same (no) matcher adapt to ONE
		// grouped route holding both subroutes, so the second runs
		// after the first passes: Caddy's consolidation, mirrored.
		"two matcherless handles": {order: []string{"handle first"}, inside: "\thandle {\n\t\theader X-A b\n\t}\n\thandle {\n\t\trespond no\n\t}\n", want: respondFirst},
		"a handle group whose first member may have matched": {order: []string{"handle first"},
			inside: "\t@m header X-A b\n\thandle @m {\n\t\theader X-B c\n\t}\n\thandle {\n\t\trespond no\n\t}\n", want: mayHeader},

		// Refused: the request changed before box_webhook sees it.
		"a rewrite":                        {order: []string{"rewrite first"}, inside: "\trewrite * /x\n", want: "rewrite changes the request first"},
		"a uri rewrite":                    {order: []string{"uri first"}, inside: "\turi strip_prefix /x\n", want: "rewrite changes the request first"},
		"a request header":                 {order: []string{"request_header first"}, inside: "\trequest_header -Authorization\n", want: "headers changes the request first"},
		"a rewrite under a header matcher": {order: []string{"rewrite first"}, inside: "\t@m header X-A b\n\trewrite @m /x\n", want: "rewrite may change the request first (its header matcher is decided per request)"},

		// Refused: a matcher decided per request.
		"a header matcher":      {order: []string{"respond first"}, inside: "\t@m header X-A b\n\trespond @m ok\n", want: mayHeader},
		"an expression matcher": {order: []string{"respond first"}, inside: "\t@m expression {method} == \"GET\"\n\trespond @m ok\n", want: "the expression matcher ahead of it may fail (decided per request)"},
		// A matcher that may fail refuses whatever stands behind it:
		// Caddy answers its error and routes no further.
		"an expression matcher on a handler that passes": {order: []string{"header first"}, inside: "\t@m expression {method} == \"GET\"\n\theader @m X-B c\n", want: "the expression matcher ahead of it may fail (decided per request)"},
		"not over an expression":                         {order: []string{"respond first"}, inside: "\t@m not expression {method} == \"GET\"\n\trespond @m ok\n", want: "the not matcher ahead of it may fail (decided per request)"},
		"a query matcher":                                {order: []string{"respond first"}, inside: "\t@m query x=1\n\trespond @m ok\n", want: "static_response may answer first (its query matcher is decided per request)"},
		"a placeholder in a path":                        {order: []string{"respond first"}, inside: "\t@m path /{http.request.uri.query.x}\n\trespond @m ok\n", want: "static_response may answer first (its path matcher is decided per request)"},
		"not over a header":                              {order: []string{"respond first"}, inside: "\t@m not header X-A b\n\trespond @m ok\n", want: "static_response may answer first (its not matcher is decided per request)"},
		"a decided path beside an undecided header": {order: []string{"respond first"},
			inside: "\t@m path /x\n\t@n header X-A b\n\trespond @m ok\n\trespond @n ok\n", want: mayHeader},
	} {
		t.Run(name, func(t *testing.T) {
			wantReach(t, reachOf(t, reachFile(c.order, c.before, c.inside, c.after)), c.want)
		})
	}
}

// reachJSON is an adaptation by hand: one server, the box site's route
// as Caddy emits it, with routes before box_webhook inside it.
func reachJSON(before string, siteBefore string) []byte {
	return []byte(`{"apps":{"http":{"servers":{"srv0":{"listen":[":443"],"routes":[` + siteBefore +
		`{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[` + before +
		`{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`)
}

// TestReachJSON is the rows the adapter cannot be made to produce in
// this module's tests: a handler the table names that no registered
// directive adapts to here, one it does not name, and the shapes of an
// adaptation the box itself must not trust blindly.
func TestReachJSON(t *testing.T) {
	for name, c := range map[string]struct {
		adapted []byte
		want    string // "" reachable; "error" a plain error; else the refusal
	}{
		"the penaltybox hint ahead":                     {adapted: reachJSON(`{"handle":[{"handler":"hint_penaltybox"}]},`, ""), want: ""},
		"vars, map, log and tracing ahead":              {adapted: reachJSON(`{"handle":[{"handler":"vars"},{"handler":"map"},{"handler":"log_append"},{"handler":"tracing"},{"handler":"timeouts"},{"handler":"push"},{"handler":"encode"},{"handler":"request_body"}]},`, ""), want: ""},
		"templates ahead":                               {adapted: reachJSON(`{"handle":[{"handler":"templates","mime_types":["application/json"]}]},`, ""), want: "templates answers first"},
		"templates ahead with its default types":        {adapted: reachJSON(`{"handle":[{"handler":"templates"}]},`, ""), want: "templates answers first"},
		"intercept ahead":                               {adapted: reachJSON(`{"handle":[{"handler":"intercept","handle_response":[{"match":{"status_code":[2]},"routes":[{"handle":[{"handler":"static_response","body":"maintenance"}]}]}]}]},`, ""), want: "intercept answers first"},
		"a response headers handler":                    {adapted: reachJSON(`{"handle":[{"handler":"headers","response":{"set":{"X-A":["b"]}}}]},`, ""), want: ""},
		"a request headers handler":                     {adapted: reachJSON(`{"handle":[{"handler":"headers","request":{"delete":["Authorization"]}}]},`, ""), want: "headers changes the request first"},
		"a handler the table does not name":             {adapted: reachJSON(`{"handle":[{"handler":"cache"}]},`, ""), want: "cache answers first"},
		"invoke":                                        {adapted: reachJSON(`{"handle":[{"handler":"invoke","name":"r"}]},`, ""), want: "invoke answers first"},
		"an undecided matcher on a handler that passes": {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"encode"}]},`, ""), want: ""},
		"an undecided matcher on a responder":           {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its remote_ip matcher is decided per request)"},
		"two sets, one decided no and one undecided":    {adapted: reachJSON(`{"match":[{"path":["/x"]},{"header":{"X-A":["b"]}}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its header matcher is decided per request)"},
		"two sets, one decided yes":                     {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}},{"path":["/"]}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response answers first"},
		// Sets run in order until one matches: a set that matches for
		// sure shields the sets after it, not the ones before.
		"a failing set after one that matches":                       {adapted: reachJSON(`{"match":[{"path":["/"]},{"expression":{"expr":"true"}}],"handle":[{"handler":"vars"}]},`, ""), want: ""},
		"a failing set before one that matches":                      {adapted: reachJSON(`{"match":[{"expression":{"expr":"true"}},{"path":["/"]}],"handle":[{"handler":"vars"}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		"a failing matcher beside a decided no in one set":           {adapted: reachJSON(`{"match":[{"path":["/x"],"expression":{"expr":"true"}}],"handle":[{"handler":"vars"}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		"a file matcher on a handler that passes":                    {adapted: reachJSON(`{"match":[{"file":{"try_files":["{path}"]}}],"handle":[{"handler":"encode"}]},`, ""), want: "the file matcher ahead of it may fail (decided per request)"},
		"a matcher the table does not know on a handler that passes": {adapted: reachJSON(`{"match":[{"geoip":{"country":["GB"]}}],"handle":[{"handler":"encode"}]},`, ""), want: "the geoip matcher ahead of it may fail (decided per request)"},
		"a failing matcher on a route a group already skips":         {adapted: reachJSON(`{"group":"g","handle":[{"handler":"vars"}]},{"group":"g","match":[{"expression":{"expr":"true"}}],"handle":[{"handler":"vars"}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		// Caddy's group map is one per request across nesting: a member
		// under a per-request outer route satisfies its group only for
		// the requests that outer route admits, so a sibling of the
		// group outside it still runs for the rest. (Caddy's adapter
		// never shares a group across levels; the contract is kept for
		// any bytes.)
		"a group member under an undecided outer route": {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"subroute","routes":[{"group":"g","handle":[{"handler":"vars"}]}]}]},{"group":"g","handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its header matcher is decided per request)"},
		"a group member under a decided outer route":    {adapted: reachJSON(`{"match":[{"path":["/"]}],"handle":[{"handler":"subroute","routes":[{"group":"g","handle":[{"handler":"vars"}]}]}]},{"group":"g","handle":[{"handler":"static_response"}]},`, ""), want: ""},
		"a query matcher on a handler that passes":      {adapted: reachJSON(`{"match":[{"query":{"x":["1"]}}],"handle":[{"handler":"encode"}]},`, ""), want: ""},
		"an empty not": {adapted: reachJSON(`{"match":[{"not":[]}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response answers first"},
		"a terminal route inside the site that passes":            {adapted: reachJSON(`{"handle":[{"handler":"vars"}],"terminal":true},`, ""), want: "a terminal route answers first"},
		"a terminal route under an undecided matcher":             {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"vars"}],"terminal":true},`, ""), want: "a terminal route may answer first (its header matcher is decided per request)"},
		"an earlier site under an undecided matcher":              {adapted: reachJSON("", `{"match":[{"host":["deploy.example.com"],"header":{"X-A":["b"]}}],"handle":[],"terminal":true},`), want: "an earlier site for deploy.example.com may answer first (its header matcher is decided per request)"},
		"an earlier site on another host":                         {adapted: reachJSON("", `{"match":[{"host":["other.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true},`), want: ""},
		"an earlier site with a host placeholder":                 {adapted: reachJSON("", `{"match":[{"host":["{http.request.host}"]}],"handle":[{"handler":"static_response"}],"terminal":true},`), want: "static_response may answer first (its host matcher is decided per request)"},
		"a responder in a server that does not carry the webhook": {adapted: []byte(`{"apps":{"http":{"servers":{"other":{"listen":[":8443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true}]},"srv0":{"listen":[":443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`), want: ""},
		"another handler's routes key, of another shape":          {adapted: reachJSON(`{"handle":[{"handler":"vars","routes":{"a":1}}]},`, ""), want: ""},
		"a subroute whose routes do not read":                     {adapted: reachJSON(`{"handle":[{"handler":"subroute","routes":{"a":1}}]},`, ""), want: "error"},
		// The webhook itself under a per-request matcher: reached for
		// some requests only, so not reached. Nothing the walk accepts
		// adapts to this; the contract is kept anyway.
		"the webhook behind a header matcher":               {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"box_webhook"}]},{"handle":[{"handler":"static_response"}]}]}],"terminal":true}]}}}}}`), want: "box_webhook stands behind a header matcher decided per request"},
		"the webhook in a subroute behind a header matcher": {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}]}]}],"terminal":true}]}}}}}`), want: "box_webhook stands behind a header matcher decided per request"},
		"not JSON":                    {adapted: []byte("{"), want: "error"},
		"no box_webhook":              {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"static_response"}]}]}}}}}`), want: "error"},
		"the webhook on another host": {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["other.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`), want: "error"},
	} {
		t.Run(name, func(t *testing.T) {
			err := reach(c.adapted, "deploy.example.com")
			if c.want == "error" {
				var r *refusal
				if err == nil || errors.As(err, &r) {
					t.Fatalf("got %v, want the box's own error", err)
				}
				return
			}
			wantReach(t, err, c.want)
		})
	}
}

// TestReachIsCaseInsensitive: the walk lower-cases the host; Caddy's
// host matcher does too, so a site written in capitals is still the
// box site, and still shadowed by one with a path.
func TestReachHostCase(t *testing.T) {
	in := strings.Replace(reachFile(nil, "Deploy.Example.com/* {\n\trespond x\n}\n", "", ""), "deploy.example.com {\n", "DEPLOY.example.com {\n", 1)
	wantReach(t, reachOf(t, in), "static_response answers first")
}

// TestReachDeepNesting: the adaptation is converted in one pass, so a
// chain of nested subroutes as deep as Go's JSON decoder allows is
// walked without re-reading the subtree at every level.
func TestReachDeepNesting(t *testing.T) {
	const depth = 2000 // four JSON levels a step; encoding/json stops at 10000
	var b strings.Builder
	b.WriteString(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[`)
	for range depth {
		b.WriteString(`{"handler":"subroute","routes":[{"handle":[`)
	}
	b.WriteString(`{"handler":"box_webhook"}`)
	for range depth {
		b.WriteString(`]}]}`)
	}
	b.WriteString(`],"terminal":true}]}}}}}`)
	start := time.Now()
	if err := reach([]byte(b.String()), "deploy.example.com"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}
