#!/usr/bin/env bash
# mirror.sh — publish this development tree to the public repository.
#
# This procedure used to be a sequence of commands typed by hand. That was
# acceptable while the private repository ran CI on every push; it stopped
# being acceptable when Actions was disabled there, because the mirror is now
# the ENFORCEMENT BOUNDARY: public CI validates whatever this step produces,
# and if the exclusion list drops or adds a file, it validates a tree nobody
# developed.
#
# So the exclusion list lives here, in one place, and the script asserts
# afterwards that the published tree differs from this one by exactly those
# exclusions and nothing else.
#
# It does NOT commit or push. It prepares the mirror, prints the diff, and
# stops — publishing is a decision, not a side effect.
#
# Usage:
#   hack/mirror.sh [target-dir]
#
# Env:
#   PUBLIC_REPO   default kubeneuron/kubeneuron
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"

PUBLIC_REPO=${PUBLIC_REPO:-kubeneuron/kubeneuron}
TARGET=${1:-${MIRROR_DIR:-}}

# --- the exclusion list -------------------------------------------------------
# Every entry is a file that must NEVER reach the public repository, with the
# reason it must not. Anything added here must also be added to the doc-lint
# skip list in hack/verify-docs.sh, which knows the same three documents.
EXCLUDES=(
	# Agent working state: session checkpoints, handoff notes, and a scratch
	# blocker file. Internal process, not product.
	"AGENT_SESSION_STATE.md"
	"TRANSFER_HANDOFF.md"
	"blocker.md"
	# NOT excluded, deliberately: PRODUCT_PLAN.md and
	# PRODUCTION_READINESS_PLAN.md. They are published product roadmap
	# documents — hack/verify-docs.sh lints them as such, and they have been in
	# the public repository since before this script existed. Excluding them
	# here would silently delete two published files on the next mirror.
	# Build output and local tooling.
	"bin/"
	".claude/"
	".git/"
)

note() { printf '\n=== %s\n' "$*"; }
die() {
	echo "mirror: $*" >&2
	exit 1
}

[[ -n $TARGET ]] || die "usage: hack/mirror.sh <target-dir>   (a checkout of ${PUBLIC_REPO})"
[[ -d $TARGET/.git ]] || die "$TARGET is not a git checkout; clone ${PUBLIC_REPO} there first"

target_remote=$(git -C "$TARGET" remote get-url origin 2>/dev/null || true)
case "$target_remote" in
*"${PUBLIC_REPO}"*) ;;
*) die "$TARGET's origin is '${target_remote}', not ${PUBLIC_REPO}; refusing to mirror into the wrong repository" ;;
esac

if [[ -n $(git -C "$TARGET" status --porcelain) ]]; then
	die "$TARGET has uncommitted changes; commit or discard them before mirroring onto it"
fi

note "syncing the tree"
rsync_args=(-a --delete)
# Honour .gitignore as well as the explicit list above.
#
# The explicit list names three documents and three directories. It cannot name
# the working tree's debris, and rsync copied all of it: a 126 MB
# kubeneuron-controller build output at the repository root rode into the
# public checkout on every mirror. It was never COMMITTED, because the mirrored
# .gitignore ignores it there too — which is precisely why nobody noticed. The
# mirror should carry what git tracks, not whatever happens to be lying around.
rsync_args+=(--filter="dir-merge,- .gitignore")
for e in "${EXCLUDES[@]}"; do
	rsync_args+=(--exclude "$e")
done
rsync "${rsync_args[@]}" "$repo_root/" "$TARGET/"

# --- the assertion the hand-typed procedure never made ------------------------
# Compare the two trees directly. Anything that differs and is not an exclusion
# is either a file rsync failed to copy or one that should not be here — both
# are the drift this script exists to make impossible.
note "verifying the published tree differs by exactly the exclusions"
listing() {
	git -C "$1" ls-files
}
mapfile -t here < <(listing "$repo_root")
mapfile -t there < <(git -C "$TARGET" status --porcelain --untracked-files=all | sed 's/^...//')

excluded() {
	local path=$1 e
	for e in "${EXCLUDES[@]}"; do
		[[ $path == "$e" || $path == "${e%/}/"* ]] && return 0
	done
	return 1
}

unexpected=0
declare -A tracked=()
for path in "${here[@]}"; do
	tracked["$path"]=1
	if excluded "$path"; then
		if [[ -e "$TARGET/$path" ]]; then
			echo "LEAKED: $path is on the exclusion list but is present in the mirror" >&2
			unexpected=1
		fi
		continue
	fi
	if [[ ! -e "$TARGET/$path" ]]; then
		echo "MISSING: $path is tracked here but absent from the mirror" >&2
		unexpected=1
	fi
done

# The other direction, which this script claimed in its header and did not
# check: anything the mirror would ADD.
#
# `there` was computed and then never read — shellcheck SC2034, and shellcheck
# is not in make gates, so nothing said so. The loop above only walks this
# tree's tracked files, so it can report MISSING and LEAKED and never ADDED.
# Meanwhile rsync copies every untracked, non-ignored working-tree file into
# the checkout and the script signs off with `git add -A && git commit && git
# push` — so a scratch file left beside the code is published, and the
# assertion written to make that impossible was inert.
for path in "${there[@]}"; do
	[[ -n $path ]] || continue
	[[ -n ${tracked["$path"]:-} ]] && continue
	excluded "$path" && continue
	echo "ADDED: $path is in the mirror but is not tracked here; it would be published" >&2
	unexpected=1
done
((unexpected == 0)) || die "the mirror does not match this tree; fix before publishing"

# --- the grep sweep -----------------------------------------------------------
# A last look for a published DOCUMENT that cites an unpublished one — how an
# internal note leaks by reference rather than by copy. A reader who follows
# such a link gets a 404 on a repository that is supposed to be self-contained.
#
# Prose only. Tooling that names these files in its own skip lists
# (.dockerignore, verify-spec-paths.py, this script) is doing exactly what it
# should, and flagging it would train everyone to ignore this check.
note "sweeping published prose for references to excluded documents"
sweep=0
for e in AGENT_SESSION_STATE.md TRANSFER_HANDOFF.md blocker.md; do
	if hits=$(git -C "$TARGET" grep -n --untracked -F -- "$e" -- '*.md' 2>/dev/null); then
		echo "A PUBLISHED DOCUMENT REFERENCES $e, WHICH IS NOT PUBLISHED:" >&2
		echo "$hits" >&2
		sweep=1
	fi
done
((sweep == 0)) || die "published documents reference documents that are not published"

note "what would be published"
git -C "$TARGET" status --short | head -50
changed=$(git -C "$TARGET" status --porcelain | wc -l | tr -d ' ')
echo
echo "mirror: OK — ${changed} path(s) staged in ${TARGET}"
echo
echo "Nothing has been committed or pushed. Review the diff, then in ${TARGET}:"
echo "  git add -A && git commit && git push"
