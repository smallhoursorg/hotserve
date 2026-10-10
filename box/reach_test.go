package box

import (
	"errors"
	"strings"
	"testing"
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
		"an ordered responder":  {order: []string{"respond first"}, inside: "\trespond ok\n", want: respondFirst},
		"method GET":            {order: []string{"respond first"}, inside: "\t@m method GET\n\trespond @m ok\n", want: respondFirst},
		"method POST":           {order: []string{"respond first"}, inside: "\t@m method POST\n\trespond @m ok\n", want: respondFirst},
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
		"a rewrite":        {order: []string{"rewrite first"}, inside: "\trewrite * /x\n", want: "rewrite changes the request first"},
		"a uri rewrite":    {order: []string{"uri first"}, inside: "\turi strip_prefix /x\n", want: "rewrite changes the request first"},
		"a request header": {order: []string{"request_header first"}, inside: "\trequest_header -Authorization\n", want: "headers changes the request first"},

		// Refused: a matcher decided per request.
		"a header matcher":        {order: []string{"respond first"}, inside: "\t@m header X-A b\n\trespond @m ok\n", want: mayHeader},
		"an expression matcher":   {order: []string{"respond first"}, inside: "\t@m expression {method} == \"GET\"\n\trespond @m ok\n", want: "static_response may answer first (its expression matcher is decided per request)"},
		"a query matcher":         {order: []string{"respond first"}, inside: "\t@m query x=1\n\trespond @m ok\n", want: "static_response may answer first (its query matcher is decided per request)"},
		"a placeholder in a path": {order: []string{"respond first"}, inside: "\t@m path /{http.request.uri.query.x}\n\trespond @m ok\n", want: "static_response may answer first (its path matcher is decided per request)"},
		"not over a header":       {order: []string{"respond first"}, inside: "\t@m not header X-A b\n\trespond @m ok\n", want: "static_response may answer first (its not matcher is decided per request)"},
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
		"the penaltybox hint ahead":                               {adapted: reachJSON(`{"handle":[{"handler":"hint_penaltybox"}]},`, ""), want: ""},
		"vars, map, log and tracing ahead":                        {adapted: reachJSON(`{"handle":[{"handler":"vars"},{"handler":"map"},{"handler":"log_append"},{"handler":"tracing"},{"handler":"timeouts"},{"handler":"push"},{"handler":"intercept"},{"handler":"encode"},{"handler":"templates"},{"handler":"request_body"}]},`, ""), want: ""},
		"a response headers handler":                              {adapted: reachJSON(`{"handle":[{"handler":"headers","response":{"set":{"X-A":["b"]}}}]},`, ""), want: ""},
		"a request headers handler":                               {adapted: reachJSON(`{"handle":[{"handler":"headers","request":{"delete":["Authorization"]}}]},`, ""), want: "headers changes the request first"},
		"a handler the table does not name":                       {adapted: reachJSON(`{"handle":[{"handler":"cache"}]},`, ""), want: "cache answers first"},
		"invoke":                                                  {adapted: reachJSON(`{"handle":[{"handler":"invoke","name":"r"}]},`, ""), want: "invoke answers first"},
		"an undecided matcher on a handler that passes":           {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"encode"}]},`, ""), want: ""},
		"an undecided matcher on a responder":                     {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its remote_ip matcher is decided per request)"},
		"two sets, one decided no and one undecided":              {adapted: reachJSON(`{"match":[{"path":["/x"]},{"header":{"X-A":["b"]}}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its header matcher is decided per request)"},
		"two sets, one decided yes":                               {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}},{"path":["/"]}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response answers first"},
		"an empty not":                                            {adapted: reachJSON(`{"match":[{"not":[]}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response answers first"},
		"a terminal route inside the site that passes":            {adapted: reachJSON(`{"handle":[{"handler":"vars"}],"terminal":true},`, ""), want: "an earlier site for deploy.example.com answers first"},
		"an earlier site on another host":                         {adapted: reachJSON("", `{"match":[{"host":["other.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true},`), want: ""},
		"an earlier site with a host placeholder":                 {adapted: reachJSON("", `{"match":[{"host":["{http.request.host}"]}],"handle":[{"handler":"static_response"}],"terminal":true},`), want: "static_response may answer first (its host matcher is decided per request)"},
		"a responder in a server that does not carry the webhook": {adapted: []byte(`{"apps":{"http":{"servers":{"other":{"listen":[":8443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true}]},"srv0":{"listen":[":443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`), want: ""},
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
