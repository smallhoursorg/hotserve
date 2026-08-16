// Goroutine-leak gate for the unit suite: a store sweeper or timer
// that outlives its test fails the package. Scoped to !integration
// because caddytest starts a real Caddy whose background goroutines
// legitimately outlive tests.
//go:build !integration

package penaltybox

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
