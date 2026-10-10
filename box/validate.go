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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap"
)

// The children's bounds (DESIGN-box.md, Caps): one deadline for each
// of the three, validate's output kept by its tail, the adaptation by
// its head.
const (
	validateTimeout   = 120 * time.Second
	maxValidateOutput = 4 << 10
	maxAdaptation     = 16 << 20
)

// The programs, by absolute path: the package installs both there.
const (
	hotserveProgram = "/usr/bin/hotserve"
	backupProgram   = "/usr/bin/hotserve-backup"
)

// xOK is access(2)'s X_OK: `test -x`'s question.
const xOK = 0x1

// validator is step 6. host is the box_webhook host the walk found in
// caddyfile, which reachability is asked for. msg is a refusal's whole
// catalogue text; err is the box's own trouble (the staged file could
// not be written, a child could not start, whether backups are
// installed could not be told, an adaptation that does not read).
type validator interface {
	validate(ctx context.Context, id string, caddyfile []byte, host string) (msg string, err error)
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
// without the variables systemd and the login shell set for every
// service (systemd.exec(5), "Environment Variables in Spawned
// Processes"): they are the box's own words — `hotserve` as USER, the
// state paths — and a diagnostic needs them readable, as liveswap keeps
// an app's SOCKET, HOME and PATH. Everything else, the operator's
// Environment= and EnvironmentFile= lines among it, is a value the
// filter knows, whatever it looks like.
func secretCandidates(env []string) []string {
	var out []string
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && !serviceVariable(key) {
			out = append(out, kv)
		}
	}
	return out
}

// serviceVariable reports whether key is one systemd or the shell sets.
func serviceVariable(key string) bool {
	switch key {
	case "PATH", "LANG", "LANGUAGE", "USER", "LOGNAME", "HOME", "SHELL", "PWD", "TERM",
		"INVOCATION_ID", "JOURNAL_STREAM", "NOTIFY_SOCKET", "MAINPID", "MANAGERPID", "SYSTEMD_EXEC_PID",
		"LISTEN_FDS", "LISTEN_PID", "LISTEN_FDNAMES", "WATCHDOG_PID", "WATCHDOG_USEC",
		"RUNTIME_DIRECTORY", "STATE_DIRECTORY", "CACHE_DIRECTORY", "LOGS_DIRECTORY",
		"CONFIGURATION_DIRECTORY", "CREDENTIALS_DIRECTORY", "MEMORY_PRESSURE_WATCH",
		"MEMORY_PRESSURE_WRITE", "FDSTORE", "LOG_NAMESPACE":
		return true
	}
	return strings.HasPrefix(key, "LC_") || strings.HasPrefix(key, "XDG_")
}

func (c *childValidator) validate(ctx context.Context, id string, caddyfile []byte, host string) (string, error) {
	staged := filepath.Join(c.stage, stagedConfig(id))
	if err := writeExclusive(staged, caddyfile, 0o600); err != nil {
		return "", fmt.Errorf("staging the Caddyfile for validate: %w", err)
	}
	defer os.Remove(staged) //nolint:errcheck // a leftover is swept at the next admission
	if msg, err := c.run(ctx, "hotserve validate", c.hotserve, "validate", "--adapter", "caddyfile", "--config", staged); msg != "" || err != nil {
		return msg, err
	}
	if msg, err := c.adapt(ctx, staged, host); msg != "" || err != nil {
		return msg, err
	}
	// `test -x`'s answer: no file, or one this uid cannot run, is "not
	// installed"; a lookup that failed some other way is "could not tell".
	switch err := syscall.Access(c.backup, xOK); {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.ENOTDIR):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("could not ask whether backups are installed: %w", err)
	}
	return c.run(ctx, "hotserve-backup validate", c.backup, "validate", staged)
}

// run runs one validate child. A non-zero exit is a refusal quoting its
// error line; a child that did not finish in time is one too, since a
// configuration whose provisioning hangs is the push's; a child that
// could not start, or that a signal other than the deadline's killed
// (the OOM killer, an operator), is the box's. A child that exited 0 and
// left a grandchild holding the output passed.
func (c *childValidator) run(ctx context.Context, what, program string, args ...string) (string, error) {
	var out tailBuffer
	// One writer for both: os/exec then uses one pipe, and WaitDelay
	// ends the wait on it should a grandchild hold it.
	exit, timedOut, err := c.exec(ctx, program, &out, &out, args...)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %w", what, err)
	case timedOut:
		return fmt.Sprintf("%s: did not finish within %s", what, c.timeout), nil
	case exit == nil:
		return "", nil
	}
	line, _ := c.redactor.Redact(errorLine(out.b))
	if line == "" {
		line = exit.String()
	}
	return what + ": " + proof.Bound(line), nil
}

// adapt runs the adapt child on the staged file and asks reachability
// of its output (DESIGN-box.md, "Reachability"). The adapter is the one
// validate just ran, so an exit here is the box's error, not the
// push's; a deadline is the push's, as validate's. Stdout is kept to
// the cap and stderr dropped: the JSON holds every expanded value, and
// nothing of it but the walk's verdict leaves this function.
func (c *childValidator) adapt(ctx context.Context, staged, host string) (string, error) {
	out := cappedBuffer{limit: maxAdaptation}
	exit, timedOut, err := c.exec(ctx, c.hotserve, &out, nil, "adapt", "--adapter", "caddyfile", "--config", staged)
	switch {
	case err != nil:
		return "", fmt.Errorf("hotserve adapt: %w", err)
	case timedOut:
		return fmt.Sprintf("hotserve adapt: did not finish within %s", c.timeout), nil
	case exit != nil:
		return "", fmt.Errorf("hotserve adapt: %w", exit)
	case out.over():
		return msgAdaptationTooLarge, nil
	}
	err = reach(out.b, host)
	var r *refusal
	switch {
	case errors.As(err, &r):
		return r.msg, nil
	case err != nil:
		return "", fmt.Errorf("hotserve adapt: %w", err)
	}
	return "", nil
}

// exec runs one child in its own process group, bounded by the
// deadline and WaitDelay, stdout and stderr to the writers given (nil
// is /dev/null, and no pipe a grandchild could hold). It returns the
// child's exit when it exited non-zero; whether the deadline ended it
// (an exit status decides first: a child that exited said why, even if
// the deadline passed while a grandchild held the output); and the
// box's own error: the caller gone, a program that could not start, a
// signal other than the deadline's. A child that exited 0 and left a
// grandchild holding the output passed.
func (c *childValidator) exec(ctx context.Context, program string, stdout, stderr io.Writer, args ...string) (exit *exec.ExitError, timedOut bool, err error) {
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, program, args...) //nolint:gosec // a fixed program by absolute path; the one argument from the push is a file name the handler chose
	cmd.Env = c.env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = childWaitDelay
	// Its own process group, so that the deadline, and the end of the
	// run, take whatever the child started with it: a grandchild left
	// holding the output would otherwise outlive the request.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return os.ErrProcessDone // it had exited: nothing was cancelled
	}
	runErr := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH when nothing is left
	}
	exited := errors.As(runErr, &exit) && exit.Exited()
	switch {
	case runErr == nil, errors.Is(runErr, exec.ErrWaitDelay):
		return nil, false, nil
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case !exited && bounded.Err() != nil:
		return nil, true, nil
	case !exited:
		return nil, false, runErr
	}
	return exit, false, nil
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
