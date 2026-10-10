package box

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// TestWalkQuotedBracesAreValues is the review's case: a quoted `{` in
// one site and a quoted `}` in the next would, to a walk that read
// braces by text alone, open a phantom block in the first site and
// swallow the second into it — balanced, accepted, and `box_webhook`
// attributed to the wrong host. Caddy's parser reads structural
// braces only when unquoted, and so must the walk: the host is the
// second site's, and Caddy agrees there are two sites.
func TestWalkQuotedBracesAreValues(t *testing.T) {
	in := file(strings.Replace(good,
		"example.com {\n\treverse_proxy {\n\t\tdynamic liveswap example\n\t}\n}\n",
		"example.com {\n\trespond \"{\"\n}\n", 1))
	in = []byte(strings.Replace(string(in), "\tbox_webhook\n}\n", "\tbox_webhook\n\trespond \"}\"\n}\n", 1))
	s, err := Walk(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "deploy.example.com" {
		t.Fatalf("host %q", s.Host)
	}
	blocks, err := caddyfile.Parse("Caddyfile", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 || blocks[1].GetKeysText()[0] != "example.com" || blocks[2].GetKeysText()[0] != "deploy.example.com" {
		t.Fatalf("%d blocks", len(blocks))
	}
	// And the lexer's flag is what tells them apart.
	toks, err := caddyfile.Tokenize([]byte("a \"{\" {\n}\n"), "Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	if isOpen(toks[1]) || !isOpen(toks[2]) || !isClose(toks[3]) {
		t.Fatalf("%+v", toks)
	}
}
