# The boxes' config, checked here and applied by the workflow on main.
# See README.md.
HOTSERVE ?= hotserve
export HOTSERVE

.PHONY: check signer wip

# Every box here is one hotserve will take, and a line for each. Needs a
# hotserve binary: Linux only (the release tarball's, as
# .github/workflows/apply.yml installs it). On a Mac, the workflow runs
# it on every push and pull request.
check:
	bin/check

# Your SSH key into every box's signer list, and this checkout's commits
# signed with it:
#   make signer [KEY=~/.ssh/id_ed25519.pub] [PRINCIPAL=you@example.com]
signer:
	KEY="$(KEY)" PRINCIPAL="$(PRINCIPAL)" bin/signer

# A commit left unsigned, for config not yet meant to run: no box
# applies a history with an unsigned commit in it, whoever lands it on
# main (README.md, "The rules").
wip:
	git -c commit.gpgsign=false commit
