# DESIGN — box

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- box/DESIGN-box.md`); the
"History" section at the end holds only dated one-liners. Amendments
are for decisions still fresh or contested and get folded into the
body once they settle.

Status: design, written before the code. The behaviour specification
is normative for the PRs that build it (proof core, Caddyfile surface,
applier, `init`, template, docs); a PR that cannot hold a line here
changes the line in the same PR, with the reason. Until the applier
ships, `examples/box/bin/push` is how config reaches a box.

`box` is the subsystem that writes `/etc/hotserve/Caddyfile`. It is
not liveswap's: applying the whole file covers Caddy sites, the cache,
penaltybox and liveswap alike. It sits beside liveswap and penaltybox,
imports liveswap's deploy-trust verifier (the allowed direction), and
owns three things: the `box` global option and `box_webhook` site
directive, the root applier unit, and `hotserve init` / `hotserve box
…`. The wider attack surface it changes is in
[DESIGN-threat-model.md](../DESIGN-threat-model.md), "Config webhook"
and T6.

## Why this subsystem exists

The Caddyfile is where the box's policy lives: which repository may
deploy each app, each Deno app's permission flags, which hosts are
served, and — with this subsystem — who may change the file itself.
Before it, the file reached the box by a hand edit as root on day 0
and afterwards by `examples/box/bin/push`: a laptop script over SSH as
an administrator whose sudoers file allowed eight fixed commands. That
shape could not promise three things.

- **The change was whatever the laptop sent.** `bin/push` pushed the
  file beside it. Nothing tied the bytes to a commit, a review or a
  person; the README asked for "merge, then push from a checkout of
  main" as a ritual.
- **The administrator was a standing privileged account.** Eight
  sudoers lines, a group, an SSH key, and a "prove `sudo -n` works
  before you close root" step on page 2 of the tutorial. Secrets rode
  the same account (`sudoedit`).
- **The box could not be told who it belonged to.** A compromised
  laptop, a leaked GitHub token or an OAuth app with `contents:write`
  all had routes to the file, and the box had no way to tell them from
  the operator.

With `box`, the file is written in exactly one place — a private
repository of the operator's — and the box applies a commit only if
(a) a token proves it came from that repository's `main` workflow,
(b) an SSH key the box lists signed the commit, (c) the file sent is
the file in that commit, and (d) the commit descends from the one the
box runs. Root on the box is used twice in a box's life: to install
the package, and to tell the box which repository it belongs to.

## The shape

The repository holds one directory per box; the box is one file:

```
box1/Caddyfile                 # the whole config
.github/workflows/apply.yml    # validates every push and PR; applies main to the box
Makefile                       # make signer, make check
```

The Caddyfile's global options carry a `box` block beside `admin`, and
exactly one site carries `box_webhook`:

```
{
	admin unix//run/hotserve/admin.sock

	box {
		deploy_trust github {
			audience hotserve
			claim repository your-org/boxes
			claim ref refs/heads/main
		}
		# signers
		signer alice@example.com ssh-ed25519 AAAAC3Nz…
	}

	liveswap { … }
}

example.com { reverse_proxy { dynamic liveswap example } }

deploy.example.com {
	liveswap_webhook     # deploys: POST /<app>
	box_webhook          # this file: POST / from the workflow
}
```

- `deploy_trust` is liveswap's grammar and verifier, unchanged
  (`parseDeployTrust`, liveswap/caddyfile.go; `authorize`,
  liveswap/deploytrust.go): GitHub, GitLab, generic OIDC and the local
  key come for free, and the 401, the 429 and the `refused` journal
  line are the same shape as a deploy's.
- `signer <principal> <key-type> <base64>` is one person's SSH public
  key. The principal is the name the result and the journal carry; it
  matches `^[A-Za-z0-9._@+-]+$` and nothing else (allowed_signers
  principals are patterns with `*`, `?`, `!` and `,`; none of those
  may appear). Key types: `ssh-ed25519`, `ecdsa-sha2-nistp256/384/521`,
  `sk-ssh-ed25519@openssh.com`, `sk-ecdsa-sha2-nistp256@openssh.com`,
  `ssh-rsa`. The base64 must decode to a key of the declared type.
- `box_webhook` takes no arguments. The site that carries it has
  exactly one address, and that address is a bare hostname: no
  scheme, no port, no path, no wildcard, no placeholder, no second
  name after a comma. `deploy.example.com` qualifies;
  `https://deploy.example.com:8443`, `deploy.example.com,
  deploy2.example.com` and `{$DEPLOY_HOST}` do not, and `init`, the
  applier and `hotserve box webhook` refuse them with the same
  message. That one hostname is the address the workflow posts to
  (`https://<host>/`) and the box's identity in the repository (see
  "The box's identity"); the directory name (`box1`) is a label the
  operator chooses.
- `box` and `box_webhook` are read by the applier from the raw token
  stream (see "Reading the signed file"), so both must be written in
  the file itself, never reached through `import`.

