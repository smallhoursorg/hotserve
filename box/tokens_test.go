package box

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	"golang.org/x/crypto/ssh"
)

// keys are two real ed25519 public keys for signer lines.
var keys = func() [2]string {
	var out [2]string
	for i := range out {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		k, err := ssh.NewPublicKey(pub)
		if err != nil {
			panic(err)
		}
		out[i] = base64.StdEncoding.EncodeToString(k.Marshal())
	}
	return out
}()

// file is a Caddyfile from a template in which K1 and K2 are the keys.
func file(s string) []byte {
	return []byte(strings.NewReplacer("K1", keys[0], "K2", keys[1]).Replace(s))
}

const good = `{
	admin unix//run/hotserve/admin.sock
	email {$ACME_EMAIL:ops@example.com}

	box {
		deploy_trust github {
			audience hotserve
			claim repository your-org/boxes
		}
		# signers
		signer alice@example.com ssh-ed25519 K1
		signer bob ssh-ed25519 K2
	}

	liveswap {
		app example {
			command node server.js
		}
	}
}

example.com {
	reverse_proxy {
		dynamic liveswap example
	}
}

deploy.example.com {
	liveswap_webhook
	box_webhook
}
`

func TestWalkGood(t *testing.T) {
	s, err := Walk(file(good))
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "deploy.example.com" || len(s.Signers) != 2 || s.Signers[0].Principal != "alice@example.com" || s.Signers[1].Principal != "bob" || s.Signers[0].B64 != keys[0] {
		t.Fatalf("%+v", s)
	}
}

