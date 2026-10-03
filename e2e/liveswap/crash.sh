#!/bin/sh
# The "crash" release's ./server: prints its secret (as a careless app
# does) and exits 3 before ever listening. The failed deploy's response
# must carry the exit status and these lines with the secret redacted.
# The fatal line comes from a child, as a runtime's error often does:
# the shell reaps it at once, and journald, reading the line after, can
# no longer tell its unit — the response must carry it all the same.
echo "starting with SECRET=$SECRET"
sh -c 'echo "fatal: cannot start" >&2'
exit 3