## Behaviour specification (normative)

The chain, in order. A step that fails stops the chain; the file on
disk is unchanged; the result names the step (messages in "Refusals").

**In the hotserve process (the `box_webhook` handler, uid hotserve):**

1. **Token.** `Authorization: Bearer <JWT>` is verified against the
   `box` block's `deploy_trust` sources exactly as liveswap verifies a
   deploy token: signature, `iss`, `aud`, `exp`, then the claim
   allowlist. The flat 401, the per-address and process-wide failure
   budgets, the 429, the `webhook auth failed` line with `refused` and
   the `could not consult a trust source` line are liveswap's, through
   one exported entry point both handlers call; the budgets are shared
   (one per client address across both webhooks).
2. **Method and body.** `GET /` and `GET /?result=<id>` answer status
   (below). `POST /` takes one gzip tarball — the *bundle* — of at most
   16 MiB, declared as `application/gzip`, `application/x-gzip` or
   `application/octet-stream`; another method is 405, another media
   type 415, a larger body 413, all after authentication. One push at
   a time: a second `POST` while one is pending is 409.
3. **Bundle.** Parsed from memory under strict rules: regular files
   only; fixed names — `path` (the file's path in the repository, as
   the workflow knows it), `Caddyfile`, `commit`, `parents/NNNN`,
   `trees/<sha>` — and nothing else; per-type caps (below). The 16 MiB
   cap is on the *decompressed* stream, which the reader stops at, as
   well as on the body; a bundle is a few kilobytes. The same parser
   runs in the applier.
4. **Token ↔ bundle.** When the token carries a `sha` claim (GitHub
   does), it must equal the bundle's commit id, so a valid token
   cannot carry some other signed commit, and a workflow that bundled
   the wrong HEAD is told so. This needs the verified claims, not only
   liveswap's attribution string: the shared entry point returns both
   (`by` and the claim map), and liveswap's own handler ignores the
   map.
5. **Signature pre-check.** The commit's signature is verified against
   the signers of the Caddyfile the box runs (`/etc/hotserve/Caddyfile`,
   world-readable). This is a courtesy, not the boundary — the applier
   repeats it as root — but it means step 6 is reachable only by a
   listed signer, not by anyone who can pass step 1.
6. **Validate.** `/usr/bin/hotserve validate --config <staged Caddyfile>`
   runs as a bounded child of the hotserve process, and so does
   `/usr/bin/hotserve-backup validate <file>` when `/usr/bin/hotserve-backup`
   is executable (`test -x` exit 1 is "not installed"; any other
   failure is "could not tell", which refuses). Each child gets the
   environment the service runs with — the hotserve process's own, as
   `hotserve.service` set it — so a `{$VAR}` line expands as it will
   at reload and no more; their output reaches the response and the
   journal only through liveswap's redactor primed with that same
   environment, because Caddy's errors quote expanded values. Root
   never runs either: `caddy validate`
   provisions every module (liveswap's `App.Provision` builds OIDC
   clients and warms JWKS over the network), and the Caddyfile adapter
   expands `{$VAR}` from the caller's environment and runs each
   module's `UnmarshalCaddyfile` on the input.
7. **Drop.** The bundle is written as one regular file in `stage/`,
   then `rename`d into `in/` as `<id>.tar`. Nothing but a complete
   bundle ever appears in `in/`. The handler writes the audit line for
   the request — `box push accepted` with `id`, `commit`, `by` (the
   token's attribution, as a deploy's `deployed_by`) and `remote` — to
   the journal; nothing about the caller is handed to root, whose
   record names the signer it proved, not the token the handler saw.
8. **Answer.** The handler waits at most 30 s for the applier's
   `verified` result and answers: 422 with the refusal; 200 `no_change`
   when the file already runs (nothing reloads); 202 with `{id, commit}`
   when the applier is about to install the file; 504 with `{id}` when
   no result arrived in time (the apply continues; `GET /?result=<id>`
   has the outcome). The handler never waits past `verified` — see
   "Why 202 and a poll".

**In the applier (`hotserve box apply`, root, one shot, path-triggered):**

9. **Recover, then take.** Under an exclusive lock, the applier first
   settles whatever a crash left behind, before it looks at `in/`. If
   `/etc/hotserve/Caddyfile.prev` exists, an install was interrupted
   between the swap and a successful reload: the previous file is
   renamed back over `Caddyfile` and hotserve reloaded, exactly as a
   failed reload is handled (step 19) — `applied.json` is written only
   after a reload succeeds, so it still names the previous commit and
   nothing disagrees. Every entry left in `work/` is then given a
   terminal result, `failed` — "interrupted; the previous Caddyfile is
   on disk and running" — and removed; a bundle is never resumed,
   because it was read once and a crash is not a reason to read it
   again (the workflow re-runs the push). Only then is every entry of
   `in/` renamed into `work/` and processed in lexical order of id. An id is 32
   lowercase hex characters: the first 16 are the handler's clock in
   nanoseconds when it accepted the request, the last 16 random, so
   lexical order is arrival order by the handler's clock. The order
   matters only when more than one bundle is pending (a crash left
   some behind; the handler admits one at a time) and only for which
   is tried first — the descent check (step 14) decides each outcome
   regardless. `in/` is empty before any work starts and on every
   exit, whatever happens. Each bundle is opened
   `O_NOFOLLOW|O_NONBLOCK`, checked `S_ISREG` and under the cap, read
   once into memory, and never touched on disk again: a writer holding
   a descriptor from before the rename cannot change what was
   verified.
