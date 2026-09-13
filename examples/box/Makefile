# The box's config, checked here and pushed to the box. See README.md.
HOTSERVE ?= hotserve

.PHONY: check push

# A full config check with a hotserve binary: Linux only (the release
# tarball's, as .github/workflows/check.yml installs it). On a Mac
# there is none; `make push` runs the same check on the box. (No
# `hotserve fmt` here: it reflows the backslash-continued flag lines.)
check:
	$(HOTSERVE) validate --config Caddyfile

# Validate on the box, show the diff, ask, swap the file in, reload —
# as an ordinary user there, with the sudoers file beside this one.
push:
	bin/push
