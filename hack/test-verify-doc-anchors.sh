#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cat >"$dir/target.md" <<'EOF'
# Target page
## Impact preview (read-only)
## `spec.safety` and **bold** _text_
## Repeated
## Repeated
Setext heading
--------------
<a id="explicit-anchor"></a>
```
## Not a heading
```
EOF
cat >"$dir/good.md" <<'EOF'
[a](target.md#impact-preview-read-only)
[b](target.md#specsafety-and-bold-text)
[c](target.md#repeated-1) and [d](target.md#setext-heading)
[e](target.md#explicit-anchor) [f](#local) [g](https://example.com/x.md#gone)
`[h](target.md#inside-code)`
[ref]: target.md#repeated
## Local
```
[i](target.md#inside-a-fence)
```
EOF
python3 -I hack/verify-doc-anchors.py "$dir/good.md"
cat >"$dir/bad.md" <<'EOF'
[a](target.md#impact-preview)
[b](target.md#not-a-heading)
[c](#nowhere)
[d](missing.md#x)
EOF
if output=$(python3 -I hack/verify-doc-anchors.py "$dir/bad.md" 2>&1); then
	echo "expected broken anchors to fail" >&2
	exit 1
fi
for location in 'bad.md:1: target.md#impact-preview' 'bad.md:2: target.md#not-a-heading' 'bad.md:3: #nowhere' 'bad.md:4: missing.md#x'; do
	if ! grep -F "$location" <<<"$output" >/dev/null; then
		printf 'Missing diagnostic %s\n%s\n' "$location" "$output" >&2
		exit 1
	fi
done