10. **The installed file.** `/etc/hotserve/Caddyfile` is read once into
    a buffer that is both the source of the signer list and the
    rollback copy. Its signers come from the raw token stream, never
    from the running configuration (which the hotserve uid authors
    through the admin socket) and never from an imported file. An
    empty signer list refuses: the box's own file is the only
    authority, and a file with no signers has no authority.
11. **Identity.** The incoming file's single `box_webhook` site host
    must equal the installed file's. Another box's file is refused by
    name.
12. **Signature.** The commit's `gpgsig` is an SSH signature over the
    commit object with that header removed (git's rule, continuation
    lines included). An allowed_signers file is generated from the
    installed signers, each line `<principal> namespaces="git" <type>
    <base64>`, and `ssh-keygen -Y find-principals` then `ssh-keygen -Y
    verify -n git -I <principal>` run as uid 65534 on the payload with
    an environment of `PATH` alone; the verdict is the exit status
    alone. A commit with no `gpgsig`, an
    OpenPGP `gpgsig`, a `gpgsig-sha256`, or a key not listed is refused
    by name — the OpenPGP case tells the operator GitHub's merge button
    signed it.
13. **The file is the committed file.** HEAD's id is computed, never
    read: `sha1("commit <n>\0" + raw)` over the bundled `commit`
    object. It is the id the result and `applied.json` carry, the id
    the token's `sha` claim must equal when that claim is present
    (step 4, in the handler), and the id the chain (step 14) starts
    from; the bundle's own name is the 32-hex request id and names no
    commit. `sha1("tree <n>\0" +
    raw)` of the root tree must be the commit's `tree`; the path's
    components (the bundle's `path`, a hint only — it carries no
    trust, and is checked to be safe components before use) are walked
    through the bundled trees, at most 32 deep; the
    last entry must be a blob (mode `100644` or `100755`) whose id is
    `sha1("blob <n>\0" + file)`. A symlink, a submodule or a tree at
    the path is refused. SHA-256 repositories (64-hex ids) are refused
    with a message; plain `crypto/sha1`, not sha1dc, is an accepted
    residual (a chosen-prefix collision needs the operator to sign a
    crafted tree).
14. **Descent, every step signed.** `applied.json` names the baseline:
    the commit the box runs. The bundle's `parents/` chain is walked
    first-parent from HEAD; it must reach the baseline within 500
    commits, and **every commit on it above the baseline must carry a
    signature that step 12 accepts** against the installed signer
    list, not HEAD alone. Otherwise an unsigned commit pushed by a
    leaked credential — refused on its own push — would ride into the
    box under the next signed commit on top of it, since HEAD's tree
    contains whatever that commit changed. The price is that one
    unsigned (or OpenPGP-signed) commit on `main` stops every later
    push until it is removed from the first-parent history: rebase it
    away and force-push. The baseline sits below the removed commit,
    so the rewritten history still descends from it and the next push
    applies — `hotserve box baseline` is **not** part of this recovery,
    and must not be: moving the baseline up to a signed HEAD empties
    the chain and skips every ancestor below it, which is the ride-in
    this step exists to prevent. The refusal names the commit and says
    so. A merge commit a signer signed vouches for the
    branch it merges, whose own commits are off the first-parent line
    and not examined. HEAD equal to the baseline passes (an empty
    chain) and step 16 decides whether there is anything to do. A
    chain that misses the baseline is a history that no longer
    contains the commit the box runs — a rewind, a replay of an older
    signed commit, or a rebase that rewrote the baseline itself — and
    all are refused the same way. Only that case is what `hotserve box
    baseline` is for: a trust reset by root at the console, naming a
    commit whose ancestors the box will then never examine, so the
    message says that too.
15. **Never cut the branch you sit on.** The incoming file must have a
    `box` block with at least one `signer`, a `deploy_trust` that
    parses non-empty, exactly one site carrying `box_webhook`, and must
    still list the key that verified HEAD. Rotating a key is therefore
    two *applied* pushes — add the new key, let the box apply it, then
    remove the old one — because a single push carrying both commits
    has its second commit signed by a key the box does not list yet
    (step 14). This guard checks presence, not reachability; a typo in
    a `claim` or a key that does not match any laptop passes it. The
    guarantee behind it is the console ("At 3am").
