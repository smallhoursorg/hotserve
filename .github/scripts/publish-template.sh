#!/bin/sh
# Publishes one example as the next commit on its template repository.
#
#   .github/scripts/publish-template.sh <runtime> <tag> [owner/repo]
#
# `node v0.2.0` makes smallhoursorg/hotserve-example-node's main hold
# exactly what examples/node holds at v0.2.0 (the tree `git archive`
# gives, nothing added) and tags it v0.2.0. release.yml's templates job
# runs it on each release. From a laptop: a checkout with its tags,
# GH_TOKEN=$(gh auth token), and a scratch repository as the third
# argument.
#
# A repository made from a template carries none of its history, so
# the mirror's history is only a record: each release is a commit on
# top, never a force-push, and anything edited there by hand is
# overwritten by the next one (changes go to examples/ here). Running
# it again is harmless: the same tree makes no commit, and a tag the
# mirror already has on that commit pushes nothing. A tag the mirror
# has on another commit is refused, never moved.
set -eu

rt=${1:?runtime: node or deno}
tag=${2:?the release tag, e.g. v0.2.0}
repo=${3:-smallhoursorg/hotserve-example-$rt}
src=examples/$rt

sha=$(git rev-parse -q --verify "refs/tags/$tag^{commit}") ||
	{ echo "publish-template: no tag $tag here" >&2; exit 1; }

# The templates follow the newest release: a fix to an older line
# (v0.1.1 after v0.2.0) must not roll them back. Prereleases (a
# hyphen) never count, and the modules' liveswap/v* tags do not match.
newest=$(git tag -l 'v*' | grep -v -- - | sort -V | tail -n 1)
if [ "$tag" != "$newest" ]; then
	echo "publish-template: $tag is not the newest release ($newest); $repo left as it is"
	exit 0
fi
git cat-file -e "$tag:$src" 2>/dev/null ||
	{ echo "publish-template: $tag has no $src" >&2; exit 1; }

# gh answers git's credential requests with GH_TOKEN, for these two
# commands only: nothing is written to any git config.
auth() { git -c credential.helper= -c 'credential.helper=!gh auth git-credential' "$@"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
m=$work/mirror
auth clone --quiet --depth 1 "https://github.com/$repo.git" "$m"
# An empty repository (the first release) has no branch yet.
git -C "$m" checkout -q -B main

git -C "$m" rm -rq --ignore-unmatch .
git archive "$tag:$src" | tar -x -C "$m"
# -f: the example's own .gitignore must not drop a file it ships.
git -C "$m" add -A -f
if git -C "$m" diff --cached --quiet; then
	echo "publish-template: $repo already holds $src as of $tag"
else
	git -C "$m" -c user.name='github-actions[bot]' \
		-c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
		commit -q -F - <<EOF
hotserve $tag: $src

Generated from https://github.com/smallhoursorg/hotserve/tree/$tag/$src
($sha). Nothing here is edited by hand: changes go there.
EOF
fi
git -C "$m" tag -f "$tag" HEAD >/dev/null
auth -C "$m" push --quiet --atomic origin HEAD:refs/heads/main "refs/tags/$tag"
echo "publish-template: $repo main is $src as of $tag ($(git -C "$m" rev-parse --short HEAD))"
