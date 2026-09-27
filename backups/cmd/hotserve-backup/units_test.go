package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The unit files the package ships, read as a table: what each has to
// say for the engine's own rules to hold, and what none of them may
// say. The engine makes bind mounts the manager has to see, so no
// property that gives the unit a mount namespace of its own may be
// there [M22]; the run's units are bound to the unit the run says it
// is, so its name has to reach the run as its environment; nothing
// starts before setup, so the credential file's existence is a
// condition on the service, not a failure of it; and the two services
// are hardened alike, since one measurement covers both.
const unitsDir = "../../../packaging"

// mountNamespaceProperties each put the unit in a mount namespace of
// its own, where the mounts the engine makes are invisible to the
// manager, which then binds the bare mount point into the units [M22].
// PrivateNetwork= and ProtectKernelModules= are on the list because
// they were measured to do the same [M54]: with either, every database
// copy failed "open app.db: permission denied".
var mountNamespaceProperties = []string{
	"ProtectSystem", "ProtectHome", "PrivateTmp", "PrivateDevices", "PrivateMounts",
	"ProtectKernelTunables", "ProtectKernelLogs", "ProtectControlGroups", "ProtectProc", "ProcSubset",
	"ProtectHostname", "ReadOnlyPaths", "ReadWritePaths", "InaccessiblePaths", "ExecPaths", "NoExecPaths",
	"BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "RootDirectory", "RootImage", "MountAPIVFS",
	"PrivateUsers", "PrivatePIDs", "DynamicUser", "MountFlags", "LogNamespace",
	"PrivateNetwork", "ProtectKernelModules",
}

