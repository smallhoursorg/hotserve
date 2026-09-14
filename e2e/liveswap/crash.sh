#!/bin/sh
# The "crash" release's ./server: prints its secret (as a careless app
# does) and exits 3 before ever listening. The failed deploy's response
# must carry the exit status and these lines with the secret redacted.
echo "starting with SECRET=$SECRET"
echo "fatal: cannot start" >&2
exit 3
