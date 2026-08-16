// Goroutine-leak gate for the unit suite: any test that leaves a
// goroutine behind (a health prober that never stops, an unreaped
// process waiter, a forgotten timer) fails the package. Scoped to
// !integration because caddytest starts a real Caddy whose background
// goroutines (admin, cert maintenance) legitimately outlive tests.
//go:build !integration

package liveswap

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
