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
# Usage: hack/verify-release.sh <tag> [repo [release-id]]
#   hack/verify-release.sh v0.2.2
#   hack/verify-release.sh v0.2.3-rc.1 kubeneuron/kubeneuron
#   hack/verify-release.sh v0.6.0 kubeneuron/kubeneuron 405392108
#
# Without a release id the release is resolved by tag, which is what a user
# does and only works once it is published. With one, the exact release with
# that id is fetched through the authenticated API, which is the only way to
# reach a DRAFT: GitHub does not resolve a draft by tag (the v0.6.0 run proved
# it with `release not found`), and lists it only to a token with push access.
# The id is bound to <tag> and must still be a draft before anything is
# downloaded, so a wrong id fails instead of verifying some other release, and
# an already-published release is refused rather than re-verified as if it
# were still waiting to go out. Everything after the download is identical in
# both modes.
set -euo pipefail

TAG=${1:?usage: verify-release.sh <tag> [owner/repo [release-id]]}
REPO=${2:-${RELEASE_REPO:-kubeneuron/kubeneuron}}
RELEASE_ID=${3:-}
GH_BIN=${GH_BIN:-gh}
fail=0

# All three reach the shell, the API path, and file names. In CI they arrive
# from job outputs and the event; locally from whoever typed them. Refuse
# anything that is not the one shape each can legitimately have.
[[ $TAG =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || {
	echo "verify-release: tag '${TAG}' is not a vX.Y.Z[-pre] release tag" >&2
	exit 2
}
[[ $REPO =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || {
	echo "verify-release: repo '${REPO}' is not owner/name" >&2
	exit 2
}
if [[ -n $RELEASE_ID && ! $RELEASE_ID =~ ^[0-9]+$ ]]; then
	echo "verify-release: release id '${RELEASE_ID}' is not numeric" >&2
	exit 2
fi
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

# Download the exact release with that id through the API. Only the tag is
# accepted as proof the id is the right release; the asset list and each byte
# downloaded come from the same release object, never from a tag lookup.
download_by_id() {
	local release="repos/${REPO}/releases/${RELEASE_ID}"
	local meta got_tag got_draft id state name
	# One read of the release object gives both facts this mode insists on:
	# the id is the release for <tag>, and it is still a DRAFT. The id path
	# exists to verify a release before anyone can get it; a release that is
	# already published is past the point this check is meant to guard, so
	# accepting one here would make a pass mean less than it claims.
	meta=$("$GH_BIN" api "$release" --jq '"\(.tag_name)\t\(.draft)"') || {
		echo "release ${RELEASE_ID} is not readable in ${REPO} (a draft is visible only to a token with push access)" >&2
		return 1
	}
	IFS=$'\t' read -r got_tag got_draft <<<"$meta"
	[[ $got_tag == "$TAG" ]] || {
		echo "release ${RELEASE_ID} in ${REPO} is for tag '${got_tag}', not '${TAG}'" >&2
		return 1
	}
	[[ $got_draft == true ]] || {
		echo "release ${RELEASE_ID} in ${REPO} (${TAG}) is not a draft (draft=${got_draft}); this mode verifies a release BEFORE publication, so an already-published release is refused" >&2
		return 1
	}
	local -a assets=()
	mapfile -t assets < <("$GH_BIN" api "$release" --jq '.assets[] | "\(.id)\t\(.state)\t\(.name)"')
	for entry in "${assets[@]}"; do
		IFS=$'\t' read -r id state name <<<"$entry"
		# The name becomes a path under $work. GitHub already restricts asset
		# names, but the allow-list below is what this script trusts, not a
		# remote's input validation.
		[[ $id =~ ^[0-9]+$ && $name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || {
			echo "release ${RELEASE_ID} has an asset with an unexpected id/name: '${id}' '${name}'" >&2
			return 1
		}
		[[ $state == uploaded ]] || {
			echo "asset ${name} is in state '${state}', not 'uploaded'; the release is not finished" >&2
			return 1
		}
		"$GH_BIN" api -H 'Accept: application/octet-stream' \
			"repos/${REPO}/releases/assets/${id}" >"$work/$name" || {
			echo "downloading asset ${name} (id ${id}) failed" >&2
			return 1
		}
	done
}

if [[ -n $RELEASE_ID ]]; then
	note "downloading ${REPO}@${TAG} (release id ${RELEASE_ID}, by id: must be a draft)"
	download_by_id
else
	note "downloading ${REPO}@${TAG}"
	"$GH_BIN" release download "$TAG" --repo "$REPO" --dir "$work" --clobber
fi
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

# --- the asset set is COMPLETE ------------------------------------------------
# The allow-list above rejects assets that should not be there. Nothing
# required the ones that should — and every downstream check is written as a
# loop over a glob, so when the glob expands to nothing the loop simply does
# not run and the script reports OK.
#
# Run this against a release carrying only the CLI tarballs and images.txt and
# the old version printed "asset set closed, checksums complete and matching,
# images digest-pinned" — while never once looking at the installer or the
# manifest, which are the artifacts an operator actually applies. An empty
# dist/ is the exact failure mode of a broken release job, i.e. the one this
# script exists to catch.
note "asset set completeness"
#
# Count with an array, not `ls $pattern`. nullglob is set above, so an
# unmatched pattern expands to NOTHING and `ls -1d` would run bare, list the
# working directory, and report one match — the completeness check would then
# pass on precisely the release it exists to reject.
# Then filter by existence, because nullglob only removes patterns that
# CONTAIN a metacharacter: a literal path with no wildcard expands to itself
# whether or not the file is there, so the signature and certificate — the two
# entries named literally — would have passed on a release that has neither.
require_one() {
	local what=$1 pattern=$2 hit
	local -a hits present=()
	# shellcheck disable=SC2206 # the pattern must glob; that is the point.
	hits=($pattern)
	for hit in "${hits[@]}"; do
		[[ -e $hit ]] && present+=("$hit")
	done
	((${#present[@]} > 0)) ||
		bad "no ${what} in the release (${pattern}); every check that reads it silently did nothing"
}
require_one "kubeneuronctl archive" "$work/kubeneuronctl-*"
require_one "SBOM" "$work/sbom-*"
require_one "image digest table" "$work/images.txt"
require_one "install manifest" "$work/kubeneuron-install-*.yaml"
require_one "installer script" "$work/install*.sh"
require_one "checksum file" "$work/checksums-${TAG}.txt"
# The signature pair is required whether or not this machine can verify it.
# Absent cosign explains why the signature was not CHECKED; it does not excuse
# a release published without one.
require_one "cosign signature" "$work/checksums-${TAG}.txt.sig"
require_one "cosign certificate" "$work/checksums-${TAG}.txt.pem"

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
