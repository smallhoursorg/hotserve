# Working on this app

It is deployed by [hotserve](https://github.com/smallhoursorg/hotserve),
which runs it under rules that do not show up when you run it locally.

- **Listen on the unix socket in `$SOCKET`**, via
  `Deno.serve({ path })`. On the box there is no `PORT`, and nothing
  may listen on TCP. (Locally, with no `$SOCKET`, it serves
  127.0.0.1:8000.)
- **`GET /health` answers 2xx once the app can serve.** A deploy only
  goes live after it has, continuously, for the soak period.
- **Handle SIGTERM**: finish in-flight requests, then exit. It is how
  the old version is stopped after a deploy.
- **Persistent data goes in `$DATA_DIR`** (the app's `shared/` dir on
  the box). The release directory is replaced on every deploy.
- **Migrations go in `migrate.ts`.** It runs before the new version
  starts; a non-zero exit cancels the deploy and the old version keeps
  serving. It must be safe to run again on a database it has already
  migrated.
- **Nothing is fetched at runtime.** `deno task bundle` ships the
  module cache in the tarball and the app runs with `--cached-only`:
  every import must resolve at build time.
- **Only its own dirs exist in production.** The app's sandbox holds
  its release dir, `$DATA_DIR`, a private `/tmp` and the OS runtime
  under `/usr`. `/opt`, `/srv`, `/home` and host unix sockets (a local
  Postgres's, say) are absent, not merely unreadable.
- **Permissions are granted on the box, not in this repo.** The app
  runs with the Deno flags in the box's Caddyfile; `hotserve.caddy`
  here is a copy that documents what the code needs. If a change needs
  a new env var, file path or network host, update `hotserve.caddy`
  in the same change and say in its description that the box needs
  the same flag *before* it is deployed. Never use `-A`,
  `--allow-all`, `--allow-run` or `--allow-ffi`.

Check a change with `deno task dev`, then `deno task bundle`.
