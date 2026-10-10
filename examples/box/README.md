# A box repo for hotserve

The server's config, kept in git: one `Caddyfile` per box, reviewed
like code, signed by the people who may change it, and applied to the
box by this repo's workflow. The box checks every change itself before
it applies it. Keep this repo **private** and separate from your apps'
repos.

It is a separate repo because the Caddyfile is where the box's policy
lives: which repo may deploy each app, each Deno app's permission
flags, and who may change the file itself. An app repo can be
compromised through a dependency or a leaked token; it should not also
be able to grant itself `--allow-net`. So an app that needs a new
permission asks for it here, and the change is a diff someone reads
and signs.

| File | What it is |
|---|---|
| `deploy.example.com/Caddyfile` | One box's whole config; becomes `/etc/hotserve/Caddyfile` on the box whose deploy host names the directory |
| `.github/workflows/apply.yml` | `make check` on every push and pull request; on `main`, sends the commit to each box (`bin/apply`) |
| `Makefile` | `make signer`, `make check`, `make wip` |
| `bin/` | What the Makefile and the workflow run |
| `sudoers` | Creating and editing the apps' env files on the box, and nothing else |

## How a change reaches the box

A push to `main` runs the workflow. It checks every box's file (the
same check runs on pull requests), and posts each box its file from
that commit, with the commit and the
commits before it back to the one the box runs. The box applies the
file only if:

