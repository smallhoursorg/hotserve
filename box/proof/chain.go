package proof

import (
	"context"
	"errors"
)

// MaxChain bounds the first-parent chain a bundle carries
// (DESIGN-box.md, "Store rules").
const MaxChain = 500

// Chain is step 14's walk: first parents from head until baseline,
// through the bundled parent objects (keyed by computed id). It
// returns the commits above the baseline, head first — every one of
// which must then carry a signature the box accepts (VerifyChain) —
// and an empty chain when head is the baseline. A walk that runs out
// of parents, or of bundled objects, before it reaches the baseline is
// a history that no longer contains the commit the box runs: a rewind,
// a replay, or a rebase that rewrote the baseline itself, all refused
// the same way, naming `hotserve box baseline` for the one case it is
// for. A chain of more than MaxChain commits above the baseline is
// refused with the message that names the reset — the workflow checks
// the same bound before it posts.
func Chain(head *Commit, parents map[string]*Commit, baseline string) ([]*Commit, error) {
	if head.ID == baseline {
		return nil, nil
	}
	chain := []*Commit{head}
	cur := head
	for {
		if len(cur.Parents) == 0 {
			return nil, descendRefusal(head.ID, baseline)
		}
		p := cur.Parents[0]
		if p == baseline {
			return chain, nil
		}
		next, ok := parents[p]
		if !ok || next.ID != p {
			return nil, descendRefusal(head.ID, baseline)
		}
		if len(chain) == MaxChain {
			return nil, refuse("the chain from %s to %s is longer than %d commits; run hotserve box baseline %s as root on the box", baseline, head.ID, MaxChain, head.ID)
		}
		chain = append(chain, next)
		cur = next
	}
}

func descendRefusal(head, baseline string) error {
	return refuse("%s does not descend from the commit this box runs (%s). "+
		"If the box already runs a later commit than this run's, nothing is wrong: a newer push applied first. "+
		"A rewind or a replay is refused on purpose. "+
		"Only if main was rewritten below %s does hotserve box baseline %s, as root on the box, reset trust to %s — whose ancestors the box will then never examine",
		head, baseline, baseline, head, head)
}

// VerifyChain is steps 12 and 14 together: every commit of the chain
// (head first, as Chain returns it) must verify against the signers.
// It returns the principal whose key signed head. Head's own refusals
// are the three named ones (not signed, OpenPGP, not a signer); a
// commit between the baseline and head is refused with the message
// that says what to do about it — rebase it away and force-push, no
// baseline change — because an unsigned commit a leaked credential
// pushed would otherwise ride into the box under the next signed one.
func VerifyChain(ctx context.Context, v *Verifier, chain []*Commit, signers Signers, baseline string) (string, error) {
	principal := ""
	for i, c := range chain {
		p, err := v.Verify(ctx, c, signers)
		if err != nil {
			var r *Refusal
			if i > 0 && errors.As(err, &r) {
				return "", refuse("%s, between the commit this box runs and %s, is not signed by a signer; every commit on main must be — rebase it out of the history and force-push; the box still runs %s, so no baseline change is needed",
					c.ID, chain[0].ID, baseline)
			}
			return "", err
		}
		if i == 0 {
			principal = p
		}
	}
	return principal, nil
}