func TestTheShippedUnitsSayWhatTheEngineNeeds(t *testing.T) {
	cfg := config()
	services := map[string]string{
		"hotserve-backup.service":       cfg.Self + " run",
		"hotserve-backup-drill.service": cfg.Self + " drill",
	}
	for name, exec := range services {
		t.Run(name, func(t *testing.T) {
			u := readUnit(t, name)
			for _, row := range []struct{ section, key, want string }{
				{"Unit", "ConditionPathExists", cfg.EnvFile},
				{"Unit", "After", "network-online.target"},
				{"Unit", "Wants", "network-online.target"},
				{"Service", "Type", "oneshot"},
				{"Service", "ExecStart", exec},
				{"Service", "Environment", "HOTSERVE_BACKUP_UNIT=%n"},
				{"Service", "TimeoutStartSec", "infinity"},
				{"Service", "TimeoutStopSec", "3min"},
				// The run needs root for the mounts, the walk into an
				// app's 0750 directory, the chown of its staging and of
				// the fetch directory, and the chmod of those once they
				// are the accounts'; nothing else of root's [M54].
				{"Service", "CapabilityBoundingSet", "CAP_SYS_ADMIN CAP_DAC_READ_SEARCH CAP_CHOWN CAP_FOWNER"},
				{"Service", "RestrictAddressFamilies", "AF_UNIX"},
				{"Service", "NoNewPrivileges", "yes"},
				{"Service", "RestrictSUIDSGID", "yes"},
				{"Service", "LockPersonality", "yes"},
				{"Service", "RestrictRealtime", "yes"},
				{"Service", "MemoryDenyWriteExecute", "yes"},
				{"Service", "RestrictNamespaces", "yes"},
				{"Service", "ProtectClock", "yes"},
				{"Service", "DevicePolicy", "closed"},
				{"Service", "SystemCallArchitectures", "native"},
				{"Service", "SystemCallFilter", "@system-service @mount"},
				// The run holds CAP_DAC_READ_SEARCH, and a unit that does
				// denies open_by_handle_at [M34], here as everywhere.
				{"Service", "SystemCallFilter", "~open_by_handle_at"},
			} {
				if !slices.Contains(u[row.section][row.key], row.want) {
					t.Errorf("[%s] %s: want %q, have %q", row.section, row.key, row.want, u[row.section][row.key])
				}
			}
			for _, set := range forbiddenIn(u) {
				t.Errorf("%s is set, and must not be", set)
			}
			// %n is the manager's own expansion of the unit's name, and
			// the run binds its units to it only when it is a name the
			// run's own rule takes.
			if env := u["Service"]["Environment"]; len(env) != 1 {
				t.Errorf("[Service] Environment= lines: %q, want the one", env)
			} else if own := strings.ReplaceAll(env[0], "%n", name); own != "HOTSERVE_BACKUP_UNIT="+name || !serviceRe.MatchString(name) {
				t.Errorf("the run's own name would reach it as %q", own)
			}
		})
	}
	// One hardening set for both: what is measured on one holds for the
	// other only if the two say the same.
	a, b := readUnit(t, "hotserve-backup.service")["Service"], readUnit(t, "hotserve-backup-drill.service")["Service"]
	delete(a, "ExecStart")
	delete(b, "ExecStart")
	for k := range a {
		if !slices.Equal(a[k], b[k]) {
			t.Errorf("[Service] %s differs: run %q, drill %q", k, a[k], b[k])
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("[Service] %s is the drill's alone: %q", k, b[k])
		}
	}
	// The drill waits for a run under way (its start job queues behind
	// the run's, M57); the run does not wait for a drill, so that no
	// transaction holding both is an ordering cycle.
	if drill := readUnit(t, "hotserve-backup-drill.service"); !slices.Contains(drill["Unit"]["After"], "hotserve-backup.service") {
		t.Errorf("the drill is not After= the run: %q", drill["Unit"]["After"])
	}
	if run := readUnit(t, "hotserve-backup.service"); slices.Contains(run["Unit"]["After"], "hotserve-backup-drill.service") {
		t.Error("the run is After= the drill too: an ordering cycle")
	}

	// The timers: hourly with one per-box offset inside ten minutes,
	// and the drill on Sunday at 03:30 (the owner, 2026-09-20), both
	// catching up a missed elapse at boot; each starting the service of
	// its name.
	for name, rows := range map[string][]struct{ section, key, want string }{
		"hotserve-backup.timer": {
			{"Timer", "OnCalendar", "hourly"},
			{"Timer", "RandomizedDelaySec", "10min"},
			{"Timer", "FixedRandomDelay", "true"},
			// The manager's default accuracy of a minute is applied after
			// the delay: "within the hour and ten minutes" holds only with
			// the accuracy tight (Copilot on #153).
			{"Timer", "AccuracySec", "1s"},
			{"Timer", "Persistent", "true"},
			{"Install", "WantedBy", "timers.target"},
		},
		// The drill's has the hourly's spread (the owner, 2026-09-27):
		// it fetches whole snapshots, and a fleet does not reach one
		// storage at 03:30. One offset, this box's own, every week.
		"hotserve-backup-drill.timer": {
			{"Timer", "OnCalendar", "Sun *-*-* 03:30"},
			{"Timer", "RandomizedDelaySec", "10min"},
			{"Timer", "FixedRandomDelay", "true"},
			{"Timer", "AccuracySec", "1s"},
			{"Timer", "Persistent", "true"},
			{"Install", "WantedBy", "timers.target"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			u := readUnit(t, name)
			for _, set := range forbiddenIn(u) {
				t.Errorf("%s is set, and must not be", set)
			}
			for _, row := range rows {
				if !slices.Contains(u[row.section][row.key], row.want) {
					t.Errorf("[%s] %s: want %q, have %q", row.section, row.key, row.want, u[row.section][row.key])
				}
			}
			// A Unit= would name another service; the default is the
			// service of the timer's own name, which is the one meant.
			if v, ok := u["Timer"]["Unit"]; ok {
				t.Errorf("[Timer] Unit=%q names a unit; the timer's own name is the service's", v)
			}
			if _, ok := u["Timer"]["OnBootSec"]; ok {
				t.Error("[Timer] OnBootSec= would run a backup at every boot; Persistent= is what catches up")
			}
		})
	}
}

// forbidden is what none of the unit files may say, in any section.
var forbidden = append(slices.Clone(mountNamespaceProperties), "SuccessExitStatus", "EnvironmentFile", "User", "Restart", "OnFailure", "SystemCallErrorNumber")