func TestWalkAccepts(t *testing.T) {
	for name, in := range map[string]string{
		"host upper-cased":         strings.Replace(good, "deploy.example.com {", "Deploy.Example.COM {", 1),
		"webhook nested":           strings.Replace(good, "\tbox_webhook\n", "\troute {\n\t\tbox_webhook\n\t}\n", 1),
		"webhook twice":            strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook\n\tbox_webhook\n", 1),
		"matcher":                  strings.Replace(good, "\tbox_webhook\n", "\t@m {\n\t\tpath /x\n\t}\n\trespond @m ok\n\tbox_webhook\n", 1),
		"placeholder value":        strings.Replace(good, "\tbox_webhook\n", "\trespond {$MSG:hi}\n\tbox_webhook\n", 1),
		"heredoc value":            strings.Replace(good, "\tbox_webhook\n", "\trespond <<EOF\n\thello\n\timport nothing\n\tEOF 200\n\tbox_webhook\n", 1),
		"quoted multiline":         strings.Replace(good, "\tbox_webhook\n", "\trespond \"a\nimport b\n\"\n\tbox_webhook\n", 1),
		"nested close on the line": strings.Replace(good, "\tbox_webhook\n", "\troute {\n\t\tbox_webhook }\n", 1),
		"empty box_webhook":        strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook {\n\t}\n", 1),
		"comments":                 strings.Replace(good, "\tbox_webhook\n", "\t# import x\n\tbox_webhook # not import\n", 1),
		"single label host":        strings.Replace(good, "deploy.example.com {", "localhost {", 1),
		"ip host":                  strings.Replace(good, "deploy.example.com {", "192.0.2.1 {", 1),
		"braced empty site":        good + "\nother.example.com {\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Walk(file(in))
			if err != nil {
				t.Fatal(err)
			}
			if s.Host == "" || len(s.Signers) != 2 {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestWalkRefuses(t *testing.T) {
	noKey := "\t\tsigner alice ssh-ed25519 K1\n"
	for name, c := range map[string]struct{ in, want string }{
		"empty":                   {"", "is empty"},
		"whitespace":              {"\n\t\n", "is empty"},
		"comment only":            {"# nothing\n", "has no box block"},
		"no global block":         {"deploy.example.com {\n\tbox_webhook\n}\n", "has no box block"},
		"global without box":      {"{\n\tadmin off\n}\ndeploy.example.com {\n\tbox_webhook\n}\n", "has no box block"},
		"box in a site":           {"{\n}\ndeploy.example.com {\n\tbox {\n" + noKey + "\t}\n\tbox_webhook\n}\n", "has no box block"},
		"two boxes":               {strings.Replace(good, "\tbox {\n", "\tbox {\n\t}\n\tbox {\n", 1), "has more than one box block"},
		"two global blocks":       {"{\n}\n" + good, "has more than one global options block"},
		"no signer":               {strings.Replace(strings.Replace(good, "\t\tsigner alice@example.com ssh-ed25519 K1\n", "", 1), "\t\tsigner bob ssh-ed25519 K2\n", "", 1), "has no signer"},
		"box empty":               {strings.Replace(good, "\tbox {\n", "\tbox {\n\t}\n\tbox_ {\n", 1), "has no signer"},
		"box without a block":     {strings.Replace(good, "\tbox {\n", "\tbox\n\tbox_ {\n", 1), "has no signer"},
		"no deploy_trust":         {strings.Replace(good, "\t\tdeploy_trust github {\n\t\t\taudience hotserve\n\t\t\tclaim repository your-org/boxes\n\t\t}\n", "", 1), "has a box block with no deploy_trust"},
		"empty deploy_trust":      {strings.Replace(good, "\t\tdeploy_trust github {\n\t\t\taudience hotserve\n\t\t\tclaim repository your-org/boxes\n\t\t}\n", "\t\tdeploy_trust github {\n\t\t}\n", 1), "has a box block with no deploy_trust"},
		"signer two args":         {strings.Replace(good, "signer bob ssh-ed25519 K2", "signer bob K2", 1), "has a signer line that is not `signer <principal> <key-type> <base64>`"},
		"signer four args":        {strings.Replace(good, "signer bob ssh-ed25519 K2", "signer bob ssh-ed25519 K2 extra", 1), "has a signer line that is not"},
		"signer with a block":     {strings.Replace(good, "signer bob ssh-ed25519 K2", "signer bob ssh-ed25519 K2 {\n\t\t}", 1), "has a signer line that is not"},
		"signer bad principal":    {strings.Replace(good, "signer bob ", "signer b*b ", 1), "has a bad signer principal \"b*b\" may only contain"},
		"signer bad type":         {strings.Replace(good, "signer bob ssh-ed25519", "signer bob ssh-dss", 1), "has a bad signer bob: key type \"ssh-dss\" is not one of"},
		"signer bad key":          {strings.Replace(good, "ssh-ed25519 K2", "ssh-ed25519 AAAA", 1), "has a bad signer bob: the key does not parse"},
		"signer same key twice":   {strings.Replace(good, "ssh-ed25519 K2", "ssh-ed25519 K1", 1), "has a bad signer alice@example.com and signer bob are the same key"},
		"no webhook":              {strings.Replace(good, "\tbox_webhook\n", "", 1), "has no site with box_webhook"},
		"webhook in snippet":      {strings.Replace(good, "\tbox_webhook\n", "", 1) + "(s) {\n\tbox_webhook\n}\n", "has no site with box_webhook"},
		"two webhook sites":       {good + "\nother.example.com {\n\tbox_webhook\n}\n", "has more than one site with box_webhook"},
		"two addresses":           {strings.Replace(good, "deploy.example.com {", "deploy.example.com, deploy2.example.com {", 1), "has a box_webhook site whose address is not one bare hostname (deploy.example.com, deploy2.example.com); no scheme, port, path, wildcard, placeholder or second name"},
		"continued addresses":     {strings.Replace(good, "deploy.example.com {", "deploy.example.com,\ndeploy2.example.com {", 1), "not one bare hostname (deploy.example.com, deploy2.example.com)"},
		"scheme":                  {strings.Replace(good, "deploy.example.com {", "https://deploy.example.com {", 1), "not one bare hostname (https://deploy.example.com)"},
		"port":                    {strings.Replace(good, "deploy.example.com {", "deploy.example.com:8443 {", 1), "not one bare hostname (deploy.example.com:8443)"},
		"path":                    {strings.Replace(good, "deploy.example.com {", "deploy.example.com/hook {", 1), "not one bare hostname"},
		"wildcard":                {strings.Replace(good, "deploy.example.com {", "*.example.com {", 1), "not one bare hostname (*.example.com)"},
		"port only":               {strings.Replace(good, "deploy.example.com {", ":443 {", 1), "not one bare hostname (:443)"},
		"trailing dot":            {strings.Replace(good, "deploy.example.com {", "deploy.example.com. {", 1), "not one bare hostname"},
		"hyphen label":            {strings.Replace(good, "deploy.example.com {", "-deploy.example.com {", 1), "not one bare hostname"},
		"underscore":              {strings.Replace(good, "deploy.example.com {", "deploy_1.example.com {", 1), "not one bare hostname"},
		"placeholder address":     {strings.Replace(good, "deploy.example.com {", "{$DEPLOY_HOST} {", 1), "has a placeholder where a directive name, a site address or a box line goes ({$DEPLOY_HOST}); placeholders are for values"},
		"placeholder in address":  {strings.Replace(good, "deploy.example.com {", "deploy.{$DOMAIN}.com {", 1), "has a placeholder where"},
		"placeholder directive":   {strings.Replace(good, "\tbox_webhook\n", "\t{$DIRECTIVE:respond} ok\n\tbox_webhook\n", 1), "has a placeholder where a directive name, a site address or a box line goes ({$DIRECTIVE:respond})"},
		"placeholder option":      {strings.Replace(good, "\tadmin unix", "\t{$ADMIN:admin} unix", 1), "has a placeholder where"},
		"placeholder signer":      {strings.Replace(good, "ssh-ed25519 K2", "ssh-ed25519 {$BOB_KEY}", 1), "has a placeholder where a directive name, a site address or a box line goes ({$BOB_KEY})"},
		"placeholder in trust":    {strings.Replace(good, "audience hotserve", "audience {$AUD}", 1), "has a placeholder where"},
		"placeholder box line":    {strings.Replace(good, "\t\t# signers\n", "\t\t{$X:signer} a b c\n", 1), "has a placeholder where"},
		"import top":              {"import snippets/*\n" + good, "imports snippets/*; the box applies only a self-contained Caddyfile — inline the snippet"},
		"import in global":        {strings.Replace(good, "\tadmin unix", "\timport common\n\tadmin unix", 1), "imports common;"},
		"import in box":           {strings.Replace(good, "\t\t# signers\n", "\t\timport signers.caddy\n", 1), "imports signers.caddy;"},
		"import in site":          {strings.Replace(good, "\tbox_webhook\n", "\timport tls-snippet\n\tbox_webhook\n", 1), "imports tls-snippet;"},
		"import nested":           {strings.Replace(good, "\tbox_webhook\n", "\troute {\n\t\timport inner arg\n\t}\n\tbox_webhook\n", 1), "imports inner;"},
		"import bare":             {strings.Replace(good, "\tbox_webhook\n", "\timport\n\tbox_webhook\n", 1), "imports; the box applies"},
		"import quoted":           {strings.Replace(good, "\tbox_webhook\n", "\t\"import\" x\n\tbox_webhook\n", 1), "imports x;"},
		"import after comma":      {strings.Replace(good, "deploy.example.com {", "deploy.example.com,\nimport x\n{", 1), "imports x;"},
		"import snippet body":     {"(s) {\n\timport x\n}\n" + good, "imports x;"},
		"unclosed":                {strings.TrimSuffix(good, "}\n"), "does not parse: a block is not closed"},
		"stray close":             {good + "}\n", "does not parse: a } with no block open"},
		"braceless site":          {good + "\nother.example.com\n", "has a site written without braces (other.example.com); every site block in the signed file must be braced"},
		"braceless site body":     {strings.Replace(good, "deploy.example.com {\n\tliveswap_webhook\n\tbox_webhook\n}\n", "deploy.example.com\nliveswap_webhook\nbox_webhook\n", 1), "has a site written without braces (deploy.example.com)"},
		"braceless continued":     {good + "\na.com,\nb.com\n", "has a site written without braces (a.com, b.com)"},
		"site closed on a line":   {strings.Replace(good, "\tbox_webhook\n}\n", "\tbox_webhook }\n", 1), "does not parse: a } that closes a top-level block on a directive's line"},
		"global closed on a line": {strings.Replace(good, "\t}\n}\n\nexample.com", "\t} }\n\nexample.com", 1), "does not parse: a } that closes a top-level block"},
		"token after brace":       {strings.Replace(good, "deploy.example.com {", "deploy.example.com { box_webhook", 1), "does not parse"},
		"comma in address":        {strings.Replace(good, "deploy.example.com {", "a.com,b.com {", 1), "does not parse: a comma inside a site address"},
		"trailing comma":          {good + "\na.com,\n", "does not parse: a site address list ends with a comma"},
		"bad lexing":              {strings.Replace(good, "\tbox_webhook\n", "\trespond <<EOF\n\tnever closed\n\tbox_webhook\n", 1), "does not tokenize"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Walk(file(c.in))
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("want a refusal containing %q, got %v", c.want, err)
			}
			if !strings.Contains(r.Reason, c.want) {
				t.Fatalf("got %q, want it to contain %q", r.Reason, c.want)
			}
			if !strings.HasPrefix(err.Error(), "the Caddyfile ") {
				t.Fatal(err)
			}
		})
	}
}

// TestWalkSeesThroughPlaceholderQuotes is the case the second reading
// exists for: on the raw bytes, `import evil` sits inside a quoted
// value and nothing is at directive position; after Caddy expands the
// two defaults — each a lone quote — the quoting flips and `import
// evil` is a line of its own. Caddy's own parser confirms it would
// import.
func TestWalkSeesThroughPlaceholderQuotes(t *testing.T) {
	in := file(strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook\n\trespond {$HOTSERVE_BOX_TEST_UNSET_Q:\"} \"a\nimport evil\" {$HOTSERVE_BOX_TEST_UNSET_R:\"}\n", 1))
	// The raw reading alone is fooled.
	if _, err := walk(in); err != nil {
		t.Fatalf("the raw reading should pass: %v", err)
	}
	_, err := Walk(in)
	var r *Refusal
	if !errors.As(err, &r) || !strings.HasPrefix(r.Reason, "imports ") {
		t.Fatalf("got %v", err)
	}
	unsetForTest(t, "HOTSERVE_BOX_TEST_UNSET_Q", "HOTSERVE_BOX_TEST_UNSET_R")
	if _, err := caddyfile.Parse("Caddyfile", in); err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("Caddy's parser should try the import: %v", err)
	}
}

