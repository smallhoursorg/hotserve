package box

// Step 6 (DESIGN-box.md, "The trust chain"): the pushed file validated
// as the serving process would load it — by `hotserve validate`, and
// by `hotserve-backup validate` when backups are installed — as bounded
// children with the service's own environment. Root never runs either:
// validate provisions every module, and the adapter expands `{$VAR}`
// from the caller's environment and runs each module's parser on the
// input. A courtesy against a signer's mistake, not a boundary.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap"
)

// The children's bounds (DESIGN-box.md, Caps).
const (
	validateTimeout   = 120 * time.Second
	maxValidateOutput = 4 << 10
)

// The programs, by absolute path: the package installs both there.
const (
	hotserveProgram = "/usr/bin/hotserve"
	backupProgram   = "/usr/bin/hotserve-backup"
)

// xOK is access(2)'s X_OK: `test -x`'s question.
const xOK = 0x1

const msgBackupUnknown = "could not ask whether backups are installed; nothing changed"

// validator is step 6. msg is a refusal's whole catalogue text; err is
// the box's own trouble (the staged file could not be written, a child
// could not start).
type validator interface {
	validate(ctx context.Context, id string, caddyfile []byte) (msg string, err error)
}

// childValidator runs the real children.
type childValidator struct {
	stage            string // where the staged copy is written
	hotserve, backup string
	env              []string
	timeout          time.Duration
	// redactor is primed with the service's environment: validate's
	// errors quote expanded values.
	redactor *liveswap.Redactor
}

func newChildValidator(stage string) *childValidator {
	env := os.Environ()
	return &childValidator{
		stage: stage, hotserve: hotserveProgram, backup: backupProgram,
		env: env, timeout: validateTimeout, redactor: liveswap.NewRedactor(secretCandidates(env), nil),
	}
}

// secretCandidates is the environment as the redactor's known values,
// without the variables that hold paths: a diagnostic needs its paths,
// as liveswap keeps an app's SOCKET, HOME and PATH readable (redact.go).
func secretCandidates(env []string) []string {
	var out []string
	for _, kv := range env {
		if _, v, ok := strings.Cut(kv, "="); ok && !strings.HasPrefix(v, "/") {
			out = append(out, kv)
		}
	}
	return out
}

func (c *childValidator) validate(ctx context.Context, id string, caddyfile []byte) (string, error) {
	staged := filepath.Join(c.stage, stagedConfig(id))
	if err := writeExclusive(staged, caddyfile, 0o600); err != nil {
		return "", fmt.Errorf("staging the Caddyfile for validate: %w", err)
	}
	defer os.Remove(staged) //nolint:errcheck // a leftover is swept at the next admission
	if msg, err := c.run(ctx, "hotserve validate", c.hotserve, "validate", "--adapter", "caddyfile", "--config", staged); msg != "" || err != nil {
		return msg, err
	}
	switch err := syscall.Access(c.backup, xOK); {
	case errors.Is(err, syscall.ENOENT):
		return "", nil // backups are not installed
	case err != nil:
		return msgBackupUnknown, nil
	}
	return c.run(ctx, "hotserve-backup validate", c.backup, "validate", staged)
}

// run runs one child. A non-zero exit is a refusal quoting its error
// line; a child that did not finish in time is one too, since a
// configuration whose provisioning hangs is the push's; a child that
// could not start is the box's.
func (c *childValidator) run(ctx context.Context, what, program string, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, program, args...) //nolint:gosec // a fixed program by absolute path; the one argument from the push is a file name the handler chose
	cmd.Env = c.env
	var out tailBuffer
	// One writer for both: os/exec then uses one pipe, and WaitDelay
	// ends the wait on it should a grandchild hold it.
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = childWaitDelay
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return "", nil
	case bounded.Err() != nil && ctx.Err() == nil:
		return fmt.Sprintf("%s: did not finish within %s", what, c.timeout), nil
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s: %w", what, ctx.Err())
	case errors.As(err, &exit):
		line, _ := c.redactor.Redact(errorLine(out.b))
		if line == "" {
			line = exit.String()
		}
		return what + ": " + proof.Bound(line), nil
	default:
		return "", fmt.Errorf("%s: %w", what, err)
	}
}

// errorLine is the line of a validate's output that says why: Caddy's
// last `Error: ` line, else the last line that is not empty.
func errorLine(out []byte) string {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if rest, ok := strings.CutPrefix(lines[i], "Error: "); ok {
			return strings.TrimSpace(rest)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// tailBuffer keeps the last maxValidateOutput bytes written to it: a
// validate says why at the end. Not a bytes.Buffer: os/exec would use
// its ReadFrom and skip Write.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= maxValidateOutput {
		t.b = append(t.b[:0], p[len(p)-maxValidateOutput:]...)
		return n, nil
	}
	if over := len(t.b) + len(p) - maxValidateOutput; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	t.b = append(t.b, p...)
	return n, nil
}
