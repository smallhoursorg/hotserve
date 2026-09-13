#!/bin/sh
# Builds app.tar.gz: two single executables (the app and its migration)
# at the archive root, each a copy of this machine's Node with the
# script inside it. So the box needs no Node — but the build has to
# run on the box's architecture, since the executable IS the Node
# binary it was built with (an arm64 box needs an arm64 build).
set -eu
cd "$(dirname "$0")/.."
rm -rf dist app.tar.gz
mkdir dist
for main in server migrate; do
	printf '{"main":"%s.js","output":"dist/%s","disableExperimentalSEAWarning":true}\n' "$main" "$main" >dist/sea-$main.json
	node --build-sea "dist/sea-$main.json"
	rm "dist/sea-$main.json"
done
tar -czf app.tar.gz -C dist .
echo "built app.tar.gz ($(wc -c < app.tar.gz | tr -d ' ') bytes, $(uname -m))"
