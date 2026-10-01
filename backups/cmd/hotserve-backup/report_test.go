package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/engine"
	"github.com/smallhoursorg/hotserve/backups/record"
)

// printed is what fn writes to stdout, with in on its stdin.
func printed(t *testing.T, in string, fn func()) string {
	t.Helper()
	outR, outW, err := os.Pipe()
	must(t, err)
	inR, inW, err := os.Pipe()
	must(t, err)
	_, err = io.WriteString(inW, in)
	must(t, err)
	must(t, inW.Close())
	oldOut, oldIn := os.Stdout, os.Stdin
	os.Stdout, os.Stdin = outW, inR
	defer func() { os.Stdout, os.Stdin = oldOut, oldIn }()
	said := make(chan string)
	go func() {
		raw, _ := io.ReadAll(outR)
		said <- string(raw)
	}()
	fn()
	must(t, outW.Close())
	return <-said
}

// Every command prints a snapshot by its first eight characters and a
// time to the minute in UTC (record.Short, record.When), as status does,
// whatever zone a time was read in: one written at +02:00 — by a restic
// that ran in the box's zone, by hand — is the minute status gives it.
func TestEveryCommandPrintsASnapshotAndATimeOneWay(t *testing.T) {
	const id = "5f422bce75a41b7f86515ed9050c3d535df6b5728b7815f6c146eb774197d91f"
	at := time.Date(2026, 10, 1, 14, 34, 56, 0, time.FixedZone("", 2*60*60))
	snap := record.Snapshot{ID: id, Time: at}
	proven := &record.Drill{Snapshot: snap, Time: at}
	for what, print := range map[string]func(){
		"a run's report": func() {
			report(&record.Status{Apps: map[string]*record.App{"blog": {Class: record.NotRun, LastOK: &snap}}})
		},
		"a restore's question": func() {
			confirm(context.Background())(engine.RestoreAsk{App: "blog", Snapshot: snap, Into: "/srv/out", LastOK: &snap})
		},
		"a restore's report": func() {
			reportRestore(&engine.RestoreReport{App: "blog", Snapshot: snap, Into: "/srv/out", LastOK: &snap, Items: []record.Item{{Kind: "files", Path: "uploads", OK: true}}})
		},
		"a drill's report": func() {
			reportDrill(&record.Status{Apps: map[string]*record.App{
				"blog": {RestoreDrill: &record.Drill{Snapshot: snap, Time: at, Detail: "no room"}, RestoreProven: proven},
				"shop": {RestoreProven: proven},
			}}, nil)
		},
	} {
		said := printed(t, "blog\n", print)
		if strings.Contains(said, "+0200") || !strings.Contains(said, "2026-10-01 12:34 UTC") {
			t.Errorf("%s prints a time other than as 2026-10-01 12:34 UTC:\n%s", what, said)
		}
		if strings.Contains(said, id[:9]) || !strings.Contains(said, id[:8]) {
			t.Errorf("%s prints a snapshot other than as %s:\n%s", what, id[:8], said)
		}
	}
}
