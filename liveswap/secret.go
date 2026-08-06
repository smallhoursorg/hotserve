package liveswap

import (
	"crypto/sha256"
	"crypto/subtle"
)

// secretsEqual compares a provided secret against the configured one in
// constant time. Both sides are hashed first so the comparison length —
// and therefore its timing — never depends on either input's length.
func secretsEqual(provided, configured string) bool {
	hp := sha256.Sum256([]byte(provided))
	hc := sha256.Sum256([]byte(configured))
	return subtle.ConstantTimeCompare(hp[:], hc[:]) == 1
}
