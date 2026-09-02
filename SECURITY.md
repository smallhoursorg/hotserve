# Security policy

hotserve runs as a privileged system service and supervises your
application processes, so security reports are taken seriously and
handled with priority.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting:

**[Report a vulnerability](https://github.com/smallhoursorg/hotserve/security/advisories/new)**

This opens a private draft advisory that only you and the maintainer
can see. Please do not open a public issue for anything you believe is
a security problem — a public report on a deploy server puts every
operator at risk before a fix exists.

What helps: the config that reproduces it (redact your secrets), the
version (`hotserve version`), and what an attacker gains. Proof-of-
concept payloads are welcome inside the private advisory.

## What to expect

- Acknowledgement within a few days — this is a solo-maintained
  project, so "days" is honest rather than aspirational.
- If confirmed, a fix is developed in the private advisory fork,
  released, and the advisory published with credit to you (unless you
  prefer otherwise).

## Supported versions

Pre-1.0: only the latest release receives security fixes. Upgrading is
a package install and a service restart; deployed apps keep serving
through it.

## Scope notes for researchers

- Anything reachable from the webhook endpoint is squarely in scope.
  Deploys are authenticated by a verifiable token (`deploy_trust`:
  CI OIDC or a local public key), so there is no shared deploy secret
  on the box to steal; a forged or replayed token, or a way to deploy
  outside the configured trust claims, is in scope — as is any deploy
  reaching outside `artifact_allowlist` (see
  [liveswap/README.md](liveswap/README.md)).
- Apps deployed by liveswap share one UID. **What is in scope depends
  on the tier the instance reports** (`"sandbox"` in its status), which
  is the only claim hotserve makes:
  - `full` — each app runs in its own user and PID namespace behind a
    deny-by-default filesystem view
    ([liveswap/DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md)). A way
    for one app to read another's files, environment, sockets or
    `/proc`, or to escape to the *supervisor or system*, is in scope.
  - `none` — either `sandbox off` for that app, or a host the probe
    found cannot deliver the namespaces (a container, an LXC VPS, a
    kernel built without them). Cross-app access is then **expected**,
    not a vulnerability: that is the pre-sandbox behaviour the setting
    and the probe both announce, loudly, at every launch. Escapes to
    root or outside the machine remain in scope.
  Known limits at every tier: an app reaches its **own** secrets and
  database by definition, and sibling apps share a network namespace,
  so one can connect to another's localhost port. The full
  attacker/asset/attack-path analysis is in
  [DESIGN-threat-model.md](DESIGN-threat-model.md).
- Reports against a host outside the supported matrix (Debian 13) are
  welcome but are triaged as bugs, not advisories: the sandbox is
  probe-gated, so such a host reports `none` rather than claiming an
  isolation it does not have.
- Vulnerabilities in Caddy itself belong upstream:
  [caddyserver/caddy security policy](https://github.com/caddyserver/caddy/security/policy).
