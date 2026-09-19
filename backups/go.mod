// The backup engine: reads what each app declares (liveswap/backupdecl),
// and runs sqlite3 and restic in sandboxed transient system units. Its
// own module so the program that starts those units — root-equivalent
// by definition — links neither Caddy nor anything Caddy depends on.
module github.com/smallhoursorg/hotserve/backups

go 1.26.1

require (
	github.com/coreos/go-systemd/v22 v22.7.0
	github.com/godbus/dbus/v5 v5.2.2
)

require golang.org/x/sys v0.27.0 // indirect
