# liveswap

**Your reverse proxy is your deploy pipeline.** liveswap turns a
single Caddy server into a zero-downtime deploy orchestrator for the
apps it fronts — Node.js, Go, anything that listens on a port. CI
builds a tarball, POSTs a webhook with its URL, and Caddy does the
rest: download, migrate, start the new version on a fresh localhost
port, health-gate it, atomically cut traffic over, gracefully stop the
old one. No Kubernetes, no Nomad, no SSH keys in CI, no extra daemons.
One binary. Part of [hotserve](https://github.com/smallhoursorg/hotserve), from [smallhours](https://github.com/smallhoursorg).

```
git push → CI builds app.tar.gz → uploads it → curl webhook → Caddy hot-swaps it
```

## How a deploy works

```
POST /blog {url, version}
  │ downloading   stream the artifact (size-capped, https, secret-gated)
  │ extracting    hardened tar extraction into releases/<version>/
  │ preparing     pre_start command (migrations) — non-zero exit aborts
  │ starting      spawn the app on a fresh 127.0.0.1 port, PORT injected
  │ soaking       GET /health until continuously healthy for `soak`
  │ promoting     atomic cutover — new requests hit the new version
  │ draining      wait `drain` for in-flight requests on the old one
  │ stopping_old  SIGTERM the old process group, SIGKILL after `grace`
  └ 200 OK        (any failure before "promoting" → old version never
                   stopped serving, webhook returns 5xx, CI goes red)
```

The cutover is an atomic pointer swap inside a `reverse_proxy` dynamic
upstream source — no config reload, no socket juggling, and every
reverse_proxy feature (WebSockets, HTTP/2, streaming, retries) keeps
working. Rollback is one curl: re-POST the previous version.

If you know Nomad, the concept map is:

| Nomad | liveswap |
|---|---|
| `canary = 1` + `auto_promote` | start new → health gate → atomic cutover |
| `min_healthy_time` | `soak` |
| `auto_revert` | failures never promote; old version keeps serving |
| `artifact` stanza + deploy webhook | the webhook payload's `url` |
| `nomadService` template re-render + SIGUSR1 | `dynamic liveswap <app>` |

## Install

Build Caddy with the module (nothing else to install on the server):

```sh
xcaddy build --with github.com/smallhoursorg/hotserve/liveswap
```

Or in a Dockerfile:

```dockerfile
FROM caddy:2.11.4-builder AS builder
RUN xcaddy build v2.11.4 --with github.com/smallhoursorg/hotserve/liveswap

FROM caddy:2.11.4
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```

## Caddyfile

```caddyfile
{
	liveswap {
		# root /var/lib/liveswap                # where releases/state live (default)
		webhook_secret {env.LIVESWAP_SECRET}    # default secret for all apps
		artifact_allowlist github.com/your-org/ # required: where artifacts may come from
		# allow_insecure_http                  # permit http:// artifact URLs (off by default)

		app blog {                             # a Node.js app
			command node server.js             # runs with CWD = the release dir
			pre_start node migrate.js          # optional; non-zero exit aborts the deploy
			env_file /etc/liveswap/blog.env     # optional KEY=VALUE file
			env NODE_ENV production            # inline env, repeatable
			# webhook_secret {env.BLOG_SECRET} # per-app override
			# Everything below is a default, shown for reference:
			# health_path       /health        # GET must return 2xx ("off" = liveness only)
			# health_interval   5s
			# health_timeout    2s
			# soak              15s             # continuous health required before cutover
			# deadline          5m              # abort if not healthy in time
			# drain             5s              # pause between cutover and SIGTERM
			# grace             10s             # SIGTERM → grace → SIGKILL
			# keep              5               # release dirs retained on disk
			# max_artifact_size 100MB
		}

		app api {                              # a Go app
			command ./server --config config.yaml
			env DATABASE_URL sqlite:{shared_dir}/api.db
		}
	}
}

blog.example.com {
	reverse_proxy {
		dynamic liveswap blog
	}
}

api.example.com {
	reverse_proxy {
		dynamic liveswap api
	}
}

deploy.example.com {
	liveswap_webhook
}
```

Apps must listen on `127.0.0.1` at the injected `PORT`. Their
environment is, lowest precedence first: an allowlisted slice of
Caddy's environment (`PATH`, `HOME`, `LANG`, `TZ`, `LC_*` — nothing
else, so supervisor credentials like `LIVESWAP_SECRET` never reach
apps) → `env_file` → inline `env` → injected `PORT` and
`HOST=127.0.0.1`. Anything more an app needs must be passed
explicitly via `env` or `env_file`.

