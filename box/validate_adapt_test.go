package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unreachableAdaptation routes / on the box site to a responder before
// box_webhook; BODY is where a test puts an expanded value.
const unreachableAdaptation = `{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"static_response","body":"BODY"}]},{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`

// TestValidateAdapt is the adapt child (DESIGN-box.md, step 6 and the
// Caps rows "the adapt child" and "the adaptation"): run after validate
// passed and before the backup child, its stdout read to the cap and
// walked, its stderr dropped, its exit the box's error.
func TestValidateAdapt(t *testing.T) {
	file := []byte("deploy.example.com {\n\tbox_webhook\n}\n")
	t.Run("an unreachable site refuses, and the backup child does not run", func(t *testing.T) {
		c, dir := stubChildren(t, "exit 0", "printf '%s' '"+unreachableAdaptation+"'; exit 0", "exit 0")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil)
		if err != nil || msg != msgUnreachable("static_response answers first") {
			t.Fatalf("msg %q err %v", msg, err)
		}
		if exists(filepath.Join(dir, "hotserve-backup.args")) {
			t.Fatal("backup validate ran after the refusal")
		}
	})
	t.Run("the adaptation's values stay out of the refusal", func(t *testing.T) {
		secret := "pg-pass-" + strings.Repeat("q7", 10)
		before, after, _ := strings.Cut(unreachableAdaptation, "BODY")
		c, _ := stubAdapt(t, "printf '%s' '"+before+"'; printf '%s' \"$DB_PASS\"; printf '%s' '"+after+"'; exit 0", "DB_PASS="+secret)
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil)
		if err != nil || strings.Contains(msg, secret) || msg != msgUnreachable("static_response answers first") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a moved door refuses, an address the file does not have unwritten", func(t *testing.T) {
		// The adapter expanded `bind {$BIND_ADDR}` into the listen list;
		// the pushed file does not have the address, so the refusal names
		// the move without it.
		addr := "198.51.100.77:443"
		withListen := strings.Replace(reachableAdaptation, `"srv0":{`, `"srv0":{"listen":["'"$BIND_ADDR"'"],`, 1)
		c, dir := stubChildren(t, "exit 0", "printf '%s' '"+withListen+"'; exit 0", "exit 0", "BIND_ADDR="+addr)
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, &door{listen: []string{":443"}})
		if err != nil || msg != msgListenMoved([]string{":443"}, []string{msgUnwritten}, nil, nil) {
			t.Fatalf("msg %q err %v", msg, err)
		}
		if exists(filepath.Join(dir, "hotserve-backup.args")) {
			t.Fatal("backup validate ran after the refusal")
		}
		// The same door, kept: admitted, and nothing quoted.
		c, _ = stubChildren(t, "exit 0", "printf '%s' '"+withListen+"'; exit 0", "exit 0", "BIND_ADDR="+addr)
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, &door{listen: []string{addr}}); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		// Written in the pushed file: quoted as it is; the loaded one,
		// written in the running file: the same.
		c, _ = stubChildren(t, "exit 0", "printf '%s' '"+withListen+"'; exit 0", "exit 0", "BIND_ADDR="+addr)
		written := []byte("deploy.example.com {\n\tbind 198.51.100.77\n}\n")
		msg, err = c.validate(context.Background(), randomID(t), written, stubHost, &door{listen: []string{"10.0.0.5:443"}, running: []byte("bind 10.0.0.5")})
		if err != nil || msg != "this push changes where the box's server listens (10.0.0.5:443 → 198.51.100.77:443): a push cannot move the address it arrives on; make that change at the console" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	// The adapter splices `{$BIND_ADDRS}` into the file before lexing
	// it, so the real adaptation lists what the value held, in Caddy's
	// spelling, and the pushed file has none of it: every form of the
	// value, the quoting context included, is unwritten.
	for name, value := range map[string]string{
		"a value of two addresses":            "198.51.100.77 198.51.100.78",
		"a value with whitespace around it":   " 198.51.100.77\t",
		"a quoted value":                      `"198.51.100.77"`,
		"a value of two quoted addresses":     `"198.51.100.77" "198.51.100.78"`,
		"a value of two backticked addresses": "`198.51.100.77` `198.51.100.78`",
		"a value with its network":            "tcp/198.51.100.77",
	} {
		t.Run("a moved door from "+name+", unwritten", func(t *testing.T) {
			t.Setenv("BIND_ADDRS", value)
			in := reachFile(nil, "", "\tbind {$BIND_ADDRS}\n", "")
			cfg, _, err := adapt(t, in)
			if err != nil || !strings.Contains(string(cfg), `"198.51.100.77:443"`) {
				t.Fatalf("adapt: %v\n%s", err, cfg)
			}
			c, _ := stubChildren(t, "exit 0", "printf '%s' '"+string(cfg)+"'; exit 0", "exit 0", "BIND_ADDRS="+value)
			msg, err := c.validate(context.Background(), randomID(t), []byte(in), "deploy.example.com", &door{listen: []string{":443"}})
			if err != nil || strings.Contains(msg, "198.51") || !strings.HasPrefix(msg, "this push changes where the box's server listens (:443 → "+msgUnwritten) {
				t.Fatalf("msg %q err %v", msg, err)
			}
		})
	}
	t.Run("a moved door from a quoted placeholder in the file, unwritten", func(t *testing.T) {
		// `bind "{$BIND_ADDRS}"` with a value that closes and reopens the
		// quotes: two addresses to the adapter, neither in the file.
		const value = `198.51.100.77" "198.51.100.78`
		t.Setenv("BIND_ADDRS", value)
		in := reachFile(nil, "", "\tbind \"{$BIND_ADDRS}\"\n", "")
		cfg, _, err := adapt(t, in)
		if err != nil || !strings.Contains(string(cfg), `"198.51.100.78:443"`) {
			t.Fatalf("adapt: %v\n%s", err, cfg)
		}
		c, _ := stubChildren(t, "exit 0", "printf '%s' '"+string(cfg)+"'; exit 0", "exit 0", "BIND_ADDRS="+value)
		msg, err := c.validate(context.Background(), randomID(t), []byte(in), "deploy.example.com", &door{listen: []string{":443"}})
		if err != nil || strings.Contains(msg, "198.51") || !strings.HasPrefix(msg, "this push changes where the box's server listens (:443 → "+msgUnwritten) {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a moved door from a listen placeholder, unwritten", func(t *testing.T) {
		// A `{env.*}` placeholder in a listen address is replaced in the
		// serving process (listenOf), the whole address from the value.
		const value = "tcp/198.51.100.77:443"
		t.Setenv("BOX_TEST_LISTEN", value)
		withPlaceholder := strings.Replace(reachableAdaptation, `"srv0":{`, `"srv0":{"listen":["{env.BOX_TEST_LISTEN}"],`, 1)
		c, _ := stubChildren(t, "exit 0", "printf '%s' '"+withPlaceholder+"'; exit 0", "exit 0", "BOX_TEST_LISTEN="+value)
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, &door{listen: []string{":443"}})
		if err != nil || msg != msgListenMoved([]string{":443"}, []string{msgUnwritten}, nil, nil) {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a reachable site passes, then the backup child runs", func(t *testing.T) {
		c, dir := stubChildren(t, "exit 0", "printf '%s' '"+reachableAdaptation+"'; exit 0", "exit 0")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		if !exists(filepath.Join(dir, "hotserve-backup.args")) {
			t.Fatal("backup validate did not run")
		}
	})
	t.Run("stderr is dropped", func(t *testing.T) {
		c, _ := stubAdapt(t, "echo '{\"level\":\"warn\",\"msg\":\"Using a path in a site address is deprecated\"}' >&2; printf '%s' '"+reachableAdaptation+"'; exit 0")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("past the cap refuses, and the child is not stalled on its pipe", func(t *testing.T) {
		c, _ := stubAdapt(t, "head -c 17000000 /dev/zero | tr '\\0' 'x'; exit 0")
		start := time.Now()
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil)
		if err != nil || msg != msgAdaptationTooLarge || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("an exit is the box's, its words kept out", func(t *testing.T) {
		c, _ := stubAdapt(t, "echo 'Error: DB_PASS is not a number' >&2; exit 1")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil)
		if msg != "" || err == nil || err.Error() != "hotserve adapt: exit status 1" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("an adaptation that does not read is the box's", func(t *testing.T) {
		c, _ := stubAdapt(t, "printf '{'; exit 0")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil)
		if msg != "" || err == nil || !strings.HasPrefix(err.Error(), "hotserve adapt: the adaptation does not parse") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("the host asked for is the walk's", func(t *testing.T) {
		c, _ := stubAdapt(t, "printf '%s' '"+reachableAdaptation+"'; exit 0")
		msg, err := c.validate(context.Background(), randomID(t), file, "other.example.com", nil)
		if msg != "" || err == nil || !strings.HasPrefix(err.Error(), "hotserve adapt: the adaptation routes GET / on other.example.com past box_webhook") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a grandchild holding the output after a pass", func(t *testing.T) {
		c, _ := stubAdapt(t, "printf '%s' '"+reachableAdaptation+"'; sleep 60 & exit 0")
		start := time.Now()
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil); msg != "" || err != nil || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("a child that does not finish is the push's", func(t *testing.T) {
		c, _ := stubAdapt(t, "exec sleep 60")
		c.timeout = 200 * time.Millisecond
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil)
		if err != nil || msg != "hotserve adapt: did not finish within 200ms" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("not after validate refused", func(t *testing.T) {
		c, dir := stubChildren(t, "echo 'Error: no'; exit 1", "printf '%s' '"+reachableAdaptation+"'; exit 0", "")
		if msg, _ := c.validate(context.Background(), randomID(t), file, stubHost, nil); msg != "hotserve validate: no" {
			t.Fatalf("msg %q", msg)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "hotserve.args"))
		if strings.Count(string(args), "\n") != 1 || !strings.HasPrefix(string(args), "validate ") {
			t.Fatalf("the program ran more than validate: %q", args)
		}
	})
}

// TestCappedBuffer: the head kept to the limit, every byte counted.
func TestCappedBuffer(t *testing.T) {
	c := newCappedBuffer(5)
	for _, w := range []string{"ab", "cd", "ef", "gh"} {
		if n, err := c.Write([]byte(w)); n != 2 || err != nil {
			t.Fatalf("Write(%q) = %d, %v", w, n, err)
		}
	}
	if string(c.b) != "abcde" || !c.over() || c.n != 8 {
		t.Fatalf("kept %q over %v n %d", c.b, c.over(), c.n)
	}
	exact := newCappedBuffer(2)
	_, _ = exact.Write([]byte("xy"))
	if string(exact.b) != "xy" || exact.over() {
		t.Fatalf("kept %q over %v", exact.b, exact.over())
	}
}
