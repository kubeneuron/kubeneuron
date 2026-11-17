#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Every build and test input that is fetched from outside must be immutable
# (audit #24, C4): a GitHub Action by full commit SHA, a container image by
# digest. A tag can be moved to different content after it was reviewed; a
# digest or a commit SHA cannot.
#
# Checked: .github/workflows/*, build/Dockerfile, hack/*.sh. Images this
# repository builds or releases itself (kubeneuron-*, ghcr.io/kubeneuron/*,
# local fixtures) and shell variables are not external inputs and are not
# checked. Update procedure: docs/release-checklist.md, "Pinned inputs".
set -euo pipefail

cd "$(dirname "$0")/.."
fail=0
bad() {
	echo "verify-pins: $*" >&2
	fail=1
}

# Actions: owner/repo[/path]@<40 hex> (a local ./ workflow is fine).
while IFS= read -r hit; do
	bad "unpinned action (use a full commit SHA with the version in a comment): $hit"
done < <(grep -nE '^\s*-?\s*uses:\s*[^.[:space:]]' .github/workflows/*.y*ml | grep -vE 'uses:\s*[^@[:space:]]+@[0-9a-f]{40}(\s|$)' || true)

# Images: image: / --image= / FROM / docker run|pull <ref> with a tag and no digest.
ref='[a-z0-9][a-z0-9._/-]*:[A-Za-z0-9._-]+'
while IFS= read -r hit; do
	line=${hit#*:*:}
	[[ $line =~ ^[[:space:]]*# ]] && continue
	bad "image without a digest (pin name:tag@sha256:...): $hit"
done < <(grep -nE "(image:[[:space:]]*|--image[= ]|^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?|docker (run|pull) (--rm )?)${ref}([[:space:]\"']|$)" \
	.github/workflows/*.y*ml build/Dockerfile hack/*.sh |
	grep -vE "${ref}@sha256:[0-9a-f]{64}" |
	grep -vE '(kubeneuron-[a-z]+|ghcr\.io/kubeneuron/|dcgm-fixture|kindest/node:[^@]*\$)' || true)

if ((fail)); then
	echo "verify-pins: FAILED" >&2
	exit 1
fi
echo "verify-pins: OK (actions pinned by commit SHA, images by digest)"