// forbiddenIn is every forbidden key a unit sets, as "[Section]
// Key=value", whatever section it is written in: the manager reads
// each key in its own, and a table that looks in one section holds
// only the keys that belong there.
func forbiddenIn(u map[string]map[string][]string) []string {
	var set []string
	for section, keys := range u {
		for _, key := range forbidden {
			if v, ok := keys[key]; ok {
				set = append(set, fmt.Sprintf("[%s] %s=%q", section, key, v))
			}
		}
	}
	slices.Sort(set)
	return set
}

// A forbidden key is seen wherever it is written: OnFailure= is a
// [Unit] setting, and looked for in [Service] alone its row could
// never fail (the owner's /code-review on #153).
func TestTheUnitTableSeesAForbiddenKeyWhereverItIs(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		want       []string
	}{
		{"nothing forbidden", "[Unit]\nDescription=x\n[Service]\nType=oneshot\n", nil},
		{"OnFailure= where the manager reads it", "[Unit]\nOnFailure=notify.service\n[Service]\nType=oneshot\n", []string{`[Unit] OnFailure=["notify.service"]`}},
		{"Restart= in its own section", "[Service]\nRestart=on-failure\n", []string{`[Service] Restart=["on-failure"]`}},
		{"a mount namespace", "[Service]\nProtectSystem=strict\n", []string{`[Service] ProtectSystem=["strict"]`}},
		{"a key in a section the manager ignores it in is still written, and still said", "[Install]\nUser=nobody\n", []string{`[Install] User=["nobody"]`}},
		{"two, in two sections", "[Unit]\nOnFailure=a.service\n[Service]\nPrivateTmp=yes\n", []string{`[Service] PrivateTmp=["yes"]`, `[Unit] OnFailure=["a.service"]`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := forbiddenIn(parseUnit(t, tc.name, strings.NewReader(tc.unit)))
			if !slices.Equal(got, tc.want) {
				t.Errorf("forbidden keys seen: %q, want %q", got, tc.want)
			}
		})
	}
}

// readUnit reads a unit file the package ships.
func readUnit(t *testing.T, name string) map[string]map[string][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(unitsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read only
	return parseUnit(t, name, f)
}

// parseUnit reads a unit the way the manager does for what is asserted
// here: [Section], KEY=value, a key repeated adding a value; comments
// and blank lines skipped. No continuation lines: none is written.
func parseUnit(t *testing.T, name string, f io.Reader) map[string]map[string][]string {
	t.Helper()
	u := map[string]map[string][]string{}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.Trim(line, "[]")
			if u[section] == nil {
				u[section] = map[string][]string{}
			}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				t.Fatalf("%s: a line the manager would not read: %q", name, line)
			}
			if strings.HasSuffix(v, "\\") {
				t.Fatalf("%s: a continued line, which this reader does not follow: %q", name, line)
			}
			u[section][strings.TrimSpace(k)] = append(u[section][strings.TrimSpace(k)], strings.TrimSpace(v))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return u
}

