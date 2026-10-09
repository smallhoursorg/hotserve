package proof

import (
	"context"
	"errors"
)

// MaxChain bounds the first-parent chain a bundle carries
// (DESIGN-box.md, "Store rules").
const MaxChain = 500

// Chain is step 14's walk: first parents from head until baseline,
// through the bundled parent objects in the order the workflow listed
// them (`parents/0001` first), linked by hash, not by position: each
// entry's computed id must equal the previous commit's first `parent`
// header, and the walk ends when a first `parent` header equals the
// baseline — whose object is not bundled. An entry past that point, or
// one that is not the first parent of the commit before it, is a
// malformed bundle. It returns the commits above the baseline, head
// first — every one of which must then carry a signature the box
// accepts (VerifyChain) — and an empty chain when head is the
// baseline. A walk that runs out of parents, or of bundled objects,
// before it reaches the baseline is a history that no longer contains
// the commit the box runs on its first-parent line, refused with the
// message that names the three ways that happens. A chain of more than
// MaxChain commits above the baseline is refused with the message that
// names the reset; the workflow checks the same bound before it posts.
func Chain(head *Commit, parents []*Commit, baseline string) ([]*Commit, error) {
	var chain []*Commit
	cur := head
	if head.ID != baseline {
		chain = append(chain, head)
	}
	for i, p := range parents {
		if cur.ID == baseline || len(cur.Parents) == 0 || cur.Parents[0] == baseline {
			return nil, refuse("bundle: parents/%04d is past the end of the chain", i+1)
		}
		if p.ID != cur.Parents[0] {
			return nil, refuse("bundle: parents/%04d is not the first parent of %s", i+1, cur.ID)
		}
		if len(chain) == MaxChain {
			return nil, refuse("the chain from %s to %s is longer than %d commits; run hotserve box baseline %s as root on the box", baseline, head.ID, MaxChain, head.ID)
		}
		chain = append(chain, p)
		cur = p
	}
	if cur.ID == baseline || (len(cur.Parents) > 0 && cur.Parents[0] == baseline) {
		return chain, nil
	}
	return nil, descendRefusal(head.ID, baseline)
}

func descendRefusal(head, baseline string) error {
	return refuse("%s does not descend from the commit this box runs (%s) along main's first-parent line. "+
		"If the box already runs a later commit than this run's, nothing is wrong: a newer push applied first. "+
		"If a branch merged main into itself before it was fast-forwarded, %s is on the merge's other side — rebase the branch onto main instead and force-push; no box step. "+
		"A rewind or a replay is refused on purpose. "+
		"Only if main was rewritten below %s does hotserve box baseline %s, as root on the box, reset trust to %s — whose ancestors the box will then never examine",
		head, baseline, baseline, baseline, head, head)
}

// VerifyChain is steps 12 and 14 together: every commit of the chain
// (head first, as Chain returns it) must verify against the installed
// signers, within one deadline for the whole chain over and above each
// child's. It returns the principal whose key signed head. Head's own
// refusals are the named ones (not signed, OpenPGP, not a signer, a
// signature that does not verify). A commit between the baseline and
// head is refused by what its verdict was: unsigned, or signed by
// anything but an SSH key, gets the message that says to rebase it
// away and force-push, no baseline change — an unsigned commit a
// leaked credential pushed would otherwise ride into the box under the
// next signed one; signed by a key the box did not list when it last
// applied gets the message that names the principal the incoming file
// gives that key, because the commit that adds the key must apply
// before the commits it signs, or says the new file does not list it
// either; a listed key whose signature does not verify keeps its own
// message. An error that is not a verdict — ssh-keygen could not run,
// the deadline passed — is returned as it is, never as a refusal.
func VerifyChain(ctx context.Context, v *Verifier, chain []*Commit, installed, incoming Signers, baseline string) (string, error) {
	budget := v.ChainTimeout
	if budget == 0 {
		budget = defaultChainTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// The allowed_signers files are rendered once for the chain.
	allowedInstalled, err := installed.AllowedSigners()
	if err != nil {
		return "", err
	}
	var allowedIncoming []byte
	principal := ""
	for i, c := range chain {
		p, err := v.verify(ctx, c, installed, allowedInstalled)
		if err == nil {
			if i == 0 {
				principal = p
			}
			continue
		}
		var r *Refusal
		if i == 0 || !errors.As(err, &r) {
			return "", err
		}
		switch r.code {
		case codeUnlisted:
			if allowedIncoming == nil {
				if allowedIncoming, err = incoming.AllowedSigners(); err != nil {
					return "", err
				}
			}
			name, err := v.verify(ctx, c, incoming, allowedIncoming)
			var again *Refusal
			switch {
			case err == nil:
			case errors.As(err, &again) && again.code == codeAltered:
				return "", err
			case errors.As(err, &again):
				name = "not in the new Caddyfile either"
			default:
				return "", err
			}
			return "", refuse("%s, between the commit this box runs and %s, is signed by a key this box did not list when it last applied (%s); the commit that adds the key must apply first — force main back to it, let the box apply it, then push the rest",
				c.ID, chain[0].ID, name)
		case codeAltered:
			return "", err
		default:
			return "", refuse("%s, between the commit this box runs and %s, is not signed; every commit on main must be — rebase it out of the history and force-push; the box still runs %s, so no baseline change is needed",
				c.ID, chain[0].ID, baseline)
		}
	}
	return principal, nil
}
