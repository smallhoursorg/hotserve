package backupdecl

import (
	"path"
	"regexp"
)

// What a reader of declarations has to know about liveswap to find the
// data they name. Each is liveswap's own value, repeated here so that a
// program outside the serving process need not link liveswap to learn
// it; liveswap's TestBackupdeclAgreesWithLiveswap is what keeps the
// two from drifting.

// DefaultRoot is the liveswap root when the config sets none.
const DefaultRoot = "/var/lib/liveswap"

var appNameRe = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// ValidAppName reports whether name is one liveswap accepts for an app.
func ValidAppName(name string) bool { return appNameRe.MatchString(name) }

// SharedDir is the directory an app's declared paths are relative to.
func SharedDir(root, app string) string { return path.Join(root, app, "shared") }
