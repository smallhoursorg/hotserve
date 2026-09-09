#!/bin/sh
# The "probe" release's ./server: sources the shared view probe
# (liveswap/testdata/sandbox-view.sh — the same file liveswap's integration test and
# packaging/test/smoke.sh use), then becomes the app so the deploy's
# health gate passes like any other release.
#
# Sourced, not run as a child, so the probe reports $$ as this unit's
# main pid.
#
# e2e is the one lane with a resolvable name to look up, so it is the
# one that supplies PROBE_DNS_NAME. It supplies no MGR_PID — its
# Caddyfile is baked at image build and the manager's pid is a runtime
# value — so the two /proc checks report "skipped" here, and the
# systemd suite asserts exactly that. Those routes are covered from
# outside the unit instead (denied_to_hotserve, systemd.sh).
PROBE_DNS_NAME=e2e-artifacts
export PROBE_DNS_NAME
. ./sandbox-view.sh
exec ./server-bin
