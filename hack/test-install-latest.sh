#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# --version latest against a fake GitHub API and cluster: a repository whose
# only releases are pre-releases answers 404, and the installer must say so.
set -euo pipefail
REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cat >"$work/bin/curl" <<'FAKE'
#!/usr/bin/env bash
out=''
while (($#)); do case $1 in -o) out=$2; shift 2 ;; -w) shift 2 ;; *) shift ;; esac; done
[[ -z $out ]] || printf '%s' "${FAKE_LATEST_BODY:-}" >"$out"
printf '%s' "$FAKE_LATEST_CODE"
FAKE
cat >"$work/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
case "$*" in
version*) echo '{"serverVersion":{"minor": "33"}}' ;;
"get storageclass -o json") echo '{"storageclass.kubernetes.io/is-default-class": "true"}' ;;
esac
FAKE
printf '#!/usr/bin/env bash\nexit 1\n' >"$work/bin/gh"
chmod +x "$work/bin/"*
fail() { echo "install --version latest FAIL: $*" >&2; exit 1; }
run() { # $1=HTTP code, $2=response body
	FAKE_LATEST_CODE=$1 FAKE_LATEST_BODY=${2:-} PATH="$work/bin:$PATH" \
		bash "$REPO_ROOT/deploy/install.sh" --version latest 2>&1 || true
}
run 404 | grep -q 'no stable release is published yet; pass --version vX.Y.Z' ||
	fail "404 does not say that no stable release exists"
out=$(run 000)
grep -q 'could not resolve the latest release' <<<"$out" || fail "a network failure is not reported as one"
! grep -q 'no stable release' <<<"$out" || fail "a network failure is reported as a missing release"
run 200 '{"tag_name": "v1.0.0"}' | grep -q 'using release v1.0.0' || fail "the latest release tag is not used"
echo "install --version latest: OK (404 asks for --version, network failure, latest tag)"
