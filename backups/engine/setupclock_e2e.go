//go:build e2e

package engine

import "time"

// The e2e box's hotserve-backup only (make e2e-backup builds it with
// -tags e2e; the package never is): the setup suite waits out the clock
// once, on a repository that never answers, and two minutes of that is
// most of the suite. Thirty seconds keeps the order a person sees — the
// look's 10 s, the note at 20 s, then the clock. Every init or open the
// suite makes answers at once [measured]; the one case measured to
// take longer, 30 s for a host that swallows packets, would tie with
// this clock, and the suite has no such host.
func init() { setupClock = 30 * time.Second }
