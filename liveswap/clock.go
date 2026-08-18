package liveswap

import "time"

// clock abstracts time.Now and sleeping so deploy/health logic is
// testable without real waits. time.Time values from realClock carry
// Go's monotonic reading, making soak/deadline arithmetic immune to
// wall-clock jumps. Sleep is part of the interface (not just Now)
// because the prober and drain steps pace themselves; the fake clock's
// Sleep advances instantly so tests run in microseconds. After exists
// for the watchdog, which paces itself inside a select and so needs a
// channel rather than a blocking Sleep; the fake clock fires the
// channel from Advance.
type clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) Sleep(d time.Duration)                  { time.Sleep(d) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