// Setup's closing line says what happens next from what is so on the
// box: with the package's timer active, that the first backup runs
// within the hour and ten minutes; without one — the raw-binary
// tarball, "By hand", an administrator's disable — that nothing runs
// on a schedule, and how to have it.
func TestSetupsClosingLineFollowsTheTimer(t *testing.T) {
	const cron = "timer or cron entry of your own"
	for _, tc := range []struct {
		apps            int
		active, enabled bool
		want            string
		never           []string
	}{
		{1, true, true, "the first backup runs within the hour and ten minutes (systemctl list-timers hotserve-backup.timer)", []string{"nothing runs it on a schedule", "not enabled"}},
		// Active now and not enabled — started by hand, or disabled
		// without --now: true until the next boot, and said so.
		{1, true, false, "the first backup runs within the hour and ten minutes, but hotserve-backup.timer is not enabled: after a reboot nothing runs it (sudo systemctl enable hotserve-backup.timer)", []string{cron}},
		{1, false, true, "nothing runs it on a schedule: sudo systemctl enable --now hotserve-backup.timer, or run sudo hotserve-backup run hourly from a " + cron, []string{"within the hour"}},
		{1, false, false, "nothing runs it on a schedule: sudo systemctl enable --now hotserve-backup.timer", []string{"within the hour"}},
		{0, true, true, "the hourly timer backs it up from then on", []string{"nothing runs it on a schedule", "not enabled"}},
		{0, true, false, "hotserve-backup.timer is not enabled: after a reboot nothing runs it", []string{cron}},
		{0, false, false, "nothing runs it on a schedule", []string{"hourly timer backs it up"}},
	} {
		got := nextLine(tc.apps, tc.active, tc.enabled)
		if !strings.Contains(got, tc.want) {
			t.Errorf("apps=%d active=%v enabled=%v: %q", tc.apps, tc.active, tc.enabled, got)
		}
		for _, n := range tc.never {
			if strings.Contains(got, n) {
				t.Errorf("apps=%d active=%v enabled=%v says %q: %q", tc.apps, tc.active, tc.enabled, n, got)
			}
		}
	}
	// The manager not answering is no failure of a setup that has
	// succeeded, and no ground for advice: the line says it could not
	// tell and how to look, not to add a scheduler beside one that may
	// be there.
	got := closing(1, false, false, errors.New("asking the manager about hotserve-backup.timer: no reply"))
	if !strings.Contains(got, "could not tell whether hotserve-backup.timer is scheduled (asking the manager about hotserve-backup.timer: no reply)") || !strings.Contains(got, "systemctl list-timers hotserve-backup.timer") || strings.Contains(got, cron) || strings.Contains(got, "nothing runs it") {
		t.Errorf("with the manager not answering: %q", got)
	}
	if got := closing(1, true, true, nil); got != nextLine(1, true, true) {
		t.Errorf("with an answer: %q", got)
	}
}

// A helper works only for a command of its own version: told which
// program started it, one that is another does nothing, and says so
// with a status of its own. Not told — started by hand — it works; and
// the helper that removes plaintext works whatever it is told.
func TestAHelperWorksOnlyForACommandOfItsOwnVersion(t *testing.T) {
	const own, other = "aaaa", "bbbb"
	for _, name := range []string{"plan", "dump", "check", "install", "extract"} {
		if err := forThisCommand(name, "", func() (string, error) { return own, nil }); err != nil {
			t.Errorf("%s, told nothing: %v", name, err)
		}
		if err := forThisCommand(name, own, func() (string, error) { return own, nil }); err != nil {
			t.Errorf("%s, told its own version: %v", name, err)
		}
		err := forThisCommand(name, other, func() (string, error) { return own, nil })
		if !errors.Is(err, errOtherVersion) || !strings.Contains(err.Error(), "this helper is not the version of the command that started it (the package was upgraded while that command was under way), and does nothing") {
			t.Errorf("%s, told another version: %v", name, err)
		}
		// One that cannot read itself cannot say it is the same.
		err = forThisCommand(name, own, func() (string, error) { return "", errors.New("open /proc/self/exe: permission denied") })
		if !errors.Is(err, errOtherVersion) || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s, which could not read itself: %v", name, err)
		}
	}
	for _, name := range []string{"clean", "run", "drill", "restore", "setup", "status", "validate", "account"} {
		if err := forThisCommand(name, other, func() (string, error) { return own, nil }); err != nil {
			t.Errorf("%s, told another version: %v", name, err)
		}
	}
	if exitStatus(errOtherVersion) != 75 || exitStatus(fmt.Errorf("x: %w", errOtherVersion)) != 75 {
		t.Errorf("a helper of another version exits %d", exitStatus(errOtherVersion))
	}
	if exitStatus(errors.New("x")) != 1 || exitStatus(couldNotTell{errors.New("x")}) != 3 {
		t.Errorf("the other statuses moved: %d, %d", exitStatus(errors.New("x")), exitStatus(couldNotTell{errors.New("x")}))
	}
}