16. **Change?** The incoming buffer is compared with the installed
    one, whatever the commits say: identical means `no_change`, nothing
    is written, nothing reloads, and `applied.json` still advances to
    HEAD (the chain moved; the box should know). Different with HEAD
    equal to the baseline is the console edit of "At 3am" being
    overwritten by a re-run: the repository is the truth.
17. **`verified`.** hotserve must be active (`systemctl is-active`);
    if it is not, the push is refused here — "nothing applied" — a
    deliberate departure from `bin/push`, which left a validated file
    for the next start with a person watching; here nobody is, and the
    path unit may fire at boot before hotserve is up. Then the result
    file is written, before anything changes on disk, so the handler
    can answer (step 8). The applier does not wait for that answer:
    it goes on to install while the handler is still polling, and the
    file stays readable through the reload, so the handler returns
    within one poll interval and a reload that has already begun waits
    that long for the request, not the handler's whole timeout. Every
    refusal precedes `verified`, as does `no_change` (step 16, which
    writes its terminal result directly); after `verified` the only
    phases are `applied`, `failed`, `rolled_back` and `unknown`, each
    terminal, each saying what is on disk and what is running.
18. **Install.** First the rollback copy: the buffer from step 10 is
    written to a temporary name and renamed to `Caddyfile.prev`, and
    only once that rename has returned is the new file written to a
    temporary name in `/etc/hotserve/` (mode 0644, root) and renamed
    over `Caddyfile`. A failure writing either temporary (`ENOSPC`,
    say) leaves `Caddyfile` untouched, removes whatever temporary was
    made, and ends the push as `failed` — "the install failed before
    the Caddyfile changed; nothing changed" — a terminal phase after
    `verified`, not a refusal; the live file never changes without
    its restore copy already in place.
19. **Reload.** `systemctl reload hotserve` — which runs the unit's
    `ExecReload` (`hotserve reload --config /etc/hotserve/Caddyfile
    --force`) as the hotserve user — with no applier-side timeout
    shorter than systemd's own (`TimeoutStartSec=240s` on
    hotserve.service bounds the reload). Success: `.prev` is removed,
    `applied.json` records HEAD, the result is `applied`. Failure:
    `.prev` is renamed back over `Caddyfile` and the reload is run
    **again**, so that the file on disk is the one running by
    construction, not by inference from an exit status; the result is
    `rolled_back`. If the second reload also fails the result is
    `unknown`, the journal says so at warning level, and the file on
    disk is the previous one.
20. **Result.** `out/<id>.json` is rewritten (temporary name, rename)
    with the final phase. The applier sweeps `work/` and `out/` by age
    and exits with `in/` empty.