// TestExpandEmptyEnvPinsCaddy holds expandEmptyEnv to caddyfile.Parse:
// for an input whose placeholder names are unset, the tokens Parse
// makes are the tokens of the input expanded here, braces aside.
func TestExpandEmptyEnvPinsCaddy(t *testing.T) {
	const a, b = "HOTSERVE_BOX_TEST_UNSET_A", "HOTSERVE_BOX_TEST_UNSET_B"
	unsetForTest(t, a, b)
	for _, in := range []string{
		"x {$" + a + ":dflt} y",
		"{$" + b + "} x",
		"{$} y {$:z}",
		"x {$" + a + ":a:b} {$" + a + ":}",
		"x {$" + a + ":{$" + b + "}} y",
		"x {$" + a,
		"x {$" + a + ":\"} \"a\nb\" {$" + b + ":\"}",
		"example.com {\n\trespond {$" + a + ":hello world} {$" + b + "}\n\theader {$" + a + ":X-A} {$" + b + ":B}\n}\n",
		"{\n\temail {$" + a + ":a@example.com}\n}\n",
	} {
		blocks, err := caddyfile.Parse("Caddyfile", []byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		var want []string
		for _, sb := range blocks {
			want = append(want, sb.GetKeysText()...)
			for _, seg := range sb.Segments {
				for _, tok := range seg {
					want = append(want, tok.Text)
				}
			}
		}
		toks, err := caddyfile.Tokenize(expandEmptyEnv([]byte(in)), "Caddyfile")
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		var got []string
		for _, tok := range toks {
			got = append(got, tok.Text)
		}
		if strings.Join(strip(want), "\x00") != strings.Join(strip(got), "\x00") {
			t.Fatalf("%q:\nParse:    %q\nexpanded: %q", in, want, got)
		}
		// And the expansion did something, where there was something.
		if strings.Contains(in, ":") {
			raw, _ := caddyfile.Tokenize([]byte(in), "Caddyfile")
			var rawTexts []string
			for _, tok := range raw {
				rawTexts = append(rawTexts, tok.Text)
			}
			if strings.Join(strip(rawTexts), "\x00") == strings.Join(strip(got), "\x00") {
				t.Fatalf("%q: nothing expanded", in)
			}
		}
	}
	// A placeholder's value may hold more placeholders, which are not
	// expanded again (Caddy's one-level chaining), and the input is
	// not mutated.
	in := []byte("{$" + a + ":{$" + b + ":x}}")
	if got := string(expandEmptyEnv(in)); got != "{$"+b+":x}" || string(in) != "{$"+a+":{$"+b+":x}}" {
		t.Fatal(got, string(in))
	}
}

func strip(texts []string) []string {
	var out []string
	for _, s := range texts {
		if s != "{" && s != "}" {
			out = append(out, s)
		}
	}
	return out
}

// unsetForTest fails if a name the test relies on being unset is set.
func unsetForTest(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, set := os.LookupEnv(n); set {
			t.Fatalf("%s is set in the environment", n)
		}
	}
}

