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
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost)
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
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost)
		if err != nil || strings.Contains(msg, secret) || msg != msgUnreachable("static_response answers first") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a reachable site passes, then the backup child runs", func(t *testing.T) {
		c, dir := stubChildren(t, "exit 0", "printf '%s' '"+reachableAdaptation+"'; exit 0", "exit 0")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		if !exists(filepath.Join(dir, "hotserve-backup.args")) {
			t.Fatal("backup validate did not run")
		}
	})
	t.Run("stderr is dropped", func(t *testing.T) {
		c, _ := stubAdapt(t, "echo '{\"level\":\"warn\",\"msg\":\"Using a path in a site address is deprecated\"}' >&2; printf '%s' '"+reachableAdaptation+"'; exit 0")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("past the cap refuses, without waiting for the child", func(t *testing.T) {
		c, _ := stubAdapt(t, "head -c 17000000 /dev/zero | tr '\\0' 'x'; exit 0")
		start := time.Now()
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost)
		if err != nil || msg != msgAdaptationTooLarge || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("an exit is the box's, its words kept out", func(t *testing.T) {
		c, _ := stubAdapt(t, "echo 'Error: DB_PASS is not a number' >&2; exit 1")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost)
		if msg != "" || err == nil || err.Error() != "hotserve adapt: exit status 1" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("an adaptation that does not read is the box's", func(t *testing.T) {
		c, _ := stubAdapt(t, "printf '{'; exit 0")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost)
		if msg != "" || err == nil || !strings.HasPrefix(err.Error(), "hotserve adapt: the adaptation does not parse") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("the host asked for is the walk's", func(t *testing.T) {
		c, _ := stubAdapt(t, "printf '%s' '"+reachableAdaptation+"'; exit 0")
		msg, err := c.validate(context.Background(), randomID(t), file, "other.example.com")
		if msg != "" || err == nil || !strings.HasPrefix(err.Error(), "hotserve adapt: the adaptation routes GET / on other.example.com past box_webhook") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a grandchild holding the output after a pass", func(t *testing.T) {
		c, _ := stubAdapt(t, "printf '%s' '"+reachableAdaptation+"'; sleep 60 & exit 0")
		start := time.Now()
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost); msg != "" || err != nil || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("a child that does not finish is the push's", func(t *testing.T) {
		c, _ := stubAdapt(t, "exec sleep 60")
		c.timeout = 200 * time.Millisecond
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost)
		if err != nil || msg != "hotserve adapt: did not finish within 200ms" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("not after validate refused", func(t *testing.T) {
		c, dir := stubChildren(t, "echo 'Error: no'; exit 1", "printf '%s' '"+reachableAdaptation+"'; exit 0", "")
		if msg, _ := c.validate(context.Background(), randomID(t), file, stubHost); msg != "hotserve validate: no" {
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
