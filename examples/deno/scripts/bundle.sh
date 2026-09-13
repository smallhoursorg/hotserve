#!/bin/sh
# Builds app.tar.gz: the app's files plus a populated Deno module
# cache, all at the archive root. On the box the app runs with
# --cached-only and DENO_DIR pointing at that shipped cache, so the
# serving process never fetches a module. Build with the same Deno
# version the box runs.
set -eu
cd "$(dirname "$0")/.."
rm -rf dist app.tar.gz
mkdir dist
cp main.ts migrate.ts deno.json deno.lock dist/
(cd dist && DENO_DIR=.deno deno cache --frozen main.ts migrate.ts)
tar -czf app.tar.gz -C dist .
echo "built app.tar.gz ($(wc -c < app.tar.gz | tr -d ' ') bytes)"
