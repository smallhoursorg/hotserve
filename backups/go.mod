// The backup engine: reads what each app declares (liveswap/backupdecl),
// and runs sqlite3 and restic in sandboxed transient system units. Its
// own module so the program that starts those units — root-equivalent
// by definition — links neither Caddy nor anything Caddy depends on.
module github.com/smallhoursorg/hotserve/backups

go 1.26.1

require (
	github.com/coreos/go-systemd/v22 v22.7.0
	github.com/godbus/dbus/v5 v5.2.2
	github.com/smallhoursorg/hotserve/liveswap v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260811182544-a038080d80e5 // indirect
	golang.org/x/tools v0.49.0 // indirect
	golang.org/x/tools/go/packages/packagestest v0.1.1-deprecated // indirect
	golang.org/x/vuln v1.1.4 // indirect
)

// Built only from this repo, like the product module: backupdecl is a
// package of the liveswap module next door.
replace github.com/smallhoursorg/hotserve/liveswap => ../liveswap

// govulncheck is a tool dependency (never compiled into the product,
// invisible to importers) so that Dependabot bumps it like any require:
// the Makefile invokes `go tool govulncheck` with no version literal.
tool golang.org/x/vuln/cmd/govulncheck
