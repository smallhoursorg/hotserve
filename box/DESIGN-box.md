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
box runs. On the normal path root is used twice in a box's life: to
install the package, and to tell the box which repository it belongs
to. The console stays the way back — a rewritten history (`hotserve
box baseline`), a renamed deploy host or directory (`init` again), a
config that broke the webhook site itself ("At 3am") — and nothing
here promises that it is never needed again.

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
- `box_webhook` takes no arguments. It handles exactly the path `/`
  (with or without `?result=`) and passes every other path to the
  next handler, and its directive order is registered *before*
  `liveswap_webhook`, which is terminal on every path it sees (an
  unknown app is a 404 after liveswap's own auth, and a box token
  fails liveswap's claim allowlist as a charged 401). Put the other
  way round, box pushes would never reach `box_webhook` and ten of
  them would 429 the runner's address; in this order, `POST /<app>`
  deploys pass through `box_webhook` untouched. The site that carries it has
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
- The signed file is the whole configuration: it contains no `import`
  directive, anywhere. An imported file is bytes nobody signed, and
  one imported from a path the hotserve uid can write would let a
  supervisor RCE (T5) change what loads at the next start — the one
  thing the channel exists to deny it. `init`, the handler and the
  applier refuse a file with an `import` token at directive position,
  and the `box` block and `box_webhook` are read from the raw token
  stream (see "Reading the signed file"). Nor can a directive be
  spelled as a placeholder: no token at directive position, in a site
  address or inside the `box` block contains `{$`, because Caddy
  expands `{$NAME:default}` before it tokenizes and `{$UNSET:import}`
  would otherwise be an `import` the literal check never sees.
  Placeholders stay legal in values. Snippets are inlined; a second
  box is a second file.

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
   a time: a second `POST` while one is *pending* is 409. Admission is
   serialised by a non-blocking `flock` on `stage/lock`, taken before
   the pending check and released only after step 7 has written
   `.auth`: a `POST` that finds it held is 409 at once, before it reads
   a body, so two concurrent authenticated pushes cannot both observe
   "no marker" and both parse, validate and enqueue 16 MiB. The lock is
   on a file, not in memory, and `flock` conflicts between distinct
   open file descriptions within one process, so it holds across the
   reload that re-instantiates the handler; a crash releases it with
   the descriptor. Pending itself is a fact on disk — the handler is
   re-instantiated by the very reload a push causes — and it is defined
   as: a `stage/<id>.auth` exists, younger than fifteen minutes (the
   workflow's own poll bound), for which no terminal `out/<id>.json`
   exists. It begins when the handler writes `.auth`, which it does
   only *after* the bundle is in `in/` (step 7) and while it still
   holds the admission lock, and ends at the terminal result; so the T6 bound
   "one pending bundle" holds through the reload, a push that follows
   another within its reload is told 409 and retries, and no crash of
   the handler can leave a marker with no work behind it. The 409 has
   two causes and two messages: the admission lock is held (another
   push is between its pending check and its `.auth`; there is no id
   yet) — "a push is being admitted; retry in a moment"; or a pending
   exists — "a push is pending: <id>, <age> old", with `journalctl -u
   hotserve-box-apply -u hotserve-box-apply.path` for one that never
   ends. Two things block admission, and they bound different
   things. A bundle still in `in/` — the handler can list that
   directory — blocks for as long as it is there, with no age-out:
   that is the durable one-bundle bound, and it holds whether or not
   any marker exists, so a stalled applier means one 16 MiB on disk and
   a loud 409, never a queue growing by one bundle per window. A
   pending *marker* whose bundle root has already taken (`in/` empty,
   no terminal result yet) blocks for fifteen minutes and then stops:
   root holds at most one bundle under its lock, so what it has taken
   cannot accumulate, and a marker that outlives the workflow's own
   poll bound is a stranded result, not pending work. Together: at
   most one bundle in `in/`, at most one in root's hands, and a 409
   that names which it is.
3. **Bundle.** Parsed from memory under strict rules: regular files
   only; fixed names — `path` (the file's path in the repository, as
   the workflow knows it), `Caddyfile`, `commit`, `parents/NNNN`,
   `trees/<sha>` — and nothing else; per-type caps (below). The 16 MiB
   cap is on the *decompressed* stream, which the reader stops at, as
   well as on the body; a bundle is a few kilobytes. The same parser
   runs in the applier.
4. **Token ↔ bundle.** The token must carry a `sha` claim — GitHub's
   does; a local token mints it with `--claims sha=…`; a generic OIDC
   issuer that cannot supply one is refused for `box` with "box tokens
   must name the commit" — and it must equal the bundle's commit id.
   Without it, nothing would tie the bundle to `main`: any signed
   first-parent descendant of the baseline, a signer's unmerged branch
   tip included, would pass steps 12 to 14 and move the baseline off
   `main`. With it, a valid token cannot carry some other signed
   commit, and a workflow that bundled the wrong HEAD is told so. This
   needs the verified claims, not only
   liveswap's attribution string: the shared entry point returns both
   (`by` and the claim map), and liveswap's own handler ignores the
   map. This binding is the handler's alone, and root cannot repeat it:
   the token is not in the bundle, and an OIDC token cannot be verified
   offline — root would have to fetch the issuer's keys, which is a
   network client in a root unit and, through the handler's cache,
   root consuming the serving process's word. So a supervisor RCE
   (T5), which can write to `in/` without the handler, can present
   root any bundle it holds the objects for, and root will install it
   if a signer signed every commit on its chain and it descends from
   the baseline: `HEAD` again (undoing a console edit — "At 3am" says
   the repository wins), or a signer's signed but unmerged branch. It
   cannot present anything a signer did not sign. The threat tables
   carry this as T5's reach; the README rule that keeps unmerged
   configuration unsigned is what keeps that set to `main`'s own
   history.
5. **Pre-verification.** The handler runs the applier's own proof code
   (steps 11 to 13) before anything touches the staged file: the
   commit's signature against the signers of the Caddyfile the box
   runs (`/etc/hotserve/Caddyfile`, world-readable), the commit, tree
   and blob hashes that bind the staged bytes to the commit at the
   bundle's `path`, and the host identity. A signed commit paired with
   other Caddyfile bytes fails here, not in root. The shared verifier
   takes the uid to run `ssh-keygen` as, 0 meaning "as I am": the
   applier passes 65534, the handler passes 0 and runs it as the
   hotserve uid in `hotserve.service`'s own `PrivateTmp` — it has no
   `CAP_SETUID` and needs none, since it is already unprivileged. This
   is a courtesy, not the boundary — the applier repeats every check as
   root — but it means step 6 runs only on bytes a listed signer
   committed, never on input anyone who can pass step 1 chose.
6. **Validate.** `/usr/bin/hotserve validate --adapter caddyfile
   --config <staged file>` — the adapter named, because Caddy
   autodetects it from a basename that starts with `caddyfile` or ends
   in `.caddyfile` and otherwise parses JSON, and the staged name is
   the request id — runs as a bounded child of the hotserve process, and so does
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
   then `rename`d into `in/` as `<id>.tar`, and only then is
   `stage/<id>.auth` written (the digest of a fresh 32-byte poll
   secret, which the answer in step 8 carries, and the time) — in that
   order, so that a crash between
   the two leaves a bundle root will process and a result that ends
   the pending state, never a pending marker with nothing behind it
   (the other order would answer 409 to every push until the marker
   aged out). The cost of the crash window is only that the posting
   token's result fallback is lost for that one push, which a re-run
   restores. Nothing but a complete bundle ever appears in `in/`. The
   handler writes the audit line for
   the request — `box push accepted` with `id`, `commit`, `by` (the
   token's attribution, as a deploy's `deployed_by`) and `remote` — to
   the journal; nothing about the caller is handed to root, whose
   record names the signer it proved, not the token the handler saw.
8. **Answer.** The handler waits at most 30 s for the applier's first
   result and answers by the phase it finds: `refused` → 422 with the
   refusal; `no_change` → 200; `verified`, or any phase that follows it
   (`applied`, `failed`, `rolled_back`, `unknown` — the applier does
   not wait to be read, so a fast transaction may already be terminal
   by the handler's next poll) → 202 with `{id, commit, phase,
   poll_token}`, and the workflow reads the outcome from `GET
   /?result=<id>` bearing `poll_token`, whatever the phase was; no
   result within 30 s → 504 with `{id, poll_token}` (the apply
   continues; the same poll has the outcome). One rule, then: a POST
   that was not refused outright is answered 200, 202 or 504, never a
   terminal verdict of its own; the `phase` in a 202 body is read
   exactly as a poll answer would be — `verified` means keep polling, a
   terminal phase means the outcome is already known and the workflow
   may stop — and a 504 means poll from the start. The handler never
   waits past the first result — see "Why 202 and a poll".

**In the applier (`hotserve box apply`, root, one shot, path-triggered):**

9. **Recover, then take.** Under an exclusive lock, the applier first
   settles whatever a crash left behind, before it looks at `in/`; the
   path unit watches `work/` and the transaction record as well as
   `in/`, so a crash that left either behind starts the applier again,
   at boot included. The record lives in the applier's own root-only
   directory under a name nothing else writes — not
   `/etc/hotserve/Caddyfile.prev`, which `examples/box/bin/push` and
   its sudoers line create, and which would have the path unit race a
   push on a box whose template predates the applier. If the record
   exists, a transaction was interrupted after its record was written
   and before its terminal result was (the record is the last thing a
   transaction removes, step 20). The record is one file,
   `txn.json` — `{id, commit, path, signer, origin (applier|init), prev
   (the previous Caddyfile's bytes, base64), prev_sha256, new_sha256,
   diff (redacted, at most 64 KiB, cut with a note), apps, box_webhook,
   caddyfile_edited_out_of_band (computed at step 10, before anything is
   replaced — a `no_change` crash after `applied.json` is rewritten
   would otherwise lose the digest it was computed from), phase}` — written
   atomically (temporary name, rename, `fsync`) before anything else
   changes and rewritten the same way at each phase change, so there is
   no partial marker: it exists with all its fields or not at all. Its
   `phase` is what recovery reads, not an inference from digests, which
   cannot tell the states apart (before the swap the installed file and
   the embedded previous bytes agree; after a re-run of the same commit following a console
   edit, `applied.json` already names `commit` before the transaction
   begins). The phases, each durable before the step it names begins:
   `no_change` (nothing to install; only the baseline advances, step
   16), `installing` (nothing changed yet), `swapped` (the new file is on
   disk, its reload not yet confirmed), `applied` (the reload
   succeeded; `applied.json` may or may not be written yet),
   `rolling_back` (the reload failed; the previous bytes are being put
   back). Recovery acts on the phase, after checking the installed
   file's digest `d` against what the phase implies:
   - `no_change`: nothing was to be installed; `applied.json` is
     written (idempotent) and the entry gets `no_change`.
   - `installing`: the swap never happened; `failed` — "interrupted
     before the Caddyfile changed; nothing changed". If `d` is
     nonetheless `new_sha256` (the crash fell between the swap and the
     phase write), it is treated as `swapped`.
   - `swapped`: the reload is unconfirmed; the previous bytes are
     written back (temporary name, rename, `fsync`), the phase becomes
     `rolling_back`, and if hotserve is active a reload runs:
     `rolled_back`, or `unknown` if it fails; not running, `failed` —
     "interrupted; the previous Caddyfile is on disk; hotserve is not
     running". One exception, by `origin`: a `swapped` record written
     by `init` on a box where hotserve is not running is finished as
     `applied`, not undone — `init`'s rule is that an inactive box
     counts the swap as applied, because a person is at the console.
   - `applied`: the reload was confirmed; `applied.json` is written
     (idempotent) and the entry gets `applied`.
   - `rolling_back`: the rollback is on disk or about to be (`d` is
     checked and the bytes written back if not); reload if active, with
     the same three outcomes as `swapped`.
   - In any phase, `d` matching neither `prev_sha256` nor `new_sha256`
     means the file changed under the transaction (a console edit
     mid-flight); nothing is written to `/etc/hotserve`, the entry gets
     `unknown` — "the Caddyfile changed during the transaction; it is
     left as found" — and the journal says so at warning level.
   A `work/` entry with no record is one whose transaction ended, or
   never began: if a *terminal* `out/<id>.json` exists, that result
   stands and only the entry is removed; if the result is `verified`
   (the crash fell between steps 17 and 18, before the record landed)
   or there is no result at all, nothing changed, and the entry gets
   `failed` — "interrupted before the Caddyfile changed; nothing
   changed" — rewriting the `verified` result so the workflow's poll
   ends. So "a result with no record" means done only when the result
   is terminal. Recovery fills a terminal result's `signer`, `diff`,
   `apps` and `box_webhook` from the record alone, which carries all
   four (a `no_change` transaction has no `verified` result to take
   them from, and an `init` transaction has no result at all); it
   re-reads no bundle. A record with `origin: init` and no `work/`
   entry writes no result — `init`'s caller saw its exit, or did not —
   and only the journal line says how recovery finished it. In every case the result is
   written first and the record removed after it, so a crash inside
   recovery is recovered the same way. A bundle is never resumed, because it was read once and a
   crash is not a reason to read it again (the workflow re-runs the
   push). Only then is every entry of `in/` renamed into `work/` and
   processed in lexical order of id. An id is 32
   lowercase hex characters: the first 16 are the handler's clock in
   nanoseconds when it accepted the request, the last 16 random, so
   lexical order is arrival order by the handler's clock. The order
   matters only when more than one bundle is pending (a crash left
   some behind; the handler admits one at a time) and only for which
   is tried first — the descent check (step 14) decides each outcome
   regardless. `in/` is empty before any work starts and on every
   exit, whatever happens. Each bundle is opened
   `O_NOFOLLOW|O_NONBLOCK`, checked `S_ISREG`, and read once into
   memory through a reader that stops at the cap plus one byte and
   refuses if it gets there — a `stat` beforehand is a hint, not a
   bound, since a writer holding a descriptor from before the rename
   can append while root reads — and never touched on disk again: that
   same writer cannot change what was verified, because nothing is
   verified from disk.
10. **The installed file.** `/etc/hotserve/Caddyfile` is read once into
    a buffer that is both the source of the signer list and the
    rollback copy. Its signers come from the raw token stream, never
    from the running configuration (which the hotserve uid authors
    through the admin socket) and never from an imported file. An
    empty signer list refuses: the box's own file is the only
    authority, and a file with no signers has no authority.
11. **Identity.** The incoming file's single `box_webhook` site host
    must equal the installed file's, and the bundle's `path` must equal
    the path `init` recorded in `applied.json` (see "The box's
    identity"). Another box's file, or another file for this box's
    host elsewhere in the same tree, is refused by name. An address or
    path quoted in the refusal is bounded the way liveswap's
    `boundRefusal` bounds a token's claims — Go-quoted to ASCII if it is
    not printable UTF-8, then cut to 300 bytes at a rune boundary, in
    that order: it is signer- or
    token-holder-chosen text going into root's journal line.
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
    components (the bundle's `path`, already required by step 11 to be
    the path `init` recorded, and checked to be safe components) are walked
    through the bundled trees, at most 32 deep; the
    last entry must be a blob (mode `100644` or `100755`) whose id is
    `sha1("blob <n>\0" + file)`. A symlink, a submodule or a tree at
    the path is refused. SHA-256 repositories (64-hex ids) are refused
    with a message; plain `crypto/sha1`, not sha1dc, is an accepted
    residual (a chosen-prefix collision needs the operator to sign a
    crafted tree).
14. **Descent, every step signed.** `applied.json` names the baseline:
    the commit the box runs. The bundle's `parents/` chain is walked
    first-parent from HEAD, linked by hash, not by position: `sha1(
    "commit <n>\0" + raw)` of `parents/0001` must equal HEAD's first
    `parent` header, each next entry's id must equal the previous
    entry's first `parent` header, and the walk ends when a first
    `parent` header equals the baseline — whose object is not bundled,
    since the workflow ships `rev-list --first-parent <baseline>..HEAD`.
    An entry nothing references, a gap in the numbering, or an entry
    past the end is refused as a malformed bundle. The chain must reach
    the baseline within 500 commits, and **every commit on it above the
    baseline must carry a signature that step 12 accepts** against the
    installed signer list, not HEAD alone. Otherwise an unsigned commit pushed by a
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
    chain that misses the baseline is refused, and the message names
    the three histories that produce it: one that no longer contains
    the commit the box runs (a rewind, a replay of an older signed
    commit, a rebase that rewrote the baseline itself) — the only case
    `hotserve box baseline` is for, a trust reset by root at the
    console naming a commit whose ancestors the box will never examine;
    a branch that *merged* `main` into itself and was then
    fast-forwarded onto `main`, which puts the baseline on the merge's
    second parent, off the first-parent line (the README's rule is
    rebase onto `main`, never merge `main` into a branch; the fix is a
    rebase and a force-push, and no console step); and a newer push
    that already applied. What the chain rule does **not** stop, and
    the README must say: a push credential cannot sign, but it can
    fast-forward a branch every commit of which a signer already
    signed onto `main`, and the token's `sha` then binds the bundle to
    exactly that tip. The box applies it, because it is what the rule
    asks for. So config a signer does not yet mean to run is committed
    *unsigned* (`git -c commit.gpgsign=false commit`, which `make wip`
    wraps) or in a fork; an unsigned branch is one the chain rule
    refuses whoever fast-forwards it. The exposure has a shape worth
    knowing: a signed commit, once fetched, is the holder's for ever —
    deleting the branch on GitHub removes nothing — but it lands only
    while the box still runs the commit it forked from. Once a later
    commit has *applied*, the stale one's chain never reaches the new
    baseline, and the holder can neither rebase nor merge it forward
    without a signature. The baseline is the commit the box applied,
    not `main`'s tip, so a push that never applied (box down, refused)
    leaves the window open behind a `main` that has moved; a GitHub
    ruleset that blocks force-pushes closes the only route back, and
    the README recommends it beside `required_signatures`. After a
    credential leak the remedy is the usual one — rotate it — plus one
    applied push to `main`, which puts every stale signed object below
    the baseline.
15. **Never cut the branch you sit on.** The incoming file must have a
    `box` block with at least one `signer` and a `deploy_trust` block
    with at least one line in it (presence at the token level — root
    runs no directive parser on pushed input, liveswap's included; the
    handler's validate in step 6 is where a malformed `deploy_trust`
    is caught, as a courtesy), exactly one site carrying `box_webhook`, and must
    still list the key that verified HEAD. Rotating a key is therefore
    two *applied* pushes — add the new key, let the box apply it, then
    remove the old one — because a single push carrying both commits
    has its second commit signed by a key the box does not list yet
    (step 14). If the first push never applied (the box was down, the
    path unit had failed) and the second landed on top, the chain is
    stuck with a legitimately signed commit the box does not yet trust;
    the refusal tells the two apart — "signed by a key this box did not
    list when it last applied (<principal>)" is not "unsigned" — and
    names the recovery: force `main` back to the commit that adds the
    key, let it apply, then push the rest. `deploy_trust` may change
    freely: the result poll is authorised by a per-push secret, not by
    the trust the push changes (see "Handler contract"), so a push that
    moves the repository or narrows a claim still sees its own outcome,
    and the *next* run is
    the one the new trust judges. This guard checks presence, not
    reachability; a typo in
    a `claim` or a key that does not match any laptop passes it. The
    guarantee behind it is the console ("At 3am").
16. **Change?** The incoming buffer is compared with the installed
    one, whatever the commits say: identical means `no_change` —
    nothing is written to `/etc/hotserve`, nothing reloads — but
    `applied.json` still advances to HEAD (the chain moved; the box
    should know), and that write goes through the same transaction
    record as an install: `txn.json` with `phase: no_change`, then
    `applied.json`, then the result, then the record removed (steps 18
    to 20 without the swap and the reload). Recovery on `phase ==
    no_change` writes `applied.json` (idempotent) and the result
    `no_change`, so the baseline and the result cannot diverge. The
    store rule is therefore: `applied.json` is written only inside a
    transaction whose phase is `applied` or `no_change`. Different with
    HEAD equal to the baseline is the console edit of "At 3am" being
    overwritten by a re-run: the repository is the truth.
17. **`verified`.** hotserve must be active (`systemctl is-active`).
    `activating` is neither: the applier waits for it to settle,
    bounded by `hotserve.service`'s own `TimeoutStartSec` (240 s), and
    judges the state it settles into — Caddy listens before `READY=1`,
    so a push can be admitted while hotserve is still starting, and at
    boot the units are ordered `After=hotserve.service` so that this is
    rare rather than routine. Not running: the push is refused here —
    "nothing applied" — a deliberate departure from `bin/push`, which
    left a validated file for the next start with a person watching;
    here nobody is. Then the result
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
18. **Install.** First the transaction record: `txn.json` — the
    previous file's bytes from step 10 embedded, its digest, the new
    file's digest, `phase: installing` — written to a temporary name,
    renamed to `/var/lib/hotserve-box/txn.json` and `fsync`ed, and only
    once it is durable is the new file written to a temporary name in
    `/etc/hotserve/` (mode 0644, root) and renamed over `Caddyfile`,
    which therefore exists at every instant; then the phase is
    rewritten to `swapped`. Every write in this
    transaction is durable before the next step begins: the temporary
    is `fsync`ed before its rename and the parent directory `fsync`ed
    after it, for the record, the Caddyfile, `applied.json` and the
    result alike — write-plus-rename alone orders nothing across a
    power loss, and step 9's recovery reasons from the order. A
    failure writing either temporary (`ENOSPC`, say) leaves `Caddyfile`
    untouched, removes whatever temporary was made, and ends the push
    as `failed` — "the install failed before the Caddyfile changed;
    nothing changed" — a terminal phase after `verified`, not a
    refusal, in step 20's order: the result first, then the record,
    then the `work/` entry — and when the result cannot be written
    (`ENOSPC` again), the record and the entry are removed all the same,
    as step 20 says, because at a terminal phase the disk is settled
    and only the report is owed. The live file
    never changes without its restore copy already in place. `init`
    runs this same code path (and steps 19 and 20), under the same
    lock.
19. **Reload.** `systemctl reload hotserve` — which runs the unit's
    `ExecReload` (`hotserve reload --config /etc/hotserve/Caddyfile
    --force`) as the hotserve user — with no applier-side timeout
    shorter than systemd's own (`TimeoutStartSec=240s` on
    hotserve.service bounds the reload). Success: the record's phase
    becomes `applied` (rewritten durably) *before* `applied.json`
    records HEAD (temporary name, rename, `fsync`), and the result is
    `applied`. Failure: the phase becomes `rolling_back`, the embedded
    previous bytes are written back over `Caddyfile` (temporary name,
    rename, `fsync`) and the reload is run **again**, so that the file
    on disk is the one running by construction, not by inference from
    an exit status; the result is `rolled_back`, or `unknown` if the
    second reload also fails (the journal says so at warning level; the
    file on disk is the previous one). The record is not removed in
    this step: it outlives both reloads, so a crash anywhere here
    leaves a phase for step 9 to act on.
20. **Result, then the record.** `out/<id>.json` is rewritten
    (temporary name, rename, `fsync`) with the terminal phase; only
    then is `txn.json` removed, and the `work/` entry after it. The
    record is the last thing a transaction lets go of, which is what
    makes "record present" mean exactly "no terminal result yet", and a
    result with no record mean "done". One exception, for a full disk:
    if the result cannot be written, the terminal outcome is logged at
    error level with every field the result would have carried, and
    the record and the entry are removed anyway — both are `unlink`s
    and need no space. The record exists to recover an *unsettled*
    transaction; at a terminal phase the disk is settled and only the
    report is owed, and a record kept for a report that cannot be
    written would re-trigger the path unit (`PathExists=txn.json`) into
    its start-rate limit, so that freeing the disk would not revive the
    channel. The workflow, finding `pending` and then (after the marker
    ages out) `404`, fails red naming the journal, which has the
    outcome. Every
    `work/` entry leaves with its result, so `work/` is empty on every
    exit as `in/` is. Retention has one owner: the applier sweeps
    `out/<id>.json` and `stage/<id>.auth` together, as a pair, by two
    rules at once — a pair older than a day goes, and at most 32 pairs
    are kept, oldest first to go. A count, because an OIDC holder can
    post the already-applied `HEAD` as often as it likes and each fast
    `no_change` is a new pair, and an age alone would let a day of those
    fill the disk or the inode table. One owner, because two sweepers
    in two processes could not keep the pair whole: a marker without
    its result reports `pending` for ever, a result without its marker
    has no poll secret. The handler sweeps nothing in `stage/` but its
    own unfinished `.tar` temporaries; root reaches `stage/` through
    the capabilities it already holds.

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
run as root on pushed input. The walk also refuses the file if an
`import` token stands at directive position anywhere in it ("The
shape"): the committed file is the effective configuration, or it is
not applied.

A literal check is not enough on its own, because Caddy's `Parse`
expands `{$NAME}` and `{$NAME:default}` placeholders on the raw bytes
*before* it tokenizes (`replaceEnvVars`, caddyconfig/caddyfile/
parse.go): `{$UNSET:import}` at directive position is one token to
`Tokenize` and the word `import` to Caddy. So the walk applies two
more rules — with "directive position" defined as Caddy defines it,
the first token on a line (the lexer's new-line flag), at any brace
depth, and with one shape of file refused outright: a site written
without braces (Caddy allows a single brace-less site after the
global options block) would put its directives at depth zero where a
depth walk reads them as addresses, so every site block in the signed
file must be braced, and `init` says so by name. Fixtures pinned
against `caddyfile.Parse` cover both the braced and the refused form.
First, no token at directive position, in a site address,
or anywhere inside the `box` block may contain `{$` — a directive's
*name* is never computed, whatever the environment; placeholders stay
legal in values (`email {$ACME_EMAIL}`, an ACME DNS token), where they
cannot change what the line *is*. Second, the walk runs twice: once on
the raw bytes, and once after applying Caddy's own placeholder
expansion with an **empty** environment — unset names become empty,
defaults take effect — so that anything a default could smuggle in
(a newline and an `import` inside `{$X:…}`) is seen as the tokens it
would become. The expansion is reimplemented in `box` (it is a few
lines) and pinned by a test against `caddyfile.Parse` on fixtures with
the environment cleared. What the service's real environment expands
at reload is root's: it comes from `hotserve.service` and its
drop-ins, which the hotserve uid cannot write. The same walk is what `hotserve box webhook`
prints the address from and what `make check` prints its mapping line
from, so there is one reading of the file, not three.

## Store rules (a trust boundary in both directions)

Written before the code, as liveswap's deploy-record store was.

| Path | Mode | Owner | Rule |
|---|---|---|---|
| `/etc/hotserve/Caddyfile` | 0644 | root:root | Written only by root: `init`, the applier, the console. Read by hotserve (serving), by the handler (signers for the pre-check) and by the applier (the signer list and the rollback copy). The conffile the package ships. |
| `/var/lib/hotserve-box/txn.json` | 0600 | root:hotserve (born in the setgid directory; 0600 keeps it root's alone) | The transaction record, one file written atomically: `{id, commit, path, signer, origin, prev (base64 of the previous Caddyfile), prev_sha256, new_sha256, diff, apps, box_webhook, caddyfile_edited_out_of_band, phase}` — everything a terminal result needs, so recovery reads nothing else. Written before `Caddyfile` is replaced with `phase: installing`, rewritten at each phase change (`swapped`, `applied`, `rolling_back`), removed only after the terminal result is written (step 20), whichever way the transaction ended. Its presence at startup therefore means exactly "a transaction has no terminal result yet", which the applier (step 9) and `init` settle from its `phase` before anything else; there is no partial state, since one rename makes or unmakes it. Deliberately not `/etc/hotserve/Caddyfile.prev`: `bin/push` and its sudoers line write that name, and the path unit must never fire on a legacy push. |
| `/etc/hotserve/age/` | 0700 | root:root | Reserved, empty, created by `init`; the secrets PR puts the box's age key here. |
| `/var/lib/hotserve-box/` | 2750 | root:hotserve | Created by `tmpfiles.d`, not by either process. Setgid, so `applied.json` written here by root is born group `hotserve` without a `chown` (the unit has no `CAP_CHOWN`, and root is not in the group). Not under `/var/lib/hotserve`, which is 0750 hotserve:hotserve. Joins `sandboxHotservePaths` (liveswap/sandbox.go): never a bind source. |
| `…/stage/` | 0700 | hotserve:hotserve | The handler assembles a bundle here. Not watched. |
| `…/stage/lock` | 0600 | hotserve:hotserve | The admission lock (step 2): a non-blocking `flock` held from the pending check through the `.auth` write, so admission is atomic across concurrent requests and across the reload. Distinct from root's `lock`, which serialises applies. |
| `…/stage/<id>.auth` | 0600 | hotserve:hotserve | `{sha256 of the poll secret issued for <id>, posted}`, written only after `in/<id>.tar` is in place (step 7). Two jobs: it is the *pending* marker (step 2) — a push is pending while this exists, is under fifteen minutes old, and no terminal `out/<id>.json` does; a bundle still in `in/` blocks admission on its own, with no age-out, whatever this marker says — and it authorises `GET /?result=<id>` for the bearer of that secret, for fifteen minutes from `posted`, whatever the running `deploy_trust` says ("Handler contract"). Not removed on a read, because a read is not a delivery: the response can fail after the file is gone, and the retry must still be authorised. Swept by root, together with its `out/<id>.json` as one pair (step 20): a pair older than a day, or beyond the 32 newest, goes — one owner, so neither half outlives the other; a `404` therefore means swept, never "not yet", and a holder of a valid token who posts `HEAD` in a loop fills neither directory. A digest, never the token; root reads it only to delete it. |
| `…/in/` | 0770 | root:hotserve | The handler renames a complete bundle in; the applier renames everything out before reading anything. `DirectoryNotEmpty=` watches it, so it must be empty on every applier exit, or the path unit re-triggers until its start-rate limit fails it. |
| `…/in/<id>.tar` | 0644 | hotserve:hotserve | `<id>` matches `^[0-9a-f]{32}$`: 16 hex of the handler's nanosecond clock, then 16 random (step 9); a name that does not match — or an entry that is not a regular file at all — is moved to `work/` and refused like any other, which the unit's `CAP_DAC_OVERRIDE`/`CAP_FOWNER` exist to guarantee. The content is a public commit; 0644. |
| `…/work/` | 0700 | root:root | Bundles land here by rename and are read once. Never a source of truth after that read. |
| `/tmp/box-verify-<id>/` (the unit's `PrivateTmp`) | 0755 / files 0644 | root:root | The payload, the signature and the generated allowed_signers for `ssh-keygen` running as uid 65534, which cannot traverse `work/` (0700) or `/var/lib/hotserve-box` (0750 root:hotserve). The private `/tmp` is gone with the unit. |
| `…/out/` | 2750 | root:hotserve | Results, written to a temporary name and renamed so the handler never reads a partial file; setgid, so a result root creates is group `hotserve` and readable by the handler without a `chown`. Root sweeps by age (a day); hotserve only reads. |
| `…/out/<id>.json` | 0640 | root:hotserve | `{id, phase, commit, signer, path, diff, apps, box_webhook, error, caddyfile_edited_out_of_band}`. The `diff` passed the redactor. |
| `…/applied.json` | 0640 | root:hotserve | `{sha, path, sha256, signer, when}`, written to a temporary name, renamed and `fsync`ed, and only by a holder of root's `lock`: inside a transaction whose record says `applied` (a reload succeeded) or `no_change` (the baseline alone advances), by `init` on a box where hotserve is not running, or by `hotserve box baseline` after it has settled any transaction left on disk — never concurrently with an apply, so a trust reset cannot be overwritten by an apply verified against the old baseline, nor by a recovery of an older transaction: the baseline; the repository path of this box's file, set by `init` and changed only by `init`; the installed file's digest (an out-of-band edit is visible, not refused — root's file is root's); the principal whose key root verified, or the literal `init` when the console wrote the record and verified none; `when`, the time of the write. Nothing the handler reported about the caller is in it; that is the handler's journal line. The box's host is read from the installed file, never recorded. |
| `…/lock` | 0600 | root:root | `flock`; one apply at a time. |

Caps: Caddyfile and blob ≤ 1 MiB; a tree object ≤ 1 MiB; a commit
object ≤ 64 KiB; chain ≤ 500 commits; tree depth ≤ 32; `path` ≤ 4096
bytes of safe components; whole bundle ≤ 16 MiB *decompressed* (the
gzip reader stops there; the per-type caps add up to more, so the
total is the bound that wins) and ≤ 16 MiB as a body; ssh-keygen
output bounded into the error text; every child process has a
deadline and a `WaitDelay`.

Nothing read from `in/`, `work/` or the bundle is trusted for anything
but its bytes, and nothing from a bundle reaches a result's `error`
field or root's journal line except what a refusal must name to be
useful — a `box_webhook` address, the `path`, an `import` target, the
offending placeholder token, a principal: every input-derived value
without exception — each bounded to
300 bytes as liveswap's `boundRefusal` does — Go-quoted to ASCII first
when it is not printable UTF-8, then cut at a rune boundary, in that
order, because a cut-then-quote is not a bound — so a
newline or a control byte in either cannot split or forge a log line.
Everything else a refusal quotes (ids, principals, hosts) the applier
derived itself. The `path` is validated as safe components before it
is used to walk trees.

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

The host says which box; it cannot say which *file*. A repository may
hold two files that both name this host — a staging copy with a
relaxed `deploy_trust`, an old one under `archive/` — and both sit in
the same signed tree, so a compromised Action holding the OIDC token
could choose the one the signer never meant to run. The bundle's
`path` is therefore not a free hint: `init` records `<dir>/Caddyfile`
in `applied.json` from the directory it was given, and the applier
requires the bundle's `path` to equal it (step 11). A mismatch is
refused naming both paths. Changing the recorded path is `init` again,
as a host rename is; `hotserve box baseline` does not touch it.

The directory name is still not an identity. It is a label the
operator picks, `box1` in the template; `make check` and the run print
`box1/Caddyfile → https://deploy.example.com/ (1 app, 1 signer)` so
the mapping is always in view, and the README may suggest naming the
directory after the host but never enforces it. What the record pins
is which path in the repository this box applies, not what the
directory is called. Renaming the deploy host is a console step —
`hotserve init` again from a copied directory — which is honest: the
new host has no certificate until the box serves it, so the rename
could not be a plain push in any case.

## Handler contract

- `POST /` — the bundle. Answers as in step 8. `Content-Type:
  application/gzip` (or `x-gzip`, `octet-stream`); anything else 415.
- `GET /` — `{"commit": "<sha>", "box_webhook": "<host>", "sha256":
  "<hex>"}` from `applied.json` and the installed file. The workflow
  reads `commit` to bundle exactly `git rev-list --first-parent
  <commit>..HEAD`.
- `GET /?result=<id>` — `<id>` must match `^[0-9a-f]{32}$` before it
  names any file (400 otherwise). The result; 202 `{"phase":
  "pending"}` while `stage/<id>.auth` exists and no `out/<id>.json`
  does yet (the bundle is queued or the applier is on it); 404 only
  when neither exists, which means swept — or the one crash window of
  step 7, where the handler died after dropping the bundle and before
  writing `.auth`, and so never answered the POST either; the workflow
  re-runs. The workflow polls until a terminal phase or fifteen
  minutes, then fails red naming `journalctl -u hotserve-box-apply -u
  hotserve-box-apply.path` on the box. Authorised by a *poll secret*,
  checked first: the 202 and 504 answers to the POST carry
  `poll_token`, 32 random bytes, whose `sha256` the handler stored in
  `stage/<id>.auth` with a fifteen-minute life; a `GET /?result=<id>`
  bearing it is accepted for that `<id>` alone, whatever the running
  `deploy_trust` says, and the match costs nothing in the failure
  budget. A bearer that is not the poll secret goes to `deploy_trust`
  as any other request. Why not the OIDC token itself: a push that
  changes `deploy_trust` would lock its own workflow out of the result
  with a 401 the moment the reload succeeded, and the token's own
  lifetime (GitHub's is five minutes) is shorter than a slow apply —
  queue, recovery, two bounded reloads — so a poll late in a clean
  rollback would fall to `deploy_trust` with an expired token, be
  charged, and 429 by the eleventh. The secret is scoped to one
  result, lives fifteen minutes, and is kept rather than removed on a
  read, so a poll whose response was lost can be retried.
- Another method on `/` — 405; another path — not this handler's,
  passed on (see "The shape"). Every answer passes liveswap's response
  filter — the shape and entropy layers — with the protocol's own
  fields on the filter's safe list: `id`, `commit`, `sha256`,
  `poll_token`, `box_webhook` and `phase` are placed by the handler and
  must reach the client intact, and the entropy layer would otherwise
  mask every one of them (a 32-hex id, a 40-hex commit, a 64-hex
  digest and a random secret are exactly what it exists to catch).
  Only `diff` and `error` text, which carry bundle-derived bytes, go
  through every layer. The `diff` in a result was
  redacted by the applier before it was written, with those same two
  layers — the applier has no service environment to prime the
  environment layer with, and says so rather than pretending to.
- Auth precedes everything, including existence: an unauthenticated
  request learns nothing but 401.

## `hotserve init <dir> <sha>` and `hotserve box baseline <sha>`

`init` is the second and last thing root does over SSH. It refuses
unless it is root; reads `<dir>/Caddyfile`; refuses, by name, a file
with no `box` block, no `signer`, no site carrying `box_webhook`, more
than one, a `box_webhook` site whose address is not exactly one bare
hostname (a scheme, a port, a path, a wildcard, a placeholder, a
second name), or any `import` directive;
requires a 40-hex `<sha>` — the commit the operator is about to push,
so no null baseline ever exists; runs `hotserve validate --adapter
caddyfile` as the hotserve user, never as root (the same reasoning as
step 6), with the
environment `hotserve.service` gives the service and nothing of
root's shell — dropping the uid does not drop the environment, and
the adapter would expand `{$VAR}` from whatever it inherited; then
takes the applier's lock — the same blocking `flock`, so an applier
run the record itself triggers (the path unit watches `txn.json`)
waits for `init` to finish and then finds nothing to do — and runs the
applier's install transaction, the same code, not a description of
it: step 9's recovery first if a record is lying there, then step 18
(the existing file's bytes embedded in the record, `origin: init`, the
new file landing 0644 root by temporary name and rename, so
`/etc/hotserve/Caddyfile` exists at every instant) and step 19 (reload
if hotserve is active; on failure the previous file written back,
reloaded again, `applied.json` left as it was, and `init` exits
non-zero saying so — a re-run of `init` as the break-glass must not
leave a box that cannot restart). `/etc/hotserve/age/` is created.
`applied.json {sha, path, sha256, signer: "init", when}` is written
inside the transaction as step 19 says — `signer` is the literal
`init`, because the console verified no signature and the field says
so rather than lying or being absent, and every reader accepts that
value — with `path` from `--path <repo-relative path>` when
given and `<dir>/Caddyfile` from the directory's base name otherwise —
a nested `prod/box1/Caddyfile` needs the flag, and the path-mismatch
refusal names it. When hotserve is not running, `init` treats the swap
as applied: the record goes `swapped` → `applied` with no reload, and
`init` says the file loads at the next start, since a person is at the
console for `init` in a way nobody is for the applier; recovery honours
this through `origin` — a `swapped` record with `origin: init` on an
inactive box is finished as `applied`, not undone, where the applier's
own would be restored. Then it prints what it found and did:

```
init: box1/Caddyfile validates; 1 app (example); box_webhook on deploy.example.com; 1 signer (alice@example.com)
init: installed /etc/hotserve/Caddyfile; this box applies commits descending from 4f1c2a9
init: reloaded. From here, config changes are pushes to your-org/boxes.
```

`hotserve box baseline <sha>` (root) rewrites the baseline only — under
root's blocking `lock`, after running step 9's recovery so that any
transaction left on disk is settled first, and durably (temporary name,
rename, `fsync`), so that no apply verified against the old baseline
and no recovery of an older transaction can overwrite the reset — and
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
`DirectoryNotEmpty=/var/lib/hotserve-box/work`,
`PathExists=/var/lib/hotserve-box/txn.json` — the second and third are
what make step 9's recovery run after a crash, at boot included, since
a bundle already moved out of `in/` would otherwise wait forever; all
three paths are written by the applier, by `init` (which holds the
same lock, so the run its record triggers simply waits and then finds
the work done) and by the handler, never by a sudoers line or a laptop
script, so nothing else can fire it; the lock is a *blocking* `flock`
— a non-blocking one that exited when busy would leave `txn.json` in
place and re-trigger the unit into its start-rate limit;
`TriggerLimitIntervalSec=10s`, `TriggerLimitBurst=20`, enabled,
`WantedBy=multi-user.target`, `After=hotserve.service`) starts
`hotserve-box-apply.service` (`Type=oneshot`, `ExecStart=/usr/bin/
hotserve box apply`, root, not enabled on its own, also
`After=hotserve.service` — so that boot recovery of a `swapped` record
does not run while hotserve is still `activating` and read the wrong
answer from `is-active`; step 17 handles the state when it is met all
the same). The service carries the backups units' house
style: `ProtectSystem=strict` with `ReadWritePaths=/etc/hotserve
/var/lib/hotserve-box`, `PrivateTmp`, `NoNewPrivileges`,
`RestrictAddressFamilies=AF_UNIX` (`systemctl` over D-Bus; no network),
`SystemCallFilter=@system-service`, `CapabilityBoundingSet=CAP_SETUID
CAP_SETGID CAP_KILL CAP_DAC_OVERRIDE CAP_FOWNER` (the first two to run
`ssh-keygen` as 65534; `CAP_KILL` to be able to stop it — a uid-0
process without it cannot signal a child of another uid, so a deadline
on the verifier would be a deadline on nothing; the last two so that
the never-loop invariant below can hold against the hotserve uid: a
`mkdir in/x && chmod 000 in/x` by a supervisor RCE is an entry root
without them can neither rename, empty nor `chmod`, which would wedge
the path unit at its start-rate limit and deny the channel until a
console `rm` — a persistence T5 is not supposed to have. These are
root's ordinary powers over files, confined by `ProtectSystem=strict`
to the two writable paths; nothing else — root's uid is what writes
`/etc/hotserve`.
`init` is not this unit: it runs as root over SSH and drops to the
hotserve user for `validate` by itself), `TimeoutStartSec=infinity` as
the backups units have it — a
run is bounded from inside, not by systemd killing it mid-transaction:
each reload by hotserve.service's own 240 s, each child by its
deadline and `WaitDelay`, and the drain loop processes one bundle at a
time, so a run of recovery plus several bundles is long but never
interrupted between a swap and its reload — and a reason above every
line. `box/units_test.go` parses both shipped
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
there. The same invariant covers the two recovery triggers: every
`work/` entry leaves with a terminal result, and every recovery ends
by writing a result and removing the record whatever the reload did
(an `unknown` is a terminal result too), so a crash inside recovery
re-triggers the unit once more and that run reaches the same end;
neither trigger can loop. Twenty
re-triggers in ten seconds would fail the path unit until `systemctl
reset-failed`; the invariants are what keep the count at one.

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
- `a push is being admitted; retry in a moment` / `a push is pending:
  <id>, <age> old` — the two 409s of step 2, and no third.
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
- `<sha2>, between the commit this box runs and <sha>, is not signed;
  every commit on main must be — rebase it out of the history and
  force-push; the box still runs <baseline>, so no baseline change is
  needed`.
- `<sha2>, between the commit this box runs and <sha>, is signed by a
  key this box did not list when it last applied (<principal>); the
  commit that adds the key must apply first — force main back to it,
  let the box apply it, then push the rest`.
- `the file sent is not <path> in <sha>` / `<path> in <sha> is not a
  regular file` / `<sha> is in a SHA-256 repository, which the box
  does not read` / `bundle: path is not a relative path of safe
  components`.
- `<sha> does not descend from the commit this box runs (<baseline>)
  along main's first-parent line. If the box already runs a later
  commit than this run's, nothing is wrong: a newer push applied first.
  If a branch merged main into itself before it was fast-forwarded,
  <baseline> is on the merge's other side — rebase the branch onto main
  instead and force-push; no box step. A rewind or a replay is refused
  on purpose. Only if main was rewritten below <baseline> does hotserve
  box baseline <sha>, as root on the box, reset trust to <sha> — whose
  ancestors the box will then never examine`. The workflow does not
  normally reach this: before posting, it checks `git merge-base
  --is-ancestor HEAD <baseline>` and, when the box's commit already
  descends from this run's HEAD, exits green with "superseded by a
  later push"; and its `concurrency` group keeps runs in order. The
  message is for the box, which cannot tell the two apart from the
  bundle alone.
- `the chain from <baseline> to <sha> is longer than 500 commits; run
  hotserve box baseline <sha> as root on the box`.
- `this file is for <host2>; this box is <host1>`.
- `this box's file is <recorded path>; the bundle is <path> — the same
  host appears in more than one file of the repository, or the
  directory was renamed; hotserve init again as root records a new
  path`.
- `the new Caddyfile has no box block` / `… no signer` / `… no site
  with box_webhook` / `… more than one site with box_webhook` / `… the
  box_webhook site's address is not one bare hostname (<address>); no
  scheme, port, path, wildcard, placeholder or second name` / `… imports
  <path>; the box applies only a self-contained Caddyfile — inline the
  snippet` / `… has a placeholder where a directive name, a site
  address or a box line goes (<token>); placeholders are for values`
  / `… drops the key that signed this commit (<principal>); add the new
  key in one push, let the box apply it, then remove the old one in a
  second push` / `… box block has no deploy_trust`.
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
commit whose diff the signer read. Two more README rules fall out of
the first-parent walk and the chain rule: a branch is rebased onto
`main`, never has `main` merged into it (the merge would put the
baseline on the second parent and the box would refuse the
fast-forward, naming the case); and configuration a signer does not
yet mean to run is committed unsigned (`make wip`) or kept in a fork,
because a push credential that cannot sign can still fast-forward a
fully signed branch onto `main`, and the box applies what `main`
says — for as long as the box still runs the commit that branch forked
from, which is why the README also recommends a ruleset that blocks
force-pushes (the only route to put a stale commit back under a `main`
that has moved) and why one applied push after a credential leak
retires every stale signed object at once. Pressing the button anyway does
more harm than a red run: the commit it lands sits on `main` and
refuses every push after it until it is rebased away and
force-pushed (the box's baseline is below it and stays valid; no
console step). The box repository's README states this;
nothing on GitHub enforces it. A `required_signatures` ruleset on
`main` stops the *rebase* button and any unsigned push at the push,
and the README recommends it for that; it does not stop the *squash*
button, whose web-flow GPG signature satisfies GitHub and not the box.
The box depends on neither.

## Threat-model deltas

What each actor can and cannot do once `box` is the only writer. The
full placement is DESIGN-threat-model.md, "Config webhook" and T6.

| Actor | Can | Cannot |
|---|---|---|
| Holder of a box-repo OIDC token (the repo's own CI, a compromised action) | Reach the handler; make it validate a Caddyfile a *signer* committed (pre-check first); replay HEAD — a `no_change` normally, but after a console edit it reinstalls HEAD's file and so **reverts the operator's 3am fix** (the repository is the truth; "At 3am" says so, and a fix that must outlive a replay is a commit); hold one bundle in `in/` at a time (409 while it is there, with no age-out) and one in root's hands | Install anything not signed by a listed key; replay an older signed commit (descent); land another box's file, or another file for this host elsewhere in the tree (identity: host and recorded path) |
| Leaked PAT, stolen session, OAuth/GitHub App with `contents:write` | Push to `main`; make the run red; stop every later push until its commit is removed from the first-parent history (loud, recoverable; a `required_signatures` ruleset stops an unsigned push, not a squash); **fast-forward onto `main` a branch every commit of which a signer already signed** — a signer's own unmerged, signed config — which the box then applies, since it is exactly what the rule asks for (the README's answer: config not yet meant to run is committed unsigned, `make wip`, or lives in a fork) | Produce an SSH signature by a listed key; ride an unsigned or self-authored change into the box under a later signed commit (every commit on the chain is checked); change the box with anything a signer did not sign |
| Compromised laptop holding a software signing key | Everything the operator can: sign and push any config | Nothing the operator cannot; this is the operator |
| Compromised laptop, hardware-held key (`sk-` types) | Push unsigned or GPG-signed commits (refused) | Sign without a touch; so cannot change the box unattended |
| Supervisor RCE (T5, the hotserve uid) | Drop any bundle into `in/` without the handler, and so **make root install any configuration a signer signed that descends from the baseline and whose objects it holds**: `HEAD` again (undoing a console edit), or a signer's signed unmerged branch — the token-to-commit binding is the handler's, and root cannot repeat it (step 4); skip validation (the reload fails and rolls back); report "loaded" for a reload that did nothing (the exit status is the hotserve uid's word); read `out/` and `applied.json`; answer the workflow anything it likes — the POST response and the result poll are served by the hotserve process, so what CI *sees* is the serving process's word | Write `/etc/hotserve/Caddyfile`, `applied.json` or a result file itself (root's record of what happened is root's, whatever CI was told); make root install anything a signer did not sign — an unsigned, non-descendant, other-box or placeholder-smuggled file; persist its *own* configuration across a restart (root's file is the authority at the next start, `systemctl restart` the operator's remedy) |
| Root on the box | Everything, by definition | — |

Residuals accepted here and listed in the threat model: `systemctl
reload`'s exit status is trusted for the `applied` phase (the file on
disk is root's regardless); plain `crypto/sha1`; a passing guard that
is not reachability; validate as a courtesy only; the principal
charset rule; GitHub enforces nothing.

## Non-goals

- git on the box (no clone, no deploy key, no egress to github.com):
  the bundle carries the objects.
- `import` in the box's Caddyfile. The signed file is the whole
  configuration; a snippet is inlined.
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
  directory name a label, and (after a self-review found that an OIDC
  holder could pick any file in the signed tree naming that host) the
  file's path is recorded by `init` and must match; validation runs in the handler, never in
  root; `verified` before reload and a 202 + poll, because a request
  held across `systemctl reload` deadlocks on `Shutdown`; the exchange
  tree at `/var/lib/hotserve-box`; one bundle file per request read
  once into memory; `in/` empty on every applier exit. Review of the
  draft added: every first-parent commit above the baseline must be
  signed (an unsigned commit would otherwise ride in under the next
  signed one), the `path` travels in the bundle, ssh-keygen's files
  live in the unit's private `/tmp`, the is-active check precedes
  `verified`.