### Placeholders

`command`, `pre_start` and `env` values may use deploy-time
placeholders: `{version}`, `{port}`, `{release_dir}`, `{shared_dir}`.
`{shared_dir}` (`<root>/<app>/shared/`) survives deploys — put SQLite
files and uploads there. Standard Caddy `{env.*}` placeholders are
resolved at config load.

### Options

| Option | Default | Meaning |
|---|---|---|
| `root` | `/var/lib/liveswap` | Releases, shared data, state per app |
| `webhook_secret` | — (required) | Shared secret; global default or per app |
| `allow_insecure_http` | off | Permit plain-http artifact URLs |
| `artifact_allowlist` | — (required) | Where artifacts may be fetched from. Entries are a host (`artifacts.corp`) or a host + path prefix (`github.com/your-org/`). An entry admits only the scheme's default port unless it declares one (`minio.corp:9000`, or `127.0.0.1:*` for dev servers on random ports) — the port picks which service on the host answers, so it belongs to the operator, not the payload. Pin the path on multi-tenant hosts — a bare `github.com` admits anyone's artifacts. Query strings are refused unless the entry declares the parameter names it vouches for: `gitlab.com/api/v4/projects/42/?job` allows `?job=build`, `bucket.s3.corp/releases/?X-Amz-*` allows the presigned-URL family (a trailing `*` declares a prefix; values are never re-encoded — signed queries pass through byte-identical — but every query byte must be a legal RFC 3986 query character, so percent-encode anything exotic). Closed by default because on some servers a query *name* can override path routing entirely (WordPress-style `?p=2`), which would defeat the path pin. A refused URL fails the deploy with a 422 naming the offending parameter. First hop only, by design (GitHub asset URLs redirect to S3); every hop must still be https unless `allow_insecure_http`. Apps may override |
| `command` | — (required) | argv to start the app, CWD = release dir |
| `pre_start` | — | Run-to-completion hook; failure aborts deploy |
| `env`, `env_file` | — | Extra environment |
| `health_path` | `/health` | 2xx = healthy; `off` = process-liveness only |
| `health_interval` / `health_timeout` | `5s` / `2s` | Probe cadence |
| `soak` | `15s` | Continuous health required before cutover |
| `deadline` | `5m` | Bound on pre_start and the health gate |
| `drain` | `5s` | In-flight grace after cutover, before SIGTERM |
| `grace` | `10s` | SIGTERM → SIGKILL window |
| `keep` | `5` | Release dirs retained (GC after success) |
| `max_artifact_size` | `100MB` | Download cap; decompressed cap is 10× |

## Webhook API

`POST https://deploy.example.com/<app>`, authenticated with either
header:

- `Authorization: Bearer <secret>` — **recommended**: Caddy access
  logs redact the `Authorization` header automatically.
