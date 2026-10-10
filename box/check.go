package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// refusal is a verdict on the push, the whole catalogue text: the result
// is `refused`. Any other error from check is the box's own (ssh-keygen
// could not run, the chain deadline passed, the box's own files do not
// read): never a refusal, and the result is `failed`.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refused(format string, args ...any) error {
	return &refusal{msg: fmt.Sprintf(format, args...)}
}

// verdict is what steps 10 to 15 established about a bundle.
type verdict struct {
	// signer is the installed principal whose key verified HEAD.
	signer string
	// host is the box_webhook host, the installed file's and the
	// incoming one's alike.
	host string
	// apps are the incoming file's app names.
	apps []string
	// prevSum and newSum are the installed and the incoming file's
	// digests; outOfBand is step 10's flag: the installed file is not
	// the one applied.json recorded.
	prevSum, newSum string
	outOfBand       bool
}

// check is steps 10 to 15 of the trust chain (DESIGN-box.md), from
// bytes: the bundle as read once, the installed file as read once, the
// baseline. It is what the applier runs as root and what the handler's
// pre-verification runs as hotserve (step 5), so the two cannot
// disagree about a verdict or its words.
//
// The checks run cheapest first — the two readings of the files and the
// identity (10, 11, 15's presence rules), the file proof (13), the
// chain's linkage (14) — so that a malformed bundle never starts
// ssh-keygen; then every signature on the chain, HEAD's first (12 and
// 14); then the guard that the verifying key stays listed (15). When
// more than one step would refuse, the first so found is the one named.
func check(ctx context.Context, v *proof.Verifier, b *proof.Bundle, installed []byte, base *applied) (*verdict, error) {
	// 10. The installed file: the signer list and the host, from its
	// raw tokens. An empty list refuses by name; a file the walk refuses
	// for any other reason is the box's trouble, not the push's.
	cur, err := Walk(installed)
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) && ref.Reason == "has no signer" {
			return nil, refused("%s", msgNoSigner)
		}
		if ref != nil {
			return nil, fmt.Errorf("the Caddyfile this box runs %s", ref.Reason)
		}
		return nil, err
	}
	vd := &verdict{prevSum: digest(installed), newSum: digest(b.Caddyfile)}
	vd.outOfBand = vd.prevSum != base.SHA256

	// 11 and 15's presence rules: the incoming file reads as a box's
	// (box block, signers, deploy_trust, one bare-hostname box_webhook
	// site), names this box's host, and is this box's path.
	next, err := Walk(b.Caddyfile)
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			return nil, refused("the new Caddyfile %s", ref.Reason)
		}
		return nil, err
	}
	if next.Host != cur.Host {
		return nil, refused("this file is for %s; this box is %s", next.Host, cur.Host)
	}
	if b.Path != base.Path {
		return nil, refused("this box's file is %s; the bundle is %s — hotserve init --path records a new one", proof.Bound(base.Path), proof.Bound(b.Path))
	}
	vd.host, vd.apps = cur.Host, next.Apps

	// 13. The file is the committed file.
	if err := proof.ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile); err != nil {
		return nil, asRefusal(err)
	}
	// 14, the linkage: HEAD down to the baseline, by hash, within the cap.
	chain, err := proof.Chain(b.Commit, b.Parents, base.SHA)
	if err != nil {
		return nil, asRefusal(err)
	}
	// 12 and 14, the signatures: every commit on the chain, against the
	// installed list.
	signer, err := proof.VerifyChain(ctx, v, chain, cur.Signers, next.Signers, base.SHA)
	if err != nil {
		return nil, asRefusal(err)
	}
	vd.signer = signer

	// 15. Never cut the branch you sit on: the key that verified HEAD is
	// still listed in the incoming file, by key, whatever it is named.
	key, _, err := proof.SignatureKey(b.Commit.Signature)
	if err != nil {
		return nil, err // VerifyChain read this same signature: unreachable
	}
	if _, ok := next.Signers.PrincipalFor(key); !ok {
		return nil, refused("the new Caddyfile drops the key that signed this commit (%s); add the new key in one push, let it apply, then remove the old one", signer)
	}
	return vd, nil
}

// asRefusal turns the proof's verdicts into refusals and passes every
// other error — the box's — through as it is.
func asRefusal(err error) error {
	var r *proof.Refusal
	if errors.As(err, &r) {
		return &refusal{msg: r.Msg}
	}
	return err
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
