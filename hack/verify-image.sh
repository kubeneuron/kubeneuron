#!/usr/bin/env bash
# verify-image.sh — assert the PUBLISHED image is the one the code assumes.
#
# Why this exists: for a long time the only image any automated suite ever ran
# was test/integration/Dockerfile — the host-built binaries dropped into FROM
# scratch. The shipped image is a different artifact: distroless rather than
# empty, carrying two host tools the agent shells out to, running as a
# different user. Every divergence between them is invisible to the tests and
# visible to whoever runs the release, which is how `Reboot` reached live
# hardware and exited 127 for want of nsenter.
#
# So this builds the real build/Dockerfile with the CLASSIC builder — the same
# path `make docker` takes, which has broken on its own before — and then
# asserts the properties that a Go test cannot see because they are properties
# of the image, not of the code.
#
# Usage: hack/verify-image.sh [tag]
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"

TAG=${1:-verify-image}
DOCKER_BIN=${DOCKER_BIN:-docker}
fail=0

note() { printf '\n=== %s\n' "$*"; }
bad() {
	echo "IMAGE CHECK FAILED: $*" >&2
	fail=1
}

note "building every published target from build/Dockerfile (classic builder)"
for target in operator controller agent cli; do
	echo "--- $target"
	"$DOCKER_BIN" build --target "$target" --tag "kubeneuron-${target}:${TAG}" \
		--file build/Dockerfile . >/dev/null
done

# --- every binary starts and identifies itself --------------------------------
# A distroless image has no shell, so an entrypoint that cannot resolve its
# own loader fails with a bare exit code and no message. Running each one is
# the only way to find that out before a cluster does.
note "each entrypoint runs"
for target in operator controller agent cli; do
	image="kubeneuron-${target}:${TAG}"
	case "$target" in
	cli) binary=kubeneuronctl ;;
	*) binary="kubeneuron-${target}" ;;
	esac
	if out=$("$DOCKER_BIN" run --rm "$image" --version 2>&1); then
		grep -q "$binary" <<<"$out" ||
			bad "$image --version printed '$out', which does not name $binary"
	else
		bad "$image could not run --version: $out"
	fi
done

# --- the host tools the agent shells out to -----------------------------------
# nsenter: Reboot asks the host's init, reachable only through PID 1's
# namespaces. dcgmi: with the GPU Operator installed the host has no dcgmi at
# all, so the agent carries its own or its runtime attestation stays degraded.
note "the agent carries the host tools it shells out to"
agent_image="kubeneuron-agent:${TAG}"

# Assert on the image FILESYSTEM, never on a command's output.
#
# The first version of this check grepped the combined output of running the
# binary for /usr|busybox|nsenter/. When the binary is absent the container
# never starts and Docker prints `exec: "/bin/nsenter": no such file or
# directory` — which contains the word nsenter, so the grep matched and the
# check passed. A gate added specifically to catch a missing nsenter could not
# catch a missing nsenter. Listing the layers cannot be fooled that way.
image_contains() {
	local image=$1 path=$2 cid listing rc=0
	cid=$("$DOCKER_BIN" create "$image") || return 1
	listing=$(mktemp)
	# Materialise the listing before grepping it. Piping `docker export | tar
	# -tf - | grep -q` looks natural and is wrong: grep exits at the first
	# match, tar then writes to a closed pipe and fails, and under `set -o
	# pipefail` the whole pipeline reports failure — so a file that IS present
	# was reported missing. It failed in the safe direction, which is exactly
	# why it survived a local run and only showed up in CI.
	"$DOCKER_BIN" export "$cid" >"$listing.tar" 2>/dev/null || rc=1
	if ((rc == 0)); then
		tar -tf "$listing.tar" >"$listing" 2>/dev/null || rc=1
	fi
	if ((rc == 0)) && ! grep -qx "${path#/}" "$listing"; then
		rc=1
	fi
	rm -f "$listing" "$listing.tar"
	"$DOCKER_BIN" rm -f "$cid" >/dev/null 2>&1 || true
	return $rc
}
image_contains "$agent_image" /bin/nsenter ||
	bad "$agent_image has no /bin/nsenter; the Reboot action will exit 127 on a real node"
image_contains "$agent_image" /usr/bin/dcgmi ||
	bad "$agent_image has no /usr/bin/dcgmi; the agent's runtime attestation stays degraded"
if out=$("$DOCKER_BIN" run --rm --entrypoint /usr/bin/dcgmi "$agent_image" --version 2>&1); then
	grep -qE 'version: *[0-9]' <<<"$out" ||
		bad "$agent_image dcgmi --version printed '$out' with no version"
else
	bad "$agent_image has no working /usr/bin/dcgmi: $out"
fi

# --- the user each image runs as ----------------------------------------------
# The agent runs privileged with hostPID and must read /dev/kmsg, so it is
# root by design; everything else must not be. A silent flip either way is a
# security posture change nobody reviewed.
note "each image runs as the user its deployment assumes"
expect_user() {
	local target=$1 want=$2 got
	got=$("$DOCKER_BIN" inspect --format '{{.Config.User}}' "kubeneuron-${target}:${TAG}")
	got=${got%%:*}
	[[ -z $got ]] && got=0
	[[ $got == "$want" ]] || bad "kubeneuron-${target} runs as user '${got}', want '${want}'"
}
expect_user operator 65532
expect_user controller 65532
expect_user cli 65532
expect_user agent 0

# --- provenance labels --------------------------------------------------------
note "each image is labelled with its source and licence"
for target in operator controller agent cli; do
	image="kubeneuron-${target}:${TAG}"
	src=$("$DOCKER_BIN" inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' "$image")
	lic=$("$DOCKER_BIN" inspect --format '{{index .Config.Labels "org.opencontainers.image.licenses"}}' "$image")
	[[ $src == https://github.com/* ]] || bad "$image has no OCI source label (got '${src}')"
	[[ $lic == Apache-2.0 ]] || bad "$image licence label is '${lic}', want Apache-2.0"
done

if ((fail)); then
	echo "verify-image: FAILED" >&2
	exit 1
fi
echo
echo "verify-image: OK (four targets built from build/Dockerfile, entrypoints run, host tools present, users as declared)"
