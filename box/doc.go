// Package box is the config channel: the operator's repository is the
// only writer of /etc/hotserve/Caddyfile, and a commit reaches the box
// as a bundle — the file, the commit object, the trees on its path and
// the first-parent chain — that the box proves before root applies it.
// The design is box/DESIGN-box.md; its behaviour specification is
// normative for this code.
//
// This package reads the signed file the way root must read it: from
// its raw tokens, expanding nothing and following nothing (tokens.go).
// The proof core — commit, tree and blob hashing, the chain, the
// signer list and the ssh-keygen verdict — is the pure-Go subpackage
// proof. The Caddyfile surface (the `box` global option and the
// `box_webhook` directive), the root applier and the `hotserve init` /
// `hotserve box …` commands follow in later PRs.
package box
