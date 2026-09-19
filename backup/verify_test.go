package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// A different fifty-second of the data each week, so a year's checks read
// all of it; restic's n/t starts at 1, and the year's odd week 53 is a
// part like any other.
func TestVerifyReadsADifferentPartEachWeek(t *testing.T) {
	seen := map[string]bool{}
	for d := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC); d.Year() == 2026; d = d.AddDate(0, 0, 7) {
		seen[verifySubset(d)] = true
	}
	if len(seen) != 52 {
		t.Errorf("want 52 parts over a year of weeks, got %d: %v", len(seen), seen)
	}
	for part := range seen {
		if strings.HasPrefix(part, "0/") || !strings.HasSuffix(part, "/52") {
			t.Errorf("not a part restic takes: %q", part)
		}
	}
	if got := verifySubset(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)); got != "1/52" { // ISO week 53
		t.Errorf("week 53 reads part 1 again, got %q", got)
	}
}

// What the check found is restic's exit status, and is recorded in the
// repository for `status`: 0 as verified; 1 as not passed — but only on a
// repository that opens before the check and after it, because 1 is also
// what restic exits with when it cannot reach the repository at all
// (found by the package smoke test, against a host that does not
// resolve); and anything else as nothing, because a check that did not
// happen says nothing about the repository.
func TestVerifyRecordsWhatResticsExitStatusSays(t *testing.T) {
	for name, tc := range map[string]struct {
		opens   []error // `cat config`, before the check and after it
		check   error
		wantTag string
		wantErr string
	}{
		"nothing wrong":            {[]error{nil}, nil, VerifiedTag, ""},
		"did not pass":             {[]error{nil, nil}, exited(1), VerifyFailedTag, "did not pass"},
		"never reachable":          {[]error{exited(1)}, nil, "", "could not be checked"},
		"lost part-way":            {[]error{nil, exited(1)}, exited(1), "", "could not be checked"},
		"lock not had":             {[]error{nil}, exited(11), "", "could not be checked"},
		"stopped":                  {[]error{nil}, errors.New("signal: killed"), "", "could not be checked"},
		"the password is not this": {[]error{exited(12)}, nil, "", "could not be checked"},
	} {
		t.Run(name, func(t *testing.T) {
			var recorded string
			opens := tc.opens
			x := func(_ context.Context, c Cmd) error {
				switch c.Args[0] {
				case "cat":
					if !slices.Contains(c.Args, "--no-lock") {
						t.Errorf("asking whether it opens takes no lock: %v", c.Args)
					}
					err := opens[0]
					opens = opens[1:]
					return err
				case "check":
					if !slices.Contains(c.Args, "--read-data-subset=3/52") {
						t.Errorf("want this week's part read back: %v", c.Args)
					}
					return tc.check
				}
				recorded = c.Args[slices.Index(c.Args, "--tag")+1]
				return nil
			}
			err := Verify(context.Background(), x, time.Date(2026, 1, 14, 3, 0, 0, 0, time.UTC), io.Discard)
			if recorded != tc.wantTag {
				t.Errorf("recorded %q, want %q", recorded, tc.wantTag)
			}
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("error %v, want one saying %q", err, tc.wantErr)
			}
		})
	}
}

// The newest record decides, of either kind; none at all is "not checked
// yet", which is named and is not a failure.
func TestLastVerificationAndWhenItIsOverdue(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	list := func(snaps ...Snapshot) Exec {
		return fake(nil, func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if !slices.Contains(args, "--no-lock") {
				t.Errorf("status writes nothing into the repository, a lock included: %v", args)
			}
			return json.Marshal(snaps)
		})
	}
	ok := func(age time.Duration) Snapshot { return Snapshot{Time: now.Add(-age), Tags: []string{VerifiedTag}} }
	bad := func(age time.Duration) Snapshot {
		return Snapshot{Time: now.Add(-age), Tags: []string{VerifyFailedTag}}
	}
	day := 24 * time.Hour
	for name, tc := range map[string]struct {
		snaps   []Snapshot
		since   time.Duration // how long the repository has held backups; 0 for none yet
		overdue bool
		says    string
	}{
		"never checked, new":            {nil, 3 * day, false, "not checked yet"},
		"never checked, no backups yet": {nil, 0, false, "not checked yet"},
		// No record is not a pass for ever: a timer that is masked, or a
		// check that can never reach the repository, writes none.
		"never checked, in use for a month": {nil, 30 * day, true, "never checked, though it has held backups"},
		"checked this week":                 {[]Snapshot{ok(3 * day), ok(10 * day)}, 90 * day, false, "nothing wrong"},
		"two weeks missed":                  {[]Snapshot{ok(16 * day)}, 90 * day, true, "has not finished since"},
		"the last one found harm":           {[]Snapshot{ok(8 * day), bad(day)}, 90 * day, true, "did not pass"},
		"mended since":                      {[]Snapshot{bad(8 * day), ok(day)}, 90 * day, false, "nothing wrong"},
	} {
		t.Run(name, func(t *testing.T) {
			v, err := LastVerification(context.Background(), list(tc.snaps...))
			if err != nil {
				t.Fatal(err)
			}
			var since time.Time
			if tc.since > 0 {
				since = now.Add(-tc.since)
			}
			if v.Overdue(now, since) != tc.overdue {
				t.Errorf("overdue = %v, want %v", v.Overdue(now, since), tc.overdue)
			}
			var out strings.Builder
			FormatVerification(&out, v, now, since)
			if !strings.Contains(out.String(), tc.says) {
				t.Errorf("want the report to say %q:\n%s", tc.says, out.String())
			}
		})
	}
}

// The weekly check holds the repository exclusively, and restic waits for
// a lock only when told to: every command of a job that takes the lock is
// told, and the one that takes none is left alone.
func TestWaitingForLock(t *testing.T) {
	var got []Cmd
	x := waitingForLock(func(_ context.Context, c Cmd) error { got = append(got, c); return nil })
	for _, c := range []Cmd{restic("backup", "/data"), restic("ls", "--json", "s1"), restic("snapshots", "--no-lock", "--json"), {Name: "sqlite3", Args: []string{"app.db"}}} {
		if err := x(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []bool{true, true, false, false} {
		if has := slices.Contains(got[i].Args, "--retry-lock"); has != want {
			t.Errorf("%s %v: --retry-lock = %v, want %v", got[i].Name, got[i].Args, has, want)
		}
	}
	if got[0].Args[0] != "--retry-lock" || got[0].Args[1] != lockWait || got[0].Args[2] != "backup" {
		t.Errorf("want the wait in front of the command, which stays as it was: %v", got[0].Args)
	}
}
