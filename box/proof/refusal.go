// Package proof is the box channel's proof core: it decides whether a
// bundle — a Caddyfile, the commit object that is claimed to contain
// it, the trees on the path between them, and the first-parent chain
// down to the commit the box runs — is what it claims to be. Every
// check is from bytes: ids are computed, never read; a signature is
// ssh-keygen's verdict on the commit object with its gpgsig header
// removed, against the signer list of the Caddyfile the box runs.
//
// The package is stdlib plus golang.org/x/crypto/ssh (key parsing) and
// imports nothing of Caddy's, so the applier's root-side checks and the
// handler's pre-check (DESIGN-box.md, steps 5 and 11–14) run the same
// code with the same messages.
package proof

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Refusal is a verdict the box can show whole: one line, in the words
// DESIGN-box.md "Refusals" lists, naming the step that failed. Any
// text a bundle chose that a message quotes — the path — passes Bound
// first; everything else a message carries (ids, principals) the proof
// derived itself. An error that is not a Refusal is the box's own
// trouble (ssh-keygen could not be run), not a verdict on the push.
type Refusal struct {
	Msg  string
	code refusalCode
}

func (r *Refusal) Error() string { return r.Msg }

// refusalCode tells apart the signature verdicts a chain must treat
// differently (VerifyChain); every other refusal is codeNone.
type refusalCode int

const (
	codeNone     refusalCode = iota
	codeUnsigned             // no gpgsig, or one that is not an SSH signature
	codeUnlisted             // an SSH signature by a key the list does not carry
	codeAltered              // a listed key's signature that does not verify over the payload
)

func refuse(format string, args ...any) error {
	return &Refusal{Msg: fmt.Sprintf(format, args...)}
}

func refuseCode(code refusalCode, format string, args ...any) error {
	return &Refusal{Msg: fmt.Sprintf(format, args...), code: code}
}

// maxBound is liveswap's maxRefusalLen: the most bytes of caller-chosen
// text one refusal line carries.
const maxBound = 300

// Bound is liveswap's boundRefusal (deploytrust.go), kept the same so
// that a path or an address in a box refusal is bounded exactly as a
// token's claims are in a deploy's: Go-quoted to ASCII first if the
// text is not printable UTF-8 (so a newline or a control byte cannot
// split or forge a journal line), and only then cut, at a rune
// boundary, so the cut can neither leave a partial rune nor grow the
// string it bounds. A cut-then-quote is not a bound.
func Bound(s string) string {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return !strconv.IsPrint(r) && r != ' ' }) {
		s = strconv.QuoteToASCII(s)
	}
	if len(s) > maxBound {
		s = cutRunes(s, maxBound) + "..."
	}
	return s
}

// cutRunes is s cut to at most n bytes at a rune boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
