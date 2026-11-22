#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# hack/verify-dco.sh on a throwaway repository: a range, and --root on a
# repository whose only commit is its root.
set -euo pipefail
script=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/verify-dco.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail() { echo "verify-dco test FAIL: $*" >&2; exit 1; }
git -C "$work" init -q
g() { git -C "$work" -c user.name=Tester -c user.email=tester@example.com -c commit.gpgsign=false "$@"; }
g commit -q --allow-empty -s -m "root"
(cd "$work" && bash "$script" --root) | grep -q 'OK (1 commit' || fail "a signed-off root commit is rejected"
root=$(g rev-parse HEAD)
g commit -q --allow-empty -m "unsigned"
! (cd "$work" && bash "$script" "$root") >/dev/null 2>&1 || fail "an unsigned commit in the range passes"
! (cd "$work" && bash "$script" --root) >/dev/null 2>&1 || fail "an unsigned commit after the root passes"
git -C "$work" checkout -q --orphan unsigned-root
g commit -q --allow-empty -m "unsigned root"
! (cd "$work" && bash "$script" --root) >/dev/null 2>&1 || fail "an unsigned root commit passes"
echo "verify-dco: OK (range, signed root, unsigned root and unsigned successor)"