Running apps are never restarted by an apply, as with any reload
(liveswap's "reload trap"): a changed `app` block applies at the app's
next launch.

### Why 202 and a poll

A request that waits for the apply to finish while the applier runs
`systemctl reload` never completes. `hotserve reload` posts to the
admin API, which provisions the new configuration and then stops the
old one; stopping the HTTP app calls `http.Server.Shutdown` with no
deadline (no `grace_period` is set, and setting one would turn the
wait into a cut connection), and `Shutdown` waits for every in-flight
request — including the one waiting for the reload. The cycle breaks
only at the handler's timeout. So the applier publishes `verified`
before it reloads and does not wait to be read: the handler, polling
`out/`, sees it within one interval and answers 202, and if the
reload has already begun, `Shutdown` waits that one interval for the
request to finish rather than the handler's timeout. The workflow then
asks `GET /?result=<id>` — a fresh request, served by whichever
configuration is running — until the phase is final. A push that
changes nothing is answered 200 synchronously, because nothing
reloads. liveswap never met this: a deploy does not reload Caddy.

### Reading the signed file

The applier and `init` read the Caddyfile with `caddyfile.Tokenize`
and a brace-depth walk: the key-less top-level block, its `box` block,
that block's `signer` and `deploy_trust` tokens; each site block's
address tokens and whether `box_webhook` stands at directive
position. Tokenizing expands nothing and follows nothing — `{$VAR}`
stays literal, `import` is a token — which is what makes it safe to
run as root on pushed input, and why `box` and `box_webhook` must be
literal in the file. The same walk is what `hotserve box webhook`
prints the address from and what `make check` prints its mapping line
from, so there is one reading of the file, not three.

## Store rules (a trust boundary in both directions)

Written before the code, as liveswap's deploy-record store was.

| Path | Mode | Owner | Rule |
|---|---|---|---|
| `/etc/hotserve/Caddyfile` | 0644 | root:root | Written only by root: `init`, the applier, the console. Read by hotserve (serving), by the handler (signers for the pre-check) and by the applier (the signer list and the rollback copy). The conffile the package ships. |
| `/etc/hotserve/Caddyfile.prev` | 0644 | root:root | Written and renamed into place before `Caddyfile` is replaced (step 18); removed when the reload succeeds (step 19). Exists only between the two — so its presence at startup is the marker of an interrupted install, and the applier (step 9) and `init` both restore it before anything else. |
| `/etc/hotserve/age/` | 0700 | root:root | Reserved, empty, created by `init`; the secrets PR puts the box's age key here. |
| `/var/lib/hotserve-box/` | 0750 | root:hotserve | Created by `tmpfiles.d`, not by either process. Not under `/var/lib/hotserve`, which is 0750 hotserve:hotserve and which a root unit without `CAP_DAC_OVERRIDE` cannot traverse. Joins `sandboxHotservePaths` (liveswap/sandbox.go): never a bind source. |
| `…/stage/` | 0700 | hotserve:hotserve | The handler assembles a bundle here. Not watched. |
| `…/in/` | 0770 | root:hotserve | The handler renames a complete bundle in; the applier renames everything out before reading anything. `DirectoryNotEmpty=` watches it, so it must be empty on every applier exit, or the path unit re-triggers until its start-rate limit fails it. |
| `…/in/<id>.tar` | 0644 | hotserve:hotserve | `<id>` matches `^[0-9a-f]{32}$`: 16 hex of the handler's nanosecond clock, then 16 random (step 9); a name that does not match is moved to `work/` and refused like any other. The content is a public commit; 0644 so root reads it without DAC capabilities. |
| `…/work/` | 0700 | root:root | Bundles land here by rename and are read once. Never a source of truth after that read. |
| `/tmp/box-verify-<id>/` (the unit's `PrivateTmp`) | 0755 / files 0644 | root:root | The payload, the signature and the generated allowed_signers for `ssh-keygen` running as uid 65534, which cannot traverse `work/` (0700) or `/var/lib/hotserve-box` (0750 root:hotserve). The private `/tmp` is gone with the unit. |
| `…/out/` | 0750 | root:hotserve | Results, written to a temporary name and renamed so the handler never reads a partial file. Root sweeps by age; hotserve only reads. |
| `…/out/<id>.json` | 0640 | root:hotserve | `{id, phase, commit, signer, path, diff, apps, box_webhook, error, caddyfile_edited_out_of_band}`. The `diff` passed the redactor. |
| `…/applied.json` | 0640 | root:hotserve | `{sha, sha256, signer, when}`, written to a temporary name and renamed, and only after a reload has succeeded (or, in `init`, after the file is installed on a box where hotserve is not running): the baseline, the installed file's digest (an out-of-band edit is visible, not refused — root's file is root's), the principal whose key root verified. Nothing the handler reported about the caller is in it; that is the handler's journal line. The box's host is read from the installed file, never recorded. |
| `…/lock` | 0600 | root:root | `flock`; one apply at a time. |

Caps: Caddyfile and blob ≤ 1 MiB; a tree object ≤ 1 MiB; a commit
object ≤ 64 KiB; chain ≤ 500 commits; tree depth ≤ 32; `path` ≤ 4096
bytes of safe components; whole bundle ≤ 16 MiB *decompressed* (the
gzip reader stops there; the per-type caps add up to more, so the
total is the bound that wins) and ≤ 16 MiB as a body; ssh-keygen
output bounded into the error text; every child process has a
deadline and a `WaitDelay`.

Nothing read from `in/`, `work/` or the bundle is trusted for anything
but its bytes, and no bytes from a bundle reach a result's `error`
field: refusals quote ids, principals and hosts the applier derived,
never file content. The `path` hint is validated as safe components
before it is used to walk trees.

## The box's identity

The repository may hold several boxes; both files sit in the same
signed commit, so the proof alone cannot tell box1's file from box2's.
The box therefore requires the incoming file's `box_webhook` hostname
to equal the installed file's: already in the signed file, already
what the workflow derives the POST address from, already bound to one
box by DNS. Because the site's address is required to be exactly one
bare hostname (see "The shape"), the comparison is of two lowercase
hostnames and nothing else; a scheme, a port or a second address is
refused before it can make two spellings of one box, or one spelling
of two. Shipping box2's file to box1 is refused as "this file is for
deploy2.example.com; this box is deploy.example.com".

The directory name is not an identity. It is a label the operator
picks, `box1` in the template; `make check` and the run print
`box1/Caddyfile → https://deploy.example.com/ (1 app, 1 signer)` so
the mapping is always in view, and the README may suggest naming the
directory after the host but never enforces it. Renaming the deploy
host is a console step — `hotserve init` again from a copied
directory — which is honest: the new host has no certificate until the
box serves it, so the rename could not be a plain push in any case.

## Handler contract

- `POST /` — the bundle. Answers as in step 8. `Content-Type:
  application/gzip` (or `x-gzip`, `octet-stream`); anything else 415.
- `GET /` — `{"commit": "<sha>", "box_webhook": "<host>", "sha256":
  "<hex>"}` from `applied.json` and the installed file. The workflow
  reads `commit` to bundle exactly `git rev-list --first-parent
  <commit>..HEAD`.
- `GET /?result=<id>` — the result, or 404 once swept.
- Anything else — 405. Every answer passes liveswap's response filter
  (the shape and entropy layers; a `diff` additionally passes the
  environment layer in the applier before it is written).
- Auth precedes everything, including existence: an unauthenticated
  request learns nothing but 401.

## `hotserve init <dir> <sha>` and `hotserve box baseline <sha>`

`init` is the second and last thing root does over SSH. It refuses
unless it is root; reads `<dir>/Caddyfile`; refuses, by name, a file
with no `box` block, no `signer`, no site carrying `box_webhook`, more
than one, a `box_webhook` site whose address is not exactly one bare
hostname (a scheme, a port, a path, a wildcard, a placeholder, a
second name), or `box`/`box_webhook` reached through `import`;
requires a 40-hex `<sha>` — the commit the operator is about to push,
so no null baseline ever exists; runs `hotserve validate` as the
hotserve user, never as root (the same reasoning as step 6), with the
environment `hotserve.service` gives the service and nothing of
root's shell — dropping the uid does not drop the environment, and
the adapter would expand `{$VAR}` from whatever it inherited; then
runs the applier's own install transaction (steps 18 and 19, and the
recovery of step 9 first if a `Caddyfile.prev` is lying there): the
existing file, when there is one, is renamed to `Caddyfile.prev`
before the new file lands 0644 root by temporary name and rename;
`/etc/hotserve/age/` is created; hotserve is reloaded if it is active,
and on a failed reload the previous file is renamed back, reloaded
again, `applied.json` left as it was, and `init` exits non-zero
saying so — a re-run of `init` as the break-glass must not leave a
box that cannot restart. Only after the reload succeeds is
`applied.json {sha, sha256}` written; when hotserve is not running it
is written on install and `init` says the file loads at the next
start, since a person is at the console for `init` in a way nobody is
for the applier. Then it prints what it found and did:

```
init: box1/Caddyfile validates; 1 app (example); box_webhook on deploy.example.com; 1 signer (alice@example.com)
init: installed /etc/hotserve/Caddyfile; this box applies commits descending from 4f1c2a9
init: reloaded. From here, config changes are pushes to your-org/boxes.
```

`hotserve box baseline <sha>` (root) rewrites the baseline only, and
it is a trust reset: the box will never examine `<sha>`'s ancestors,
so it is for the case where `main`'s history no longer contains the
commit the box runs (a rewrite below it, a chain longer than 500
commits), and the messages that refuse those name it. It is not the
recovery from an unsigned commit above the baseline — that is a
rebase and a force-push, after which the unchanged baseline is still
an ancestor and the next push applies. A revert commit is the normal
path for a bad change and needs nothing on the box.

`hotserve box webhook <Caddyfile>` prints `https://<host>/` for the one
site carrying `box_webhook`; it refuses zero such sites, more than
one, or an address that is not exactly one bare hostname, with the
message the workflow shows — the same rule and the same reading of the
file as `init` and the applier, so the three cannot disagree about
which address a file names. It runs on the laptop and in CI, so it
reads tokens, not the adapted config.

## The applier unit

`hotserve-box-apply.path` (`DirectoryNotEmpty=/var/lib/hotserve-box/in`,
`TriggerLimitIntervalSec=10s`, `TriggerLimitBurst=20`, enabled,
`WantedBy=multi-user.target`) starts `hotserve-box-apply.service`
(`Type=oneshot`, `ExecStart=/usr/bin/hotserve box apply`, root, not
enabled on its own). The service carries the backups units' house
style: `ProtectSystem=strict` with `ReadWritePaths=/etc/hotserve
/var/lib/hotserve-box`, `PrivateTmp`, `NoNewPrivileges`,
`RestrictAddressFamilies=AF_UNIX` (`systemctl` over D-Bus; no network),
`SystemCallFilter=@system-service`, `CapabilityBoundingSet=CAP_SETUID
CAP_SETGID` (to run `ssh-keygen` as 65534; nothing else — root's uid
is what writes `/etc/hotserve`. `init` is not this unit: it runs as
root over SSH and drops to the hotserve user for `validate` by
itself), `TimeoutStartSec=10min` (longer than two reloads),
and a reason above every line. `box/units_test.go` parses both shipped
units and holds them to this list.

The path unit fires at boot if `in/` is non-empty from before a
crash; hotserve may not be up yet, and step 17's "not active" refusal
is what that case meets. The applier drains in a loop: it takes every
entry a listing of `in/` shows, processes them, lists again, and exits
only after a listing comes back empty. A bundle that lands between
that last listing and the exit is the one case `in/` is non-empty
when the service stops; systemd re-evaluates `DirectoryNotEmpty=` on
the stop and starts the service once more, which drains it. That is
one re-trigger per late arrival, not a loop. The loop the rate limit
guards against is an entry the applier declines to take — a name it
does not like, a file it cannot open — left in place: so every entry
the applier sees is moved to `work/` whatever it is, and refused from
there. Twenty re-triggers in ten seconds would fail the path unit
until `systemctl reset-failed`; the invariant is what keeps the count
at one.

The package depends on `openssh-client` (for `ssh-keygen`) and ships
the `tmpfiles.d` file; `postinstall.sh` runs `systemd-tmpfiles
--create` and enables the path unit the `deb-systemd-helper` way;
`preremove.sh` stops it.

## Refusals

Each is one line, `refused: …` in the result's `error`, shown by the
workflow as `apply: refused: …`, and written to the journal: by the
handler with liveswap's fields (`refused`, `remote`, `by`) for what it
refuses itself, by the applier with `id`, `commit`, `signer` and `box`
(the host) for the rest. The 401 and 429 bodies are liveswap's and say
nothing more than they do for a deploy.

- `no bearer token` / the flat 401 — liveswap's.
- `a push is already in progress` — 409.
- `the bundle is larger than 16 MiB` — 413; `bundle: <what>` — 422, a
  file or name the format does not allow, a cap exceeded.
- `the token names commit <a>; the bundle is <b>`.
- `<sha> is not signed; the box applies only commits signed by a key
  in its signer list`.
- `<sha> is signed by OpenPGP, not by an SSH key in the Caddyfile this
  box runs; GitHub's merge button cannot land config — merge on a
  laptop and push`.
- `<sha> is signed by a key that is not a signer in the Caddyfile this
  box runs`.
- `<sha2>, between the commit this box runs and <sha>, is not signed
  by a signer; every commit on main must be — rebase it out of the
  history and force-push; the box still runs <baseline>, so no
  baseline change is needed`.
- `the file sent is not <path> in <sha>` / `<path> in <sha> is not a
  regular file` / `<sha> is in a SHA-256 repository, which the box
  does not read` / `bundle: path is not a relative path of safe
  components`.
- `<sha> does not descend from the commit this box runs (<baseline>);
  the history no longer contains it — a rewind or a replay is refused
  on purpose; if main was rewritten below it, hotserve box baseline
  <sha> as root on the box resets trust to <sha>, whose ancestors the
  box will not examine`.
- `the chain from <baseline> to <sha> is longer than 500 commits; run
  hotserve box baseline <sha> as root on the box`.
- `this file is for <host2>; this box is <host1>`.
- `the new Caddyfile has no box block` / `… no signer` / `… no site
  with box_webhook` / `… more than one site with box_webhook` / `… the
  box_webhook site's address is not one bare hostname (<address>); no
  scheme, port, path, wildcard, placeholder or second name` / `… box
  or box_webhook reached through import; write them in the file itself`
  / `… drops the key that signed this commit (<principal>); add the new
  key in one push, let the box apply it, then remove the old one in a
  second push` / `… box deploy_trust: <parse error>`.
- `hotserve validate: <redacted output>` / `hotserve-backup validate:
  <redacted output>` / `could not ask whether backups are installed;
  nothing changed`.
- `hotserve is not running; nothing applied`.
- `the Caddyfile this box runs lists no signer` — the installed file
  was edited out of band into something the box cannot trust;
  `hotserve init` is the way back.

Non-refusal phases: `no_change` (`the box already runs this
Caddyfile`; terminal, no `verified` before it), `verified` (the one
non-terminal phase), and the four terminal phases that follow it:
`applied` (`loaded; running apps are not restarted`), `failed` (`the
install failed before the Caddyfile changed: <error>; nothing
changed`), `rolled_back` (`the reload failed; the previous Caddyfile
is back and running — journalctl -u hotserve -n 50 on the box says
why`), `unknown` (`the reload failed and so did the reload of the
previous file; the previous file is on disk; journalctl -u hotserve`).
The workflow polls until the phase is one of the terminal ones and
exits non-zero for every terminal phase but `applied` and `no_change`.

## At 3am

If a change broke the webhook site itself, the next push cannot reach
the box. The remedy is the one that was always there: the provider's
console as root, edit `/etc/hotserve/Caddyfile` by hand and `systemctl
reload hotserve`, or `hotserve init` again from a directory copied
over. The next signed push overwrites the hand edit, which is the
point: the repository is the truth and the edit is a bridge back to it
(`applied.json`'s `sha256` is how the box notices the edit happened; it
is reported, not refused). A change that validates but misbehaves — a
wrong domain, a flag the app does not expect — is reverted like any
commit: `git revert`, push, and the box runs the previous file in the
time a workflow run takes. A history rewritten *below* the commit the
box runs is `hotserve box baseline`; one rewritten above it (an
unsigned commit rebased away) needs nothing on the box.

## The merge button

GitHub's merge button cannot produce a commit the box applies: a
squash is signed by GitHub's web-flow GPG key, a rebase lands
unsigned, a merge commit is GitHub's. So `main` moves by `git push`
from a signer's laptop, or by `git merge --ff-only` there after a
reviewed pull request — and since every commit on `main` must be
signed by a listed key (step 14), a fast-forward works only when the
branch's commits all are; a contributor who is not a signer is landed
by `git merge --squash` and one signed commit, which is also the
commit whose diff the signer read. Pressing the button anyway does
more harm than a red run: the commit it lands sits on `main` and
refuses every push after it until it is rebased away and
force-pushed (the box's baseline is below it and stays valid; no
console step). The box repository's README states this;
nothing on GitHub enforces it. A `required_signatures` ruleset on
`main` is the one GitHub-side setting that prevents the mistake
rather than reporting it, and the README recommends it; the box does
not depend on it.

## Threat-model deltas

What each actor can and cannot do once `box` is the only writer. The
full placement is DESIGN-threat-model.md, "Config webhook" and T6.

| Actor | Can | Cannot |
|---|---|---|
| Holder of a box-repo OIDC token (the repo's own CI, a compromised action) | Reach the handler; make it validate a Caddyfile a *signer* committed (pre-check first); replay HEAD (a `no_change`); flood `in/` up to one pending bundle (409 after that) | Install anything not signed by a listed key; replay an older signed commit (descent); land another box's file (identity) |
| Leaked PAT, stolen session, OAuth/GitHub App with `contents:write` | Push to `main`; make the run red; stop every later push until its commit is removed from the first-parent history (loud, recoverable; a `required_signatures` ruleset stops it at the push) | Produce an SSH signature by a listed key; ride into the box under a later signed commit (every commit on the chain is checked); therefore change the box |
| Compromised laptop holding a software signing key | Everything the operator can: sign and push any config | Nothing the operator cannot; this is the operator |
| Compromised laptop, hardware-held key (`sk-` types) | Push unsigned or GPG-signed commits (refused) | Sign without a touch; so cannot change the box unattended |
| Supervisor RCE (T5, the hotserve uid) | Drop any bundle; skip validation (the reload fails and rolls back); report "loaded" for a reload that did nothing (the exit status is the hotserve uid's word); read `out/` and `applied.json` | Write `/etc/hotserve/Caddyfile` or `applied.json`; forge a result; make root install an unsigned, non-descendant or other-box file; persist a configuration across a restart (root's file is the authority at the next start, `systemctl restart` the operator's remedy) |
| Root on the box | Everything, by definition | — |

Residuals accepted here and listed in the threat model: `systemctl
reload`'s exit status is trusted for the `applied` phase (the file on
disk is root's regardless); plain `crypto/sha1`; a passing guard that
is not reachability; validate as a courtesy only; the principal
charset rule; GitHub enforces nothing.

## Non-goals

- git on the box (no clone, no deploy key, no egress to github.com):
  the bundle carries the objects.
- Enforcing the merge-button rule on GitHub.
- Secrets: a later PR, after the backups branch lands. `init`
  reserves `/etc/hotserve/age/` so the shape is fixed now: age files
  in the repository, decrypted by the root applier to a fixed per-app
  path, never by the hotserve uid.
- A second config channel (admin API, SSH, laptop CLI).
- Applying anything but the Caddyfile. The bundle's fixed names are
  the whole protocol.

## Open questions (with leans)

1. **Should `applied` require more than the reload's exit status?**
   The root applier could read `/etc/hotserve/Caddyfile`'s digest back
   through `GET /` after the reload and compare. Lean: no — it would
   be root consuming the serving process's word in another form; the
   `systemctl restart` remedy and the root-owned file are the honest
   answer.
2. **Host rename ergonomics.** `init` again is a console step. Lean:
   leave it; a rename needs a certificate first anyway, and a `--host`
   on `baseline` would be a second way to rebind the box.

## History

Dated one-liners; the full text of each is in git.

- 2026-10-08 — Spike: box repo first, laptop-side verify (L2) vs
  box-side (L3); user chose the box-side channel (Option P) with a
  bundle instead of a clone; applier must be root (backups' rule:
  credentials never under the hotserve uid); signing required from the
  first commit; `box` a global option beside `admin`, not under
  liveswap.
- 2026-10-09 — Design written. Decided: `init` takes the baseline sha
  (no null state); the box's identity is its `box_webhook` host, the
  directory name a label; validation runs in the handler, never in
  root; `verified` before reload and a 202 + poll, because a request
  held across `systemctl reload` deadlocks on `Shutdown`; the exchange
  tree at `/var/lib/hotserve-box`; one bundle file per request read
  once into memory; `in/` empty on every applier exit. Review of the
  draft added: every first-parent commit above the baseline must be
  signed (an unsigned commit would otherwise ride in under the next
  signed one), the `path` travels in the bundle, ssh-keygen's files
  live in the unit's private `/tmp`, the is-active check precedes
  `verified`.
