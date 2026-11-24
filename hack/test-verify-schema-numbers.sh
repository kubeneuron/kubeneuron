#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
checker="$PWD/hack/verify-schema-numbers.py"
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cd "$dir"
mkdir docs
echo '## Upgrading from v1.0' > docs/upgrade.md
cat > CHANGELOG.md <<'MD'
## [Unreleased]

## [v1.2]
SQLite 0039
## [v1.1]
SQLite 0038
MD
if python3 -I "$checker" 0040 0031 2>error; then
    echo "expected a stale schema in the newly cut release" >&2
    exit 1
fi
grep -qxF 'CHANGELOG.md:4: highest SQLite schema mentioned is 0039; expected migration head 0040' error
# A current migration-free entry leaves the older release outside the check.
sed '2s/^$/A documentation fix./' CHANGELOG.md > populated
mv populated CHANGELOG.md
python3 -I "$checker" 0040 0031