func TestBareHost(t *testing.T) {
	for in, want := range map[string]string{
		"deploy.example.com":    "deploy.example.com",
		"Deploy.Example.COM":    "deploy.example.com",
		"localhost":             "localhost",
		"192.0.2.1":             "192.0.2.1",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"a-b.c-d":               "a-b.c-d",
	} {
		got, ok := BareHost([]string{in})
		if !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", ".", "a.", ".a", "a..b", "-a", "a-", "a_b", "a b", "a:1", "a/b", "*.a", "{$A}", "https://a", "é.com", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 127) + "com"} {
		if _, ok := BareHost([]string{bad}); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := BareHost([]string{"a.com", "b.com"}); ok {
		t.Error("two accepted")
	}
	if _, ok := BareHost(nil); ok {
		t.Error("none accepted")
	}
}

func FuzzWalk(f *testing.F) {
	f.Add(file(good))
	f.Add(file(strings.Replace(good, "\tbox_webhook\n", "\trespond {$X:\"} \"a\nimport evil\" {$Y:\"}\n\tbox_webhook\n", 1)))
	f.Add([]byte("import x\n"))
	f.Add([]byte("{\n\tbox {\n\t\tsigner a b c\n\t}\n}\n"))
	f.Add([]byte("a.com,\nb.com {\n\tbox_webhook\n}\n"))
	f.Add([]byte("respond <<EOF\nx\nEOF\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		s, err := Walk(in)
		if err != nil {
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("not a refusal: %v", err)
			}
			return
		}
		if _, ok := BareHost([]string{s.Host}); !ok || s.Host != strings.ToLower(s.Host) || len(s.Signers) == 0 {
			t.Fatalf("%+v", s)
		}
		if _, err := s.Signers.AllowedSigners(); err != nil {
			t.Fatal(err)
		}
	})
}
