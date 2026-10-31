#!/bin/sh
# Every lane, one after another, each logged; a summary line per lane.
# Usage: sh docs/design/backups/tools/lanes.sh <dir for the logs>; tail <dir>/lanes.summary.
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
L=$1
for lane in test lint secretscan test-integration package install-test e2e-backup e2e; do
	t0=$(date +%s)
	make $lane >"$L/lane-$lane.log" 2>&1
	rc=$?
	echo "$lane exit $rc ($(( $(date +%s) - t0 ))s)" | tee -a "$L/lanes.summary"
done
echo "LANES DONE at $(git rev-parse --short HEAD)" | tee -a "$L/lanes.summary"
