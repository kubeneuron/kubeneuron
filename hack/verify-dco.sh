#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Developer Certificate of Origin: every commit in <base>..<head> carries a
# "Signed-off-by: Name <email>" line for its own author. The line is the
# author's statement that they may contribute the change under Apache-2.0
# (https://developercertificate.org/); it is never added on an outside
# author's behalf.
#
# Usage: hack/verify-dco.sh <base> [head]   (head defaults to HEAD)
#        hack/verify-dco.sh --root [head]   (every commit reachable from head,
#                                            the root commit included)
set -euo pipefail

base=${1:?usage: verify-dco.sh <base>|--root [head]}
head=${2:-HEAD}
range="${base}..${head}"
[[ $base != --root ]] || range=$head
fail=0
count=0
while IFS= read -r commit; do
	count=$((count + 1))
	author=$(git show -s --format='%an <%ae>' "$commit")
	if ! git show -s --format='%B' "$commit" | grep -qxF "Signed-off-by: ${author}"; then
		echo "verify-dco: $(git show -s --format='%h %s' "$commit") has no 'Signed-off-by: ${author}' (commit with git commit -s)" >&2
		fail=1
	fi
done < <(git rev-list --no-merges "$range")
if ((fail)); then
	echo "verify-dco: FAILED" >&2
	exit 1
fi
echo "verify-dco: OK (${count} commit(s) signed off by their authors)"
