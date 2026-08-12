#!/usr/bin/env bash
# verify-docs.sh — mechanical guards against documentation rot.
#
# 1. Forbidden claims: every pattern in hack/stale-claims.txt is a status
#    claim that was once true and went false; any match in a published doc
#    fails the build. CHANGELOG.md is exempt (it records history).
# 2. Derived facts: the store migration heads stated in docs must equal the
#    highest migration files actually present, so a new migration cannot
#    silently outrun the docs.
# 3. Derived facts: the per-vendor capability matrix must agree with the
#    filesystem in BOTH directions — a vendor implementation that lands while
#    the matrix still says seam-only is stale, and a matrix that claims shipped
#    support with no implementation behind it is optimistic.
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

# --- derived facts: the per-vendor capability matrix ------------------------
# Table 1 of docs/reference-capabilities.md is the honest per-vendor statement,
# delimited by HTML comment markers so this check never reads another table.
# Column 4 is AMD, column 5 is Intel (field 1 is the empty span before the
# leading pipe). A vendor implementation would land in internal/agent/<vendor>*
# or internal/accelerator/<vendor>*; that is also where the matrix says to put
# it, so the two cannot be changed independently without failing here.
matrix=docs/reference-capabilities.md
matrix_column() {
	awk -F'|' -v col="$1" '
		/vendor-matrix:start/ { inside = 1; next }
		/vendor-matrix:end/   { inside = 0 }
		inside && /^\|/ && NF >= 6 { print $col }
	' "$matrix"
}
vendor_impl_files() {
	git ls-files "internal/agent/$1*" "internal/accelerator/$1*"
}
check_vendor_column() {
	local vendor=$1 column=$2 cells impl
	cells=$(matrix_column "$column")
	if [[ -z ${cells//[[:space:]]/} ]]; then
		echo "CAPABILITY MATRIX: no ${vendor} column rows found in $matrix (markers or table shape changed)" >&2
		fail=1
		return
	fi
	impl=$(vendor_impl_files "$vendor")
	if [[ -n $impl ]] && ! grep -q 'shipped' <<<"$cells"; then
		echo "CAPABILITY MATRIX STALE: ${vendor} implementation exists but every ${vendor} cell in $matrix is seam-only/not-applicable:" >&2
		echo "$impl" >&2
		fail=1
	fi
	if [[ -z $impl ]] && grep -q 'shipped' <<<"$cells"; then
		echo "CAPABILITY MATRIX OPTIMISTIC: $matrix claims shipped ${vendor} support, but no internal/agent/${vendor}* or internal/accelerator/${vendor}* package exists" >&2
		fail=1
	fi
}
if [[ -f $matrix ]]; then
	check_vendor_column amd 4
	check_vendor_column intel 5
else
	echo "CAPABILITY MATRIX MISSING: $matrix is the per-vendor source of truth and must exist" >&2
	fail=1
fi

# --- derived facts: the neutral fault catalog's AMD row count ---------------
# The capability matrix states how many AMD rows internal/detect/fault.go
# carries. That number is the only thing in the matrix a reader can check
# against the product's actual vendor coverage, so it must not be a number
# somebody typed once. A new (vendor, code) row that the matrix does not know
# about is exactly the drift this file exists to catch.
# Every vendor, not only AMD: a count that is checked for one vendor and typed
# by hand for the next is the same defect with a longer fuse.
check_vendor_rows() {
	local vendor=$1 label=$2 rows stated
	rows=$(grep -cE "^\s*\{Vendor: \"${vendor}\", Code: \"[^\"]+\"\}:" internal/detect/fault.go || true)
	if ! grep -qE "\([0-9]+ ${label} rows;" "$matrix"; then
		echo "CAPABILITY MATRIX: no ${label} fault-row count found in $matrix (the neutral fault catalog row changed shape)" >&2
		fail=1
		return
	fi
	stated=$(grep -oE "\([0-9]+ ${label} rows;" "$matrix" | grep -oE '[0-9]+' | head -1)
	if [[ $stated != "$rows" ]]; then
		echo "CAPABILITY MATRIX STALE: $matrix says ${stated} ${label} fault rows, internal/detect/fault.go has ${rows}" >&2
		fail=1
	fi
}
check_vendor_rows nvidia NVIDIA
check_vendor_rows amd AMD

# A vendor whose implementation exists must not be described as emitting
# nothing. This catches the specific paragraph that went stale for two rounds:
# prose claiming no non-NVIDIA source emits a fault, three screens below a
# table saying two of them do.
for vendor in amd intel; do
	if [[ -n $(git ls-files "internal/agent/${vendor}*" "internal/accelerator/${vendor}*") ]]; then
		if grep -qiE "no source emits a non-NVIDIA fault|no non-NVIDIA source" "$matrix"; then
			echo "CAPABILITY MATRIX CONTRADICTS ITSELF: $matrix claims no non-NVIDIA source emits a fault, but internal/agent/${vendor}* exists" >&2
			fail=1
			break
		fi
	fi
done

# --- derived facts: every spec.* path a doc names must exist in a CRD -------
# An invented field is the most expensive documentation defect: it reads as
# authoritative, an operator acts on it, and the action silently does nothing.
# docs/upgrade.md named `spec.execution.confinement` twice — in the section
# written specifically to make a blast-radius change safe to upgrade through —
# and every other check here passed.
if ! python3 hack/verify-spec-paths.py; then
	fail=1
fi

if ((fail)); then
	echo "verify-docs: FAILED — fix the claims above (or, if a claim became true again, remove its pattern)" >&2
	exit 1
fi
echo "verify-docs: OK (claims clean; migration heads sqlite ${sqlite_head} / postgres ${pg_head})"