- the workflow's token says it ran for this repo's `main` (the box
  block's `deploy_trust`);
- a key the box's own Caddyfile lists signed that commit and every
  commit between it and the one the box runs (`signer` lines);
- the file is the one in that commit, for this box's host and path;
- hotserve validates it.

Then the box swaps the file in and reloads. A reload that fails puts
the previous file back. The run prints what happened:

```
apply: deploy.example.com/Caddyfile → https://deploy.example.com/: the box runs 4f1c2a9; sending c6d0e81 (1 commit), push 018f3a…
apply: deploy.example.com at c6d0e81, signed by alice@example.com; verified; installing
--- installed
+++ pushed
…
apply: deploy.example.com at c6d0e81: loaded; running apps are not restarted; apps example
```

No SSH key and no secret lives in CI: the workflow's OIDC token says
which repo and branch, and the commit's signature says which person.

## Set it up

The box comes from
[Your first deploy](https://github.com/smallhoursorg/hotserve/blob/main/docs/first-deploy.md):
installed, serving an app from a Caddyfile written by hand, still
administered as root. This repo takes over from there.

1. Make a private repo from this template ("Use this template" on
   [smallhoursorg/hotserve-example-box](https://github.com/smallhoursorg/hotserve-example-box)),
   and clone it.
2. Add your SSH key as the first signer:

   ```sh
   make signer        # ~/.ssh/id_ed25519.pub; or KEY=path/to/key.pub
   ```

   It adds a `signer` line under `# signers` in every box's Caddyfile,
   and makes this checkout sign every commit with that key (its own git
   config: `gpg.format ssh`, `user.signingkey`, `commit.gpgsign`). No
   key yet: `ssh-keygen -t ed25519`, or `-t ed25519-sk` for one held by
   a hardware key, which signs only with a touch.
3. Make the Caddyfile your box's. Start from the one the box runs, so
   nothing edited there is lost:

   ```sh
   scp root@box.example.com:/etc/hotserve/Caddyfile deploy.example.com/Caddyfile.live
   ```

   Then bring over its sites and apps into `deploy.example.com/Caddyfile`,
   keeping the `box` block and the deploy host's `box_webhook`, and
   delete `Caddyfile.live`. In the `box` block, `claim repository` names
   this repo. Replace `example.com`, `deploy.example.com` and
   `your-org/example` with yours — `your-org` twice: the
   `artifact_allowlist` entry pins the organization alone, and the app's
   `deploy_trust` its repo. The `app example` block is hotserve's
   examples/node; a Deno app's lines are in its examples/deno.
4. Name the directory for the box's deploy host, the site that carries
   `box_webhook`:

   ```sh
   git mv deploy.example.com deploy.yourdomain.com
   ```

   The box records `<host>/Caddyfile` as its file, and `make check`
   refuses any other layout.
5. Set `HOTSERVE_VERSION` in `.github/workflows/apply.yml` to the
   release the box runs: the tag without its `v`. `dpkg -s hotserve`
   shows it, except that dpkg writes a prerelease as `0.3.0~rc1` where
   the tag is `0.3.0-rc1`.
6. Commit. It is signed, and it is the commit the box starts from:

   ```sh
   git add -A && git commit -m "deploy.yourdomain.com: the box's config"
   ```

7. Tell the box which repo it belongs to. Copy the box's directory to
   it and run `hotserve init` there, as root, with this commit:

   ```sh
   scp -r deploy.yourdomain.com root@box.example.com:
   ssh root@box.example.com "cd deploy.yourdomain.com && hotserve init $(git rev-parse HEAD)"
   ```

   It installs the file, reloads hotserve, and records the commit; the
   box then applies only commits that descend from it. It refuses when
   hotserve is not running, and says how to start it.
8. Push:

   ```sh
   git push origin main
   ```

   The workflow's first run answers that the box already runs this
   Caddyfile. From here on, config changes are pushes to `main`.

The secrets of the apps are still a file on the box each, edited over
SSH by an administrator — see [Secrets](#secrets). Closing root login
and the rest of the hardening is on
[After the first deploy](https://github.com/smallhoursorg/hotserve/blob/main/docs/after-first-deploy.md).

## Change the config

Edit the Caddyfile, commit (signed), and push to `main`. A pull
request gets `make check` too. Land it on a signer's laptop, never with
GitHub's merge button (see [The rules](#the-rules)):

```sh
git switch main && git pull --ff-only
git merge --ff-only the-branch     # a branch of signed commits, rebased onto main
git push origin main
```

After the first push, with nothing deployed yet, a correct box looks
like this:

- `https://example.com` answers 503 with the sentence from the
  Caddyfile's `handle_errors` block — once the certificate is issued,
  which takes a few seconds after the reload with 80 and 443 open.
- `https://deploy.example.com/example` answers 401 with
  `{"error":"invalid or missing deploy token …"}`, in a browser or
  with `curl -i`. That is the webhook working: a deploy carries a
  token, and nothing without one gets further than this.

A reload never restarts a running app. A changed `app` block applies
at the app's next launch: its next deploy, or a relaunch after a crash
or a reboot.

**Widening an app's permissions:** push the new flag *before* the app
deploys code that needs it; the running version does not mind an extra
permission, and the new one will not start without it. Narrow after
the code that needed it is gone. A rollback runs the older code with
the flags pushed today.

**Undoing a change:** `git revert` it and push. The box applies the
revert like any commit.

## The rules

Each follows from what the box checks: every commit between the one it
runs and the one it is sent must be signed by a key it lists, and must
sit on `main`'s first-parent line.

| Rule | Because |
|---|---|
| Every commit on `main` is signed by a listed key. A branch from someone who is not a signer is landed by a signer: `git merge --squash` and one signed commit. | The box checks every commit it has not run. |
| Never press GitHub's merge button. | A squash is signed by GitHub's key, a rebase lands unsigned, a merge commit is GitHub's: the box refuses the push, and every push after it, until the commit is rebased out of `main` and force-pushed. |
| Rebase a branch onto `main`; never merge `main` into a branch. | A merge puts the commit the box runs off the first-parent line, and the box refuses the fast-forward, naming the case. |
| Config not yet meant to run is committed unsigned (`make wip`) or kept in a fork. | A push credential cannot sign, but it can fast-forward a branch of signed commits onto `main`, and the box applies what `main` says. A branch with an unsigned commit in it is refused whoever lands it. |
| Every box in this repo lists the same signers (`make signer` adds to all; `make check` holds it). | Every commit is on every box's history, so a key one box lacks stops that box's pushes. |
| Change a signer in two pushes: add the new key, let it apply, then remove the old one. | The box refuses a file that drops the key that signed it. |
| Recommended, in the repo's rulesets: require signed commits, and block force pushes to `main`. | The first stops the rebase button and unsigned pushes (not the squash button: GitHub's signature satisfies GitHub, not the box); it needs each signer's key added to their GitHub account as a *signing* key, or GitHub refuses their pushes. The second keeps a stale signed commit from being put back under a `main` that has moved — and so blocks the force-push that undoes a bad commit (below): lift it for that push. The box depends on neither; they shorten red runs. |
| After a credential leak: rotate it, then one applied push to `main`. | A signed commit someone fetched lands only while the box still runs the commit it forked from; one applied push puts it below. |

## Adding a signer

A box checks a commit against the signers it *runs*, so the commit that
adds Bob is signed by someone already listed:

1. Bob, in his clone, on a branch: `make signer` (it stages the line
   it adds), then `make wip` — that commit, unsigned — and pushes the
   branch.
2. Alice, a signer: `git merge --squash bob-signer && git commit` —
   one commit, signed by her — and pushes `main`.
3. Once it applies, Bob's own signed commits go through.

Removing someone is a commit that deletes their line, signed by
someone else, since a box refuses a file that drops the key that
signed it.

## When a push is refused

The run prints the box's reason; it changed nothing. The common ones:

- **A commit is not signed**, or is signed by GitHub (the merge
  button): rebase it out of `main` — re-sign it, or squash it into a
  signed one — and force-push (lifting the ruleset that blocks force
  pushes, if you set one). Nothing on the box.
- **Signed by a key the box did not list:** the commit that adds the
  key has to apply first. Put `main` back to that commit, let it apply,
  then push the rest.
- **Does not descend from the commit the box runs:** a branch that
  merged `main` into itself — rebase it onto `main` and force-push. If
  `main` itself was rewritten below that commit, `hotserve box baseline
  <sha>`, as root on the box, resets trust to `<sha>` (the box will
  never examine its ancestors).
- **Longer than 500 commits** since the one the box runs: `hotserve box
  baseline <sha>` as root on the box.
- **A run says "superseded"**: a later run already brought the box
  past this commit. Nothing is wrong.

## At 3am

If a change broke the deploy host itself, the next push cannot reach
the box. The way back is the provider's console as root: edit
`/etc/hotserve/Caddyfile` and `systemctl reload hotserve`, or copy the
box's directory over and `hotserve init` again from it. The next
signed push overwrites a hand edit — the run warns that it did — which
is the point: the repo is the truth and the edit is a bridge back to
it, so a fix that must outlive the night is a commit.

## More than one box

One directory per box, each named for its deploy host. Every push goes
to every box: each one's history must keep up, even when its own file
did not change, and then the box answers that it already runs that
Caddyfile. Each box is set up with `hotserve init` from its own
directory.

## Secrets

They never go in this repo. Put each app's in a file on the box that
only root and the `hotserve` user can read, and name it with
`env_file` in the app's block. The `sudoers` file in this directory
lets an administrator who is not root create the file and edit it;
both are its exact lines, so nothing about the mode, the owner or the
directory is theirs to choose. As root on the box, once (from the
laptop: `scp sudoers root@box.example.com:/etc/sudoers.d/hotserve-admin`):

```sh
chmod 0440 /etc/sudoers.d/hotserve-admin && visudo -c
adduser alice
install -d -m 0700 -o alice -g alice /home/alice/.ssh && cp ~/.ssh/authorized_keys /home/alice/.ssh/
groupadd hotserve-admin && usermod -aG adm,hotserve-admin alice
```

A box set up from an earlier version of this template has `bin/push`'s
lines in that file too, which let an administrator write the Caddyfile
around the signed pushes: copy this one over it, as above.

Then, as alice:

```sh
sudo install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/example.env   # once: empty, root:hotserve
sudoedit /etc/hotserve/example.env      # DATABASE_URL=postgres://…
```

Whoever can get a signed commit onto `main` can read these files
anyway: one site in the Caddyfile serves any file the `hotserve` user
can read, env files included. A signer's key is the Caddyfile, and the
Caddyfile is every app's secrets.

The `adm` group reads the whole system journal — `journalctl -u
hotserve`, an app's lines, `journalctl -u hotserve-box-apply` — not
only hotserve's. An env file is an app's environment, which for most
runtimes is a way to run code as that app (`NODE_OPTIONS`, say), so
give this to the people who may change what the apps run.

hotserve reads the file each time the app launches, so a missing or
unreadable one fails that launch, and a changed one applies at the
next. A Deno app also needs `--allow-env=` to name each variable it
reads.
