//go:build e2e

package engine

import "time"

// The e2e box's hotserve-backup only (make e2e-backup builds it with
// -tags e2e; the package never is): the setup suite waits out the clock
// once, on a repository that never answers, and two minutes of that is
// most of the suite. Thirty seconds keeps the order a person sees — the
// look's 10 s, the note at 20 s, then the clock — and is still longer
// than any init or open that answers [measured: at once, or 30 s for a
// host that swallows packets, which the suite does not have].
func init() { setupClock = 30 * time.Second }