- `X-Liveswap-Secret: <secret>` — same effect, but if you enable
  access logging on the webhook site, this custom header is logged in
  plaintext (see [Secrets and logs](#secrets-and-logs)).

JSON body:

```json
{
  "url": "https://github.com/you/blog/releases/download/v1.4.2/blog.tar.gz",
  "version": "v1.4.2",
  "auth_header": "Bearer <token-for-private-assets>"
}
```

`auth_header` is optional and is sent verbatim as `Authorization` on
the artifact download (dropped automatically on cross-host redirects,
so GitHub's S3 redirect works). The response is synchronous:

| Code | Meaning |
|---|---|
| 200 | Deployed; body is the app's status JSON |
| 401 | Bad or missing secret |
| 404 | Unknown app |
| 409 | A deploy is already running for this app (retry) |
| 422 | Bad payload (missing url, invalid version, version already running) |
| 5xx | Deploy failed — **the old version is still serving**; body says why |

Because the response is synchronous through the whole pipeline, the
POST's wall time includes the health soak, the `drain` pause and the
old version's graceful stop — with defaults, a healthy deploy answers
in roughly soak + drain (~20s). Budget your CI step timeout for
`deadline` plus drain and grace, and expect a concurrent deploy to
409 until the first one finishes.

`GET /<app>` (same secret header) returns status: phase, current
version, port, pid, last deploy result.

The tarball's contents must sit at the archive root (`tar -czf
app.tar.gz -C dist .`), with versions matching `[A-Za-z0-9._-]{1,64}`.

## Secrets and logs

What liveswap does for you:

- Deploy logs record the artifact **host only**; download errors go
  through a redactor that drops credentials and query strings (where
  presigned-URL and token secrets live).
- The packaged systemd unit deliberately does **not** use `--environ`
  (unlike Caddy's dist unit), so `LIVESWAP_SECRET` and any ACME DNS
  tokens never land in the journal — journals get pasted into bug
  reports. The package smoke test asserts this.
- Config keeps `{env.LIVESWAP_SECRET}` as a placeholder, so config
  dumps and the admin API never contain the value.

What's yours to handle:

- If you enable **access logging** on the webhook site, use
  `Authorization: Bearer` (redacted by Caddy automatically). If you
  must use `X-Liveswap-Secret` with access logs, filter it:

  ```caddyfile
  deploy.example.com {
  	log {
  		format filter {
  			request>headers>X-Liveswap-Secret delete
  		}
  	}
  	liveswap_webhook
  }
  ```

- Your app's **stdout/stderr is relayed** into hotserve's log. If your
  app prints its own secrets at startup, they end up in the journal —
  that one's on the app.

## Deploying from CI

### GitHub Actions

```yaml
- name: Build artifact
  run: |
    npm ci && npm run build
    tar -czf blog.tar.gz -C dist .

- name: Upload release
  uses: softprops/action-gh-release@v2
  with:
    tag_name: build-${{ github.run_number }}
    files: blog.tar.gz

- name: Deploy
  run: |
    curl --fail --max-time 600 -X POST \
      -H "Authorization: Bearer ${{ secrets.LIVESWAP_SECRET }}" \
      -d '{
        "url": "https://api.github.com/repos/${{ github.repository }}/releases/assets/'"$ASSET_ID"'",
        "version": "build-${{ github.run_number }}",
        "auth_header": "token ${{ secrets.GITHUB_TOKEN }}"
      }' \
      https://deploy.example.com/blog
```

(For public repos, the plain `browser_download_url` works and needs no
`auth_header`.)

### GitLab CI

```yaml
deploy:
  stage: deploy
  script:
    - tar -czf blog.tar.gz -C dist .
    - |
      curl --fail --header "JOB-TOKEN: $CI_JOB_TOKEN" \
        --upload-file blog.tar.gz \
        "$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz"
    - |
      curl --fail --max-time 600 -X POST \
        -H "Authorization: Bearer $LIVESWAP_SECRET" \
        -d "{
          \"url\": \"$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz\",
          \"version\": \"$CI_COMMIT_SHORT_SHA\",
          \"auth_header\": \"Bearer $DEPLOY_READ_TOKEN\"
        }" \
        https://deploy.example.com/blog
```

`--fail` makes the CI job red exactly when the deploy fails — and on
failure the previous version never stopped serving.

## Server layout

```
/var/lib/liveswap/blog/
  releases/v1.4.2/        one dir per deployed version
  releases/v1.4.1/
  current -> releases/v1.4.2   (convenience symlink; state.json is truth)
  shared/                 persistent data, survives deploys
  state.json              current version + process handle
  tmp/                    download staging
```

## Semantics and trade-offs (read this)

- **Apps are child processes of Caddy.** Config reloads never touch
  them (deploy state lives outside the config, reference-counted across
  reloads — proven by an e2e scenario that reloads mid-traffic and
  asserts the app's PID is unchanged). But when the **Caddy binary
  itself** restarts (upgrade, reboot), apps restart with it: on boot,
  liveswap relaunches each app's recorded current version and serves it
  as soon as the process is up. A systemd-backed runner that survives
  Caddy restarts is a designed-for v2 extension.
- **Changed app definitions apply on the next deploy**, never by
  restarting a running app mid-reload.
- **No post-promote auto-revert.** Once traffic cuts over, the deploy
  is done; if the new version misbehaves later, re-POST the previous
  version (its release dir is still on disk — that's what `keep` is
  for). Everything *before* promote is automatically contained.
- **One deploy at a time per app** — concurrent webhooks get 409, and
  CI retries are the queue. Different apps deploy in parallel.
- **Single node by design.** This is for the 1-server indie stack, not
  a cluster.
- **Unix only** (Linux servers, macOS dev). Windows is not supported.
- Deploy secrets and artifact-URL query strings never appear in logs.

## Development

liveswap lives in the [hotserve](../) monorepo; the make targets at the
repo root cover it (no local Go toolchain needed — everything runs in
Docker):

```sh
make test              # unit tests, all modules (race + coverage)
make test-integration  # real Caddy + real processes via caddytest
make e2e               # both module suites against the hotserve binary
make lint vet tidy
```

## License

Apache-2.0
