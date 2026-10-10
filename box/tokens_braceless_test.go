package box

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// TestBracelessSiteIsOneBlockToCaddy pins the premise behind the
// refusal of a site written without braces: Caddy reads the lines
// that follow a bare address as that site's directives, at what a
// depth walk would call depth zero. So `box_webhook` on its own line
// below `deploy.example.com` is a directive to Caddy and an address
// to the walk, which is why the walk refuses the shape outright
// rather than guess.
func TestBracelessSiteIsOneBlockToCaddy(t *testing.T) {
	in := "{\n\tadmin off\n}\n\ndeploy.example.com\nliveswap_webhook\nbox_webhook\n"
	blocks, err := caddyfile.Parse("Caddyfile", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("%d blocks", len(blocks))
	}
	site := blocks[1]
	if keys := site.GetKeysText(); len(keys) != 1 || keys[0] != "deploy.example.com" || site.HasBraces {
		t.Fatalf("keys %q braces %v", keys, site.HasBraces)
	}
	var names []string
	for _, seg := range site.Segments {
		names = append(names, seg[0].Text)
	}
	if strings.Join(names, " ") != "liveswap_webhook box_webhook" {
		t.Fatalf("directives %q", names)
	}
	// And the braced form is the same site to Caddy.
	braced, err := caddyfile.Parse("Caddyfile", []byte("{\n\tadmin off\n}\n\ndeploy.example.com {\n\tliveswap_webhook\n\tbox_webhook\n}\n"))
	if err != nil || len(braced) != 2 || len(braced[1].Segments) != 2 || !braced[1].HasBraces {
		t.Fatalf("%v %v", braced, err)
	}
}
