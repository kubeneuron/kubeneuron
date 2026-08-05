#!/usr/bin/env bash
# verify-docs.sh — mechanical guards against documentation rot.
#
# 1. Forbidden claims: every pattern in hack/stale-claims.txt is a status
#    claim that was once true and went false; any match in a published doc
#    fails the build. CHANGELOG.md is exempt (it records history).
# 2. Derived facts: the store migration heads stated in docs must equal the
#    highest migration files actually present, so a new migration cannot
#    silently outrun the docs.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"

fail=0

# --- forbidden claims -------------------------------------------------------
# CHANGELOG records history; the session-state and handoff documents are
# private checkpoints that never reach the public mirror.
mapfile -t doc_files < <(git ls-files '*.md' |
	grep -v -E '^(CHANGELOG\.md|AGENT_SESSION_STATE\.md|TRANSFER_HANDOFF\.md|blocker\.md)$')
while IFS= read -r pattern; do
	[[ -z $pattern || $pattern == \#* ]] && continue
	if hits=$(grep -nE -- "$pattern" "${doc_files[@]}" 2>/dev/null); then
		echo "FORBIDDEN CLAIM (pattern: $pattern):" >&2
		echo "$hits" >&2
		fail=1
	fi
done < hack/stale-claims.txt

# --- derived facts: migration heads ----------------------------------------
head_of() {
	local f head=""
	for f in "$1"/[0-9][0-9][0-9][0-9]_*.sql; do
		f=$(basename "$f")
		head=${f:0:4}
	done
	printf '%s' "$head"
}
sqlite_head=$(head_of internal/store/sqlite/migrations)
pg_head=$(head_of internal/store/postgres/migrations)
doc=docs/design.md
if grep -qE 'sqlite [0-9]{4} / (postgres|pg) [0-9]{4}' "$doc"; then
		if ! grep -qE "sqlite ${sqlite_head} / (postgres|pg) ${pg_head}" "$doc"; then
			echo "STALE MIGRATION HEADS in $doc: filesystem says sqlite ${sqlite_head} / postgres ${pg_head}" >&2
			fail=1
		fi
	fi

if ((fail)); then
	echo "verify-docs: FAILED — fix the claims above (or, if a claim became true again, remove its pattern)" >&2
	exit 1
fi
echo "verify-docs: OK (claims clean; migration heads sqlite ${sqlite_head} / postgres ${pg_head})"
