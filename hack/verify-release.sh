#!/usr/bin/env bash
# verify-release.sh — check a PUBLISHED release the way a stranger would.
#
# Everything else in this repository verifies inputs: the code, the manifests,
# the image we are about to push. Nothing verified the thing an operator
# actually consumes — the GitHub release — and three release candidates each
# found a defect no earlier gate could see, one of them CI diagnostics
# published as user-facing assets.
#
# So this downloads the release and asserts what install.md promises about it:
# the asset set is exactly the four kinds, the checksum file covers every one
# of them and matches, the cosign signature over that checksum file verifies
# against the GitHub OIDC issuer, the image digest table is present and
# digest-pinned, and the installer manifest references those same digests.
#
# Usage: hack/verify-release.sh <tag> [repo]
#   hack/verify-release.sh v0.2.2
#   hack/verify-release.sh v0.2.3-rc.1 kubeneuron/kubeneuron
set -euo pipefail

TAG=${1:?usage: verify-release.sh <tag> [owner/repo]}
REPO=${2:-${RELEASE_REPO:-kubeneuron/kubeneuron}}
GH_BIN=${GH_BIN:-gh}
fail=0
# Whether the signature was actually verified. The summary line must not claim
# it otherwise: a green "signature verified" on a machine with no cosign is the
# kind of over-claim this whole script exists to catch elsewhere.
signature_checked=1

note() { printf '\n=== %s\n' "$*"; }
bad() {
	echo "RELEASE CHECK FAILED: $*" >&2
	fail=1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

note "downloading ${REPO}@${TAG}"
"$GH_BIN" release download "$TAG" --repo "$REPO" --dir "$work" --clobber
ls -1 "$work"

# --- the asset set is closed --------------------------------------------------
# A release once shipped CI diagnostics as assets because the collecting step
# had no pattern. An operator cannot tell a diagnostic tarball from a
# deliverable, so the set is stated here and anything else fails.
note "asset allow-list"
shopt -s nullglob
for f in "$work"/*; do
	case "$(basename "$f")" in
	kubeneuronctl-* | sbom-* | images.txt | install.sh | install-*.sh | \
		kubeneuron-install-*.yaml | checksums-*.txt | checksums-*.txt.sig | checksums-*.txt.pem) ;;
	*) bad "unexpected asset published to users: $(basename "$f")" ;;
	esac
done

# --- checksums cover everything and match -------------------------------------
note "checksums"
sums="$work/checksums-${TAG}.txt"
if [[ ! -r $sums ]]; then
	bad "no checksums-${TAG}.txt in the release"
else
	(cd "$work" && sha256sum --check --quiet "checksums-${TAG}.txt") ||
		bad "at least one asset does not match its published checksum"
	# Coverage, not just correctness: a checksum file listing three of nine
	# assets passes --check and proves nothing about the other six.
	for f in "$work"/*; do
		name=$(basename "$f")
		case "$name" in
		checksums-*) continue ;;
		esac
		grep -qF "  $name" "$sums" || bad "asset ${name} is published but absent from the checksum file"
	done
fi

# --- the signature over the checksum file -------------------------------------
# Signing the checksum file rather than each asset is the point: one signature
# covers everything the file covers, which is why the coverage check above is
# load-bearing rather than pedantic.
note "cosign signature"
if command -v cosign >/dev/null 2>&1; then
	if [[ -r "$sums.sig" && -r "$sums.pem" ]]; then
		cosign verify-blob \
			--certificate "$sums.pem" \
			--signature "$sums.sig" \
			--certificate-identity-regexp "^https://github.com/${REPO}/" \
			--certificate-oidc-issuer https://token.actions.githubusercontent.com \
			"$sums" >/dev/null 2>&1 ||
			bad "the cosign signature over checksums-${TAG}.txt does not verify against ${REPO}'s GitHub OIDC identity"
	else
		bad "checksums-${TAG}.txt is published without a .sig/.pem pair"
	fi
else
	signature_checked=0
	echo "cosign not installed; skipping signature verification (install it to make this check real)" >&2
fi

# --- images are digest-pinned -------------------------------------------------
note "image digest table"
images="$work/images.txt"
if [[ ! -r $images ]]; then
	bad "no images.txt: nothing states which images this release is"
else
	while IFS= read -r line; do
		[[ -z $line ]] && continue
		grep -qE '@sha256:[a-f0-9]{64}$' <<<"$line" ||
			bad "images.txt entry is not digest-pinned: ${line}"
	done <"$images"
	# What a user actually applies must name those same digests, and each
	# artifact carries a different part of the install:
	#
	#   the manifest  -> the operator
	#   install.sh    -> the controller and the agent (via spec.*.image on the
	#                    root object the operator then reconciles)
	#
	# kubeneuronctl is published as an image for CI convenience and is deployed
	# by neither, so it is named in images.txt and nowhere else.
	digest_for() { grep -F "/$1@" "$images" | head -1 | sed 's/.*@//'; }

	for manifest in "$work"/kubeneuron-install-*.yaml; do
		want=$(digest_for operator)
		[[ -n $want ]] || bad "images.txt names no operator image"
		grep -qF "$want" "$manifest" ||
			bad "$(basename "$manifest") does not pin the operator at the digest images.txt names"
		# The manifest must not smuggle a tag-shaped reference either.
		if grep -qE "image: [^@[:space:]]+:[^@[:space:]]+$" "$manifest"; then
			bad "$(basename "$manifest") contains a tag-pinned image; a moved tag would change what an already-downloaded manifest installs"
		fi
	done

	for installer in "$work"/install-*.sh "$work"/install.sh; do
		[[ -r $installer ]] || continue
		for component in controller agent; do
			want=$(digest_for "$component")
			[[ -n $want ]] || {
				bad "images.txt names no ${component} image"
				continue
			}
			grep -qF "$want" "$installer" ||
				bad "$(basename "$installer") does not pin the ${component} at ${want}; it resolves a MOVEABLE TAG, so the install is not digest-pinned where it matters most"
		done
	done
fi

if ((fail)); then
	echo "verify-release: FAILED" >&2
	exit 1
fi
echo
summary="asset set closed, checksums complete and matching, images digest-pinned"
if ((signature_checked)); then
	summary="${summary}, signature verified"
else
	summary="${summary}; SIGNATURE NOT CHECKED (cosign absent)"
fi
echo "verify-release: OK (${REPO}@${TAG}: ${summary})"
