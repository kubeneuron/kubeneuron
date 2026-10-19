#!/usr/bin/env bash
# verify-tls-scripts.sh — offline regression coverage for the optimistic
# concurrency path of hack/tls-rotate.sh and hack/tls-emergency-recover.sh.
#
# Both helpers record a phase with a resourceVersion-checked merge patch built
# from a root read taken before slow Secret/certificate validation. The operator
# writes root status on every reconcile, and each status write advances
# resourceVersion, so a live kind run lost that race:
#   Operation cannot be fulfilled on kubeneurons.kubeneuron.io "integration-smoke":
#   the object has been modified; please apply your changes to the latest
#   version and try again
# This harness drives the real scripts against a scripted kubectl that keeps a
# root object in a state directory, enforces the compare-and-swap exactly like
# the API server (a stale resourceVersion is a Conflict; a patch WITHOUT one is
# a harness failure, so a "fix" that drops the check cannot pass), and lets a
# scenario inject a concurrent writer on the first N patch attempts. It proves:
#   - a conflict caused by a status-only or unrelated-metadata write is retried
#     against the fresh resourceVersion and succeeds without clobbering the
#     unrelated change;
#   - a forward phase waits for the re-read root to be Ready before retrying,
#     a rollback phase does not;
#   - a root that already carries exactly the planned result is accepted as an
#     idempotent no-op without a second write;
#   - a foreign transaction, a plan lookalike with a different Secret UID, a
#     spec change, or a root that became Ready during emergency recovery fails
#     closed with an explicit drift diagnostic and no further patch;
#   - the retry bound and a non-conflict error are reported explicitly.
# No cluster, no network; a few seconds.
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
rotate_script="$repo_root/hack/tls-rotate.sh"
emergency_script="$repo_root/hack/tls-emergency-recover.sh"
JQ_BIN=${JQ_BIN:-jq}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail=0
note() { printf 'verify-tls-scripts: %s\n' "$*"; }
bad() {
	printf 'verify-tls-scripts: FAIL: %s\n' "$*" >&2
	fail=1
}

for command in bash "$JQ_BIN" openssl base64 timeout; do
	command -v "$command" >/dev/null 2>&1 || {
		printf 'verify-tls-scripts: required command not found: %s\n' "$command" >&2
		exit 1
	}
done

# --- static ------------------------------------------------------------------
for script in "$rotate_script" "$emergency_script"; do
	bash -n "$script" || bad "bash -n failed for $script"
done
if command -v shellcheck >/dev/null 2>&1; then
	shellcheck "$rotate_script" "$emergency_script" || bad "shellcheck reported findings"
else
	echo "verify-tls-scripts: shellcheck not installed, skipped" >&2
fi
for script in "$rotate_script" "$emergency_script"; do
	grep -q 'PATCH_CONFLICT_RETRIES=\${PATCH_CONFLICT_RETRIES:-5}' "$script" ||
		bad "$script lacks the documented PATCH_CONFLICT_RETRIES default"
	grep -q 'metadata.resourceVersion = \$rv' "$script" ||
		bad "$script no longer sets metadata.resourceVersion on the phase patch (the compare-and-swap must stay)"
done

# --- fixtures ----------------------------------------------------------------
root_name=integration-smoke
namespace=kubeneuron-system
root_uid='root-uid-0001'

certs="$work/certs"
mkdir -p "$certs"
gen_ca() {
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
		-keyout "$certs/$1.key" -out "$certs/$1.crt" -subj "/CN=$1" -days 3 \
		-addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' >/dev/null 2>&1
}
gen_leaf() {
	local name=$1 ca=$2 san=$3 eku=$4
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
		-keyout "$certs/$name.key" -out "$certs/$name.csr" -subj "/CN=$name" >/dev/null 2>&1
	printf 'subjectAltName=%s\nextendedKeyUsage=%s\nkeyUsage=critical,digitalSignature\nbasicConstraints=CA:FALSE\n' \
		"$san" "$eku" >"$certs/$name.ext"
	openssl x509 -req -in "$certs/$name.csr" -CA "$certs/$ca.crt" -CAkey "$certs/$ca.key" \
		-CAcreateserial -out "$certs/$name.crt" -days 2 -extfile "$certs/$name.ext" >/dev/null 2>&1
}
gen_ca server-ca
gen_ca client-ca
gen_leaf server-leaf server-ca "DNS:${root_name}-controller.${namespace}.svc" serverAuth
gen_leaf client-leaf client-ca "URI:spiffe://kubeneuron.io/installation/${root_uid}/agent" clientAuth
b64() { base64 -w0 <"$1"; }
server_crt=$(b64 "$certs/server-leaf.crt")
server_key=$(b64 "$certs/server-leaf.key")
client_crt=$(b64 "$certs/client-leaf.crt")
client_key=$(b64 "$certs/client-leaf.key")
server_ca_pem=$(b64 "$certs/server-ca.crt")
client_ca_pem=$(b64 "$certs/client-ca.crt")

fixture="$work/fixture"
mkdir -p "$fixture/secrets"
# write_secret <name> <uid> <tls|ca> <immutable> <crt> <key-or-ca-pem>
write_secret() {
	local name=$1 uid=$2 kind=$3 immutable=$4 crt=$5 key=$6
	if [[ $kind == tls ]]; then
		$JQ_BIN -n --arg name "$name" --arg uid "$uid" --argjson immutable "$immutable" --arg crt "$crt" --arg key "$key" '
		  {apiVersion:"v1", kind:"Secret", type:"kubernetes.io/tls",
		   metadata:{name:$name, namespace:"kubeneuron-system", uid:$uid}, immutable:$immutable,
		   data:{"tls.crt":$crt, "tls.key":$key}}'
	else
		$JQ_BIN -n --arg name "$name" --arg uid "$uid" --argjson immutable "$immutable" --arg ca "$crt" '
		  {apiVersion:"v1", kind:"Secret", type:"Opaque",
		   metadata:{name:$name, namespace:"kubeneuron-system", uid:$uid}, immutable:$immutable,
		   data:{"ca.crt":$ca}}'
	fi >"$fixture/secrets/$name.json"
}
write_secret "$root_name-controller-tls" uid-old-server-leaf tls false "$server_crt" "$server_key"
write_secret "$root_name-controller-server-ca" uid-old-server-ca ca false "$server_ca_pem" ''
write_secret "$root_name-controller-tls-v2" uid-new-server-leaf tls true "$server_crt" "$server_key"
write_secret "$root_name-controller-server-ca-overlap-v2" uid-overlap-server-ca ca true "$server_ca_pem" ''
write_secret "$root_name-controller-server-ca-v2" uid-final-server-ca ca true "$server_ca_pem" ''
write_secret "$root_name-agent-tls" uid-old-client-leaf tls false "$client_crt" "$client_key"
write_secret "$root_name-agent-client-ca" uid-old-client-ca ca false "$client_ca_pem" ''
write_secret "$root_name-agent-tls-v2" uid-new-client-leaf tls true "$client_crt" "$client_key"
write_secret "$root_name-agent-client-ca-overlap-v2" uid-overlap-client-ca ca true "$client_ca_pem" ''
write_secret "$root_name-agent-client-ca-v2" uid-final-client-ca ca true "$client_ca_pem" ''
write_secret "$root_name-controller-tls-emergency-v2" uid-emergency-server-leaf tls true "$server_crt" "$server_key"
write_secret "$root_name-agent-tls-emergency-v2" uid-emergency-client-leaf tls true "$client_crt" "$client_key"

# Three roots. "fresh": Ready, no transaction, the start of a server rotation.
# "client-leaf-activated": a client rotation at LeafActivated whose root is NOT
# Ready, the state rollback-leaf exists for. "unready": a Ready=False root with
# no transaction, the state emergency recovery exists for.
base_root=$($JQ_BIN -n --arg name "$root_name" --arg ns "$namespace" --arg uid "$root_uid" '
  {apiVersion:"kubeneuron.io/v1alpha1", kind:"KubeNeuron",
   metadata:{name:$name, uid:$uid, generation:3, resourceVersion:"100",
     labels:{team:"a"}, annotations:{"unrelated.example/owner":"team-a"}},
   spec:{namespace:$ns, tls:{
     serverSecretRef:{name:($name+"-controller-tls")},
     serverCASecretRef:{name:($name+"-controller-server-ca")},
     clientSecretRef:{name:($name+"-agent-tls")},
     clientCASecretRef:{name:($name+"-agent-client-ca")}}},
   status:{observedGeneration:3, conditions:[
     {type:"Ready", status:"True", reason:"RuntimeAvailable", observedGeneration:3, message:"runtime available"}]}}')
printf '%s\n' "$base_root" >"$fixture/root-fresh.json"
$JQ_BIN '.status.conditions[0] |= (.status="False" | .reason="RuntimeUnavailable" | .message="leaf expired")' \
	<<<"$base_root" >"$fixture/root-unready.json"
client_plan_annotations=$($JQ_BIN -n --arg name "$root_name" '
  {"kubeneuron.io/tls-rotation-id":"client-routine-v2",
   "kubeneuron.io/tls-rotation-direction":"client",
   "kubeneuron.io/tls-rotation-phase":"LeafActivated",
   "kubeneuron.io/tls-rotation-from-leaf":($name+"-agent-tls"),
   "kubeneuron.io/tls-rotation-from-leaf-uid":"uid-old-client-leaf",
   "kubeneuron.io/tls-rotation-from-ca":($name+"-agent-client-ca"),
   "kubeneuron.io/tls-rotation-from-ca-key":"ca.crt",
   "kubeneuron.io/tls-rotation-from-ca-uid":"uid-old-client-ca",
   "kubeneuron.io/tls-rotation-new-leaf":($name+"-agent-tls-v2"),
   "kubeneuron.io/tls-rotation-new-leaf-uid":"uid-new-client-leaf",
   "kubeneuron.io/tls-rotation-overlap-ca":($name+"-agent-client-ca-overlap-v2"),
   "kubeneuron.io/tls-rotation-overlap-ca-key":"ca.crt",
   "kubeneuron.io/tls-rotation-overlap-ca-uid":"uid-overlap-client-ca",
   "kubeneuron.io/tls-rotation-final-ca":($name+"-agent-client-ca-v2"),
   "kubeneuron.io/tls-rotation-final-ca-key":"ca.crt",
   "kubeneuron.io/tls-rotation-final-ca-uid":"uid-final-client-ca"}')
$JQ_BIN --argjson plan "$client_plan_annotations" --arg name "$root_name" '
  .metadata.annotations += $plan
  | .spec.tls.clientSecretRef = {name:($name+"-agent-tls-v2")}
  | .spec.tls.clientCASecretRef = {name:($name+"-agent-client-ca-overlap-v2")}
  | .status.conditions[0] |= (.status="False" | .reason="RuntimeUnavailable" | .message="new leaf rejected")' \
	<<<"$base_root" >"$fixture/root-client-leaf-activated.json"

# The exact annotations hack/tls-rotate.sh writes for `server expand-trust
# --rotation-id server-routine-v2` against these fixtures, and the exact ones
# hack/tls-emergency-recover.sh writes for `--recovery-id emergency-v2`. They
# let a scenario stage "a concurrent identical invocation already won".
server_expand_annotations=$($JQ_BIN -n --arg name "$root_name" '
  {"kubeneuron.io/tls-rotation-id":"server-routine-v2",
   "kubeneuron.io/tls-rotation-direction":"server",
   "kubeneuron.io/tls-rotation-phase":"TrustExpanded",
   "kubeneuron.io/tls-rotation-from-leaf":($name+"-controller-tls"),
   "kubeneuron.io/tls-rotation-from-leaf-uid":"uid-old-server-leaf",
   "kubeneuron.io/tls-rotation-from-ca":($name+"-controller-server-ca"),
   "kubeneuron.io/tls-rotation-from-ca-key":"ca.crt",
   "kubeneuron.io/tls-rotation-from-ca-uid":"uid-old-server-ca",
   "kubeneuron.io/tls-rotation-new-leaf":($name+"-controller-tls-v2"),
   "kubeneuron.io/tls-rotation-new-leaf-uid":"uid-new-server-leaf",
   "kubeneuron.io/tls-rotation-overlap-ca":($name+"-controller-server-ca-overlap-v2"),
   "kubeneuron.io/tls-rotation-overlap-ca-key":"ca.crt",
   "kubeneuron.io/tls-rotation-overlap-ca-uid":"uid-overlap-server-ca",
   "kubeneuron.io/tls-rotation-final-ca":($name+"-controller-server-ca-v2"),
   "kubeneuron.io/tls-rotation-final-ca-key":"ca.crt",
   "kubeneuron.io/tls-rotation-final-ca-uid":"uid-final-server-ca"}')
emergency_annotations=$($JQ_BIN -n --arg name "$root_name" '
  {"kubeneuron.io/tls-emergency-recovery-id":"emergency-v2",
   "kubeneuron.io/tls-emergency-recovery-server-leaf":($name+"-controller-tls-emergency-v2"),
   "kubeneuron.io/tls-emergency-recovery-server-leaf-uid":"uid-emergency-server-leaf",
   "kubeneuron.io/tls-emergency-recovery-client-leaf":($name+"-agent-tls-emergency-v2"),
   "kubeneuron.io/tls-emergency-recovery-client-leaf-uid":"uid-emergency-client-leaf",
   "kubeneuron.io/tls-emergency-recovery-previous-server-leaf":($name+"-controller-tls"),
   "kubeneuron.io/tls-emergency-recovery-previous-client-leaf":($name+"-agent-tls")}')

# --- the scripted kubectl ----------------------------------------------------
# State lives in $FAKE_STATE: root.json, secrets/<name>.json, and the knobs
# conflicts_remaining (patch attempts to reject while running on_conflict.jq
# against the root as the concurrent writer), unready_gets (root reads to
# report Ready=False before serving the stored root) and fail_mode (one
# non-conflict patch failure). Every patch attempt is appended to patch.log;
# every applied one to patch.success.log. A successful patch acts as an instant
# operator: resourceVersion advances, generation advances when spec changed,
# and status is Ready at the new generation.
fakebin="$work/bin"
mkdir -p "$fakebin"
cat >"$fakebin/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
state=${FAKE_STATE:?FAKE_STATE must point at the scenario state directory}
root="$state/root.json"
conflict_message='Error from server (Conflict): Operation cannot be fulfilled on kubeneurons.kubeneuron.io "integration-smoke": the object has been modified; please apply your changes to the latest version and try again'
counter() {
	local file="$state/$1" value
	value=$(cat "$file" 2>/dev/null || echo 0)
	printf '%s' "$value"
}
decrement() {
	local file="$state/$1" value
	value=$(counter "$1")
	printf '%s\n' "$((value - 1))" >"$file"
}
args=()
patch=''
while (($#)); do
	case $1 in
	-n | --kubeconfig | -o) shift 2 ;;
	-p)
		patch=$2
		shift 2
		;;
	--*=*) shift ;;
	*)
		args+=("$1")
		shift
		;;
	esac
done
case "${args[*]}" in
"get kubeneuron integration-smoke")
	if (($(counter unready_gets) > 0)); then
		decrement unready_gets
		jq '.status.conditions |= map(if .type == "Ready" then .status = "False" | .reason = "RuntimeUnavailable" else . end)' "$root"
	else
		cat "$root"
	fi
	;;
"get secret "*)
	name=${args[2]}
	if [[ -r $state/secrets/$name.json ]]; then
		cat "$state/secrets/$name.json"
	else
		printf 'Error from server (NotFound): secrets "%s" not found\n' "$name" >&2
		exit 1
	fi
	;;
"get daemonset "* | "get deployment "*)
	jq --arg kind "${args[1]}" --arg name "${args[2]}" '
	  {apiVersion:"apps/v1", kind:$kind, metadata:{name:$name, generation:1}, status:{observedGeneration:1},
	   spec:{template:{spec:{volumes:[
	     {name:"server-tls", secret:{secretName:.spec.tls.serverSecretRef.name}},
	     {name:"server-ca", secret:{secretName:.spec.tls.serverCASecretRef.name}},
	     {name:"client-tls", secret:{secretName:.spec.tls.clientSecretRef.name}},
	     {name:"client-ca", secret:{secretName:.spec.tls.clientCASecretRef.name}}]}}}}' "$root"
	;;
"rollout status "*) exit 0 ;;
"patch kubeneuron integration-smoke")
	[[ -n $patch ]] || { echo "fake kubectl: patch without -p" >&2; exit 2; }
	printf '%s\n' "$patch" >>"$state/patch.log"
	fail_mode=$(cat "$state/fail_mode" 2>/dev/null || true)
	if [[ -n $fail_mode ]]; then
		rm -f "$state/fail_mode"
		printf 'Error from server (Forbidden): kubeneurons.kubeneuron.io "integration-smoke" is forbidden: injected %s\n' "$fail_mode" >&2
		exit 1
	fi
	if (($(counter conflicts_remaining) > 0)); then
		decrement conflicts_remaining
		# The concurrent writer lands first and advances resourceVersion.
		jq -f "$state/on_conflict.jq" "$root" |
			jq '.metadata.resourceVersion |= ((tonumber + 1) | tostring)' >"$root.next"
		mv "$root.next" "$root"
		printf '%s\n' "$conflict_message" >&2
		exit 1
	fi
	want=$(jq -r '.metadata.resourceVersion // empty' <<<"$patch")
	# A patch without a resourceVersion is a harness failure, not a Conflict:
	# the compare-and-swap is the property under test and must never be dropped.
	[[ -n $want ]] || { echo "fake kubectl: patch carries no metadata.resourceVersion" >&2; exit 2; }
	have=$(jq -r '.metadata.resourceVersion' "$root")
	if [[ $want != "$have" ]]; then
		printf '%s\n' "$conflict_message" >&2
		exit 1
	fi
	jq --argjson patch "$patch" '
	  .spec as $before
	  | . * ($patch | del(.metadata.resourceVersion))
	  | .metadata.resourceVersion |= ((tonumber + 1) | tostring)
	  | if .spec != $before then .metadata.generation += 1 else . end
	  | .status.observedGeneration = .metadata.generation
	  | .status.conditions = [{type:"Ready", status:"True", reason:"RuntimeAvailable",
	      observedGeneration:.metadata.generation, message:"runtime available"}]' "$root" >"$root.next"
	mv "$root.next" "$root"
	printf '%s\n' "$patch" >>"$state/patch.success.log"
	cat "$root"
	;;
*)
	echo "fake kubectl: unexpected invocation: ${args[*]}" >&2
	exit 2
	;;
esac
FAKE
chmod +x "$fakebin/kubectl"

# --- harness self-check: the fake enforces what the scripts rely on ----------
# Per the house rule, make the harness fail on purpose first.
self="$work/self"
mkdir -p "$self"
cp "$fixture/root-fresh.json" "$self/root.json"
set +e
FAKE_STATE="$self" "$fakebin/kubectl" patch kubeneuron integration-smoke --type=merge \
	-p '{"metadata":{"resourceVersion":"1","annotations":{"x":"y"}}}' -o json >/dev/null 2>"$self/stale.err"
rc=$?
set -e
if [[ $rc -ne 1 ]] || ! grep -q 'the object has been modified' "$self/stale.err"; then
	bad "self-check: the fake accepted a stale resourceVersion (rc=$rc)"
fi
set +e
FAKE_STATE="$self" "$fakebin/kubectl" patch kubeneuron integration-smoke --type=merge \
	-p '{"metadata":{"annotations":{"x":"y"}}}' -o json >/dev/null 2>"$self/norv.err"
rc=$?
set -e
if [[ $rc -ne 2 ]] || ! grep -q 'carries no metadata.resourceVersion' "$self/norv.err"; then
	bad "self-check: the fake accepted a patch without a resourceVersion (rc=$rc)"
fi
set +e
FAKE_STATE="$self" "$fakebin/kubectl" patch kubeneuron integration-smoke --type=merge \
	-p '{"metadata":{"resourceVersion":"100","annotations":{"x":"y"}}}' -o json >"$self/ok.json" 2>&1
rc=$?
set -e
if [[ $rc -ne 0 ]] || [[ $($JQ_BIN -r '.metadata.resourceVersion, .metadata.generation' "$self/ok.json" | paste -sd,) != "101,3" ]]; then
	bad "self-check: the fake rejected a current resourceVersion or advanced generation on a metadata-only patch"
fi
[[ $($JQ_BIN -r '.metadata.annotations["unrelated.example/owner"]' "$self/root.json") == team-a ]] ||
	bad "self-check: the fake's merge dropped an unrelated annotation"
[[ $(wc -l <"$self/patch.log") -eq 3 && $(wc -l <"$self/patch.success.log") -eq 1 ]] ||
	bad "self-check: the fake miscounted patch attempts"

# --- scenario plumbing -------------------------------------------------------
# stage <label> <root-fixture> <conflicts> <on_conflict.jq program or ->
state=''
stage() {
	local label=$1 root=$2 conflicts=$3 program=$4
	state="$work/state.$label"
	rm -rf "$state"
	mkdir -p "$state"
	cp -r "$fixture/secrets" "$state/secrets"
	cp "$fixture/$root" "$state/root.json"
	printf '%s\n' "$conflicts" >"$state/conflicts_remaining"
	if [[ $program == - ]]; then
		printf '.\n' >"$state/on_conflict.jq"
	else
		printf '%s\n' "$program" >"$state/on_conflict.jq"
	fi
	: >"$state/patch.log"
	: >"$state/patch.success.log"
}
attempts() { wc -l <"$state/patch.log"; }
successes() { wc -l <"$state/patch.success.log"; }
root_field() { $JQ_BIN -r "$1" "$state/root.json"; }

rotate_rc=0
# run_rotate <direction> <phase> <rotation-id> [ENV=value ...]
run_rotate() {
	local direction=$1 phase=$2 rotation_id=$3
	shift 3
	local -a material
	if [[ $direction == server ]]; then
		material=(
			--from-leaf-secret "$root_name-controller-tls" --from-ca-secret "$root_name-controller-server-ca"
			--new-leaf-secret "$root_name-controller-tls-v2" --overlap-ca-secret "$root_name-controller-server-ca-overlap-v2"
			--final-ca-secret "$root_name-controller-server-ca-v2")
	else
		material=(
			--from-leaf-secret "$root_name-agent-tls" --from-ca-secret "$root_name-agent-client-ca"
			--new-leaf-secret "$root_name-agent-tls-v2" --overlap-ca-secret "$root_name-agent-client-ca-overlap-v2"
			--final-ca-secret "$root_name-agent-client-ca-v2")
	fi
	set +e
	env PATH="$fakebin:$PATH" FAKE_STATE="$state" JQ_BIN="$JQ_BIN" "$@" \
		timeout 90 bash "$rotate_script" "$direction" --phase "$phase" --root "$root_name" \
		--rotation-id "$rotation_id" "${material[@]}" --timeout-seconds 20 \
		>"$state/stdout" 2>"$state/stderr"
	rotate_rc=$?
	set -e
}
run_emergency() {
	set +e
	env PATH="$fakebin:$PATH" FAKE_STATE="$state" JQ_BIN="$JQ_BIN" "$@" \
		timeout 90 bash "$emergency_script" --root "$root_name" --recovery-id emergency-v2 \
		--server-leaf-secret "$root_name-controller-tls-emergency-v2" \
		--client-leaf-secret "$root_name-agent-tls-emergency-v2" \
		--approve-emergency-leaf-recovery --timeout-seconds 20 \
		>"$state/stdout" 2>"$state/stderr"
	rotate_rc=$?
	set -e
}
expect_rc() {
	local label=$1 want=$2
	[[ $rotate_rc -eq $want ]] || bad "$label: exit $rotate_rc, want $want: $(tr '\n' ' ' <"$state/stderr")"
}
expect_attempts() {
	local label=$1 want_attempts=$2 want_successes=$3
	[[ $(attempts) -eq $want_attempts ]] || bad "$label: $(attempts) patch attempt(s), want $want_attempts"
	[[ $(successes) -eq $want_successes ]] || bad "$label: $(successes) applied patch(es), want $want_successes"
}
expect_out() {
	local label=$1 stream=$2 needle=$3
	grep -qF -- "$needle" "$state/$stream" || bad "$label: $stream lacks '$needle': $(tr '\n' ' ' <"$state/$stream")"
}
expect_server_trust_expanded() {
	local label=$1
	[[ $(root_field '.spec.tls.serverCASecretRef.name') == "$root_name-controller-server-ca-overlap-v2" ]] ||
		bad "$label: root did not end on the overlap server CA"
	[[ $(root_field '.metadata.annotations["kubeneuron.io/tls-rotation-phase"]') == TrustExpanded ]] ||
		bad "$label: root did not record TrustExpanded"
	[[ $(root_field '.metadata.annotations["kubeneuron.io/tls-rotation-new-leaf-uid"]') == uid-new-server-leaf ]] ||
		bad "$label: root did not bind the new leaf UID"
}
expect_server_untouched() {
	local label=$1
	[[ $(root_field '.spec.tls.serverCASecretRef.name') == "$root_name-controller-server-ca" ]] ||
		bad "$label: root serverCASecretRef was written despite the refusal"
	[[ $(root_field '.metadata.annotations["kubeneuron.io/tls-rotation-phase"] // "none"') != TrustExpanded ]] ||
		bad "$label: TrustExpanded was recorded despite the refusal"
}

# --- tls-rotate.sh: expand-trust ---------------------------------------------
note "rotate: baseline without a conflict patches once"
stage baseline root-fresh.json 0 -
run_rotate server expand-trust server-routine-v2
expect_rc baseline 0
expect_attempts baseline 1 1
expect_server_trust_expanded baseline
[[ $($JQ_BIN -r '.metadata.resourceVersion' "$state/patch.log") == 100 ]] ||
	bad "baseline: the patch did not carry the validated resourceVersion 100"

note "rotate: a status-only write wins the race, the retry succeeds at the fresh resourceVersion"
stage status-race root-fresh.json 1 '.status.agents = {registered: 3, acknowledged: 3}'
run_rotate server expand-trust server-routine-v2
expect_rc status-race 0
expect_attempts status-race 2 1
expect_out status-race stdout 'patch conflicted at resourceVersion 100 (retry 1 of 5)'
expect_out status-race stdout 're-read at resourceVersion 101 with unchanged TLS references and transaction annotations; retrying'
expect_out status-race stdout 'recorded phase TrustExpanded at generation 4 after 1 conflict retry'
[[ $($JQ_BIN -r '.metadata.resourceVersion' "$state/patch.log" | paste -sd,) == "100,101" ]] ||
	bad "status-race: patch attempts did not carry resourceVersions 100 then 101: $($JQ_BIN -r '.metadata.resourceVersion' "$state/patch.log" | paste -sd,)"
expect_server_trust_expanded status-race
[[ $(root_field '.status.agents.registered') == 3 ]] || bad "status-race: the retry clobbered the concurrent status write"

note "rotate: an unrelated label/annotation write is retried through and preserved"
stage unrelated-metadata root-fresh.json 1 '.metadata.labels.team = "b" | .metadata.annotations["unrelated.example/owner"] = "team-b"'
run_rotate server expand-trust server-routine-v2
expect_rc unrelated-metadata 0
expect_attempts unrelated-metadata 2 1
expect_server_trust_expanded unrelated-metadata
[[ $(root_field '.metadata.labels.team') == b && $(root_field '.metadata.annotations["unrelated.example/owner"]') == team-b ]] ||
	bad "unrelated-metadata: the retry overwrote an unrelated concurrent metadata change"

note "rotate: two conflicts in a row are both retried"
stage two-conflicts root-fresh.json 2 '.status.heartbeat = (.status.heartbeat // 0) + 1'
run_rotate server expand-trust server-routine-v2
expect_rc two-conflicts 0
expect_attempts two-conflicts 3 1
expect_out two-conflicts stdout 'retry 2 of 5'
expect_out two-conflicts stdout 'after 2 conflict retry'

note "rotate: a forward phase waits for the re-read root to be Ready before retrying"
stage unready-retry root-fresh.json 1 '.status.heartbeat = 1'
printf '1\n' >"$state/unready_gets"
run_rotate server expand-trust server-routine-v2
expect_rc unready-retry 0
expect_attempts unready-retry 2 1
[[ $(cat "$state/unready_gets") -eq 0 ]] || bad "unready-retry: the script never re-read the root after the conflict"
expect_server_trust_expanded unready-retry

note "rotate: the retry bound is enforced and reported"
stage exhausted root-fresh.json 3 '.status.heartbeat = (.status.heartbeat // 0) + 1'
run_rotate server expand-trust server-routine-v2 PATCH_CONFLICT_RETRIES=2
expect_rc exhausted 1
expect_attempts exhausted 3 0
expect_out exhausted stderr 'still conflicted after 2 retry(ies)'
expect_out exhausted stderr 'the object has been modified'
expect_server_untouched exhausted

note "rotate: zero retries means the first conflict is fatal"
stage no-retries root-fresh.json 1 '.status.heartbeat = 1'
run_rotate server expand-trust server-routine-v2 PATCH_CONFLICT_RETRIES=0
expect_rc no-retries 1
expect_attempts no-retries 1 0
expect_out no-retries stderr 'still conflicted after 0 retry(ies)'

note "rotate: a non-conflict error is fatal without a retry"
stage forbidden root-fresh.json 0 -
printf 'rbac-probe\n' >"$state/fail_mode"
run_rotate server expand-trust server-routine-v2
expect_rc forbidden 1
expect_attempts forbidden 1 0
expect_out forbidden stderr 'Forbidden'
expect_out forbidden stderr 'failed with a non-conflict error; nothing was retried'
expect_server_untouched forbidden

note "rotate: a concurrent identical invocation that already recorded the phase is accepted without a second write"
stage already-applied root-fresh.json 1 "$(printf '.metadata.annotations += %s | .spec.tls.serverCASecretRef = {name: "%s"} | .metadata.generation += 1 | .status.observedGeneration = .metadata.generation | .status.conditions[0].observedGeneration = .metadata.generation' "$server_expand_annotations" "$root_name-controller-server-ca-overlap-v2")"
run_rotate server expand-trust server-routine-v2
expect_rc already-applied 0
expect_attempts already-applied 1 0
expect_out already-applied stdout 'already recorded exactly as planned at generation 4'
expect_out already-applied stdout 'is rolled out and KubeNeuron integration-smoke is Ready'
expect_server_trust_expanded already-applied

note "rotate: a lookalike plan bound to a different new-leaf UID fails closed"
stage uid-lookalike root-fresh.json 1 "$(printf '.metadata.annotations += (%s | .["kubeneuron.io/tls-rotation-new-leaf-uid"] = "uid-impostor-leaf") | .spec.tls.serverCASecretRef = {name: "%s"} | .metadata.generation += 1 | .status.observedGeneration = .metadata.generation | .status.conditions[0].observedGeneration = .metadata.generation' "$server_expand_annotations" "$root_name-controller-server-ca-overlap-v2")"
run_rotate server expand-trust server-routine-v2
expect_rc uid-lookalike 1
expect_attempts uid-lookalike 1 0
expect_out uid-lookalike stderr 'refusing to retry phase TrustExpanded'
expect_out uid-lookalike stderr 'annotation kubeneuron.io/tls-rotation-new-leaf-uid: validated null, now "uid-impostor-leaf"'
[[ $(root_field '.metadata.annotations["kubeneuron.io/tls-rotation-new-leaf-uid"]') == uid-impostor-leaf ]] ||
	bad "uid-lookalike: the refusal still wrote over the foreign plan"

note "rotate: a foreign transaction that claimed the root during the race fails closed"
stage foreign-transaction root-fresh.json 1 '.metadata.annotations += {"kubeneuron.io/tls-rotation-id":"someone-else","kubeneuron.io/tls-rotation-direction":"client","kubeneuron.io/tls-rotation-phase":"TrustExpanded"}'
run_rotate server expand-trust server-routine-v2
expect_rc foreign-transaction 1
expect_attempts foreign-transaction 1 0
expect_out foreign-transaction stderr 'KubeNeuron integration-smoke changed after validation:'
expect_out foreign-transaction stderr 'annotation kubeneuron.io/tls-rotation-id: validated null, now "someone-else"'
expect_out foreign-transaction stderr 'nothing was written, re-run this phase'
[[ $(root_field '.metadata.annotations["kubeneuron.io/tls-rotation-id"]') == someone-else ]] ||
	bad "foreign-transaction: the refusal overwrote the other transaction"
[[ $(root_field '.spec.tls.serverCASecretRef.name') == "$root_name-controller-server-ca" ]] ||
	bad "foreign-transaction: root serverCASecretRef was written despite the refusal"

note "rotate: a concurrent spec change (new generation) fails closed"
stage spec-change root-fresh.json 1 '.spec.replicas = 2 | .metadata.generation += 1'
run_rotate server expand-trust server-routine-v2
expect_rc spec-change 1
expect_attempts spec-change 1 0
expect_out spec-change stderr 'metadata.generation (spec changed): validated 3, now 4'
expect_server_untouched spec-change

note "rotate: a concurrent change to the other direction's TLS reference fails closed"
stage other-direction-ref root-fresh.json 1 '.spec.tls.clientSecretRef = {name: "integration-smoke-agent-tls-v2"} | .metadata.generation += 1'
run_rotate server expand-trust server-routine-v2
expect_rc other-direction-ref 1
expect_attempts other-direction-ref 1 0
expect_out other-direction-ref stderr 'spec.tls.clientSecretRef: validated {"key":"ca.crt","name":"integration-smoke-agent-tls"}, now {"key":"ca.crt","name":"integration-smoke-agent-tls-v2"}'
expect_server_untouched other-direction-ref

# --- tls-rotate.sh: a rollback phase does not wait for Ready -----------------
note "rotate: rollback-leaf on an unready root retries a status-race conflict without waiting for Ready"
stage rollback-race root-client-leaf-activated.json 1 '.status.conditions[0].message = "still failing"'
run_rotate client rollback-leaf client-routine-v2
expect_rc rollback-race 0
expect_attempts rollback-race 2 1
expect_out rollback-race stdout 'recorded phase LeafRolledBack at generation 4 after 1 conflict retry'
[[ $(root_field '.spec.tls.clientSecretRef.name') == "$root_name-agent-tls" ]] ||
	bad "rollback-race: root did not return to the old client leaf"
[[ $(root_field '.metadata.annotations["kubeneuron.io/tls-rotation-phase"]') == LeafRolledBack ]] ||
	bad "rollback-race: root did not record LeafRolledBack"

note "rotate: rollback-leaf refuses when the active transaction changed hands during the race"
stage rollback-foreign root-client-leaf-activated.json 1 '.metadata.annotations["kubeneuron.io/tls-rotation-id"] = "client-routine-v3"'
run_rotate client rollback-leaf client-routine-v2
expect_rc rollback-foreign 1
expect_attempts rollback-foreign 1 0
expect_out rollback-foreign stderr 'annotation kubeneuron.io/tls-rotation-id: validated "client-routine-v2", now "client-routine-v3"'
[[ $(root_field '.spec.tls.clientSecretRef.name') == "$root_name-agent-tls-v2" ]] ||
	bad "rollback-foreign: the refusal still rolled the leaf back"

# --- tls-emergency-recover.sh ------------------------------------------------
note "emergency: a Ready root is refused before any patch"
stage emergency-ready-root root-fresh.json 0 -
run_emergency
expect_rc emergency-ready-root 1
expect_attempts emergency-ready-root 0 0
expect_out emergency-ready-root stderr 'is currently Ready; routine rotation is required instead of emergency recovery'

note "emergency: baseline without a conflict patches once"
stage emergency-baseline root-unready.json 0 -
run_emergency
expect_rc emergency-baseline 0
expect_attempts emergency-baseline 1 1
[[ $(root_field '.spec.tls.serverSecretRef.name') == "$root_name-controller-tls-emergency-v2" ]] ||
	bad "emergency-baseline: server leaf was not replaced"
[[ $(root_field '.metadata.annotations["kubeneuron.io/tls-emergency-recovery-previous-client-leaf"]') == "$root_name-agent-tls" ]] ||
	bad "emergency-baseline: previous client leaf was not recorded"

note "emergency: a status-only write wins the race, the retry succeeds"
stage emergency-status-race root-unready.json 1 '.status.conditions[0].message = "leaf expired (rechecked)"'
run_emergency
expect_rc emergency-status-race 0
expect_attempts emergency-status-race 2 1
expect_out emergency-status-race stdout 'emergency patch conflicted at resourceVersion 100 (retry 1 of 5)'
expect_out emergency-status-race stdout 'still unready with unchanged TLS references and transaction annotations; retrying'
expect_out emergency-status-race stdout 'recorded at root generation 4 after 1 conflict retry'
[[ $($JQ_BIN -r '.metadata.resourceVersion' "$state/patch.log" | paste -sd,) == "100,101" ]] ||
	bad "emergency-status-race: patch attempts did not carry resourceVersions 100 then 101"
[[ $(root_field '.spec.tls.clientSecretRef.name') == "$root_name-agent-tls-emergency-v2" ]] ||
	bad "emergency-status-race: client leaf was not replaced"

note "emergency: a root that became Ready during the race is refused"
stage emergency-became-ready root-unready.json 1 '.status.conditions[0] |= (.status = "True" | .reason = "RuntimeAvailable")'
run_emergency
expect_rc emergency-became-ready 1
expect_attempts emergency-became-ready 1 0
expect_out emergency-became-ready stderr 'became Ready at the validated generation; emergency recovery no longer applies'
[[ $(root_field '.spec.tls.serverSecretRef.name') == "$root_name-controller-tls" ]] ||
	bad "emergency-became-ready: the refusal still replaced the server leaf"

note "emergency: a routine rotation that started during the race is refused"
stage emergency-routine-started root-unready.json 1 '.metadata.annotations += {"kubeneuron.io/tls-rotation-id":"server-routine-v3","kubeneuron.io/tls-rotation-direction":"server","kubeneuron.io/tls-rotation-phase":"TrustExpanded"}'
run_emergency
expect_rc emergency-routine-started 1
expect_attempts emergency-routine-started 1 0
expect_out emergency-routine-started stderr 'annotation kubeneuron.io/tls-rotation-id: validated null, now "server-routine-v3"'
expect_out emergency-routine-started stderr 'nothing was written, rerun so the current root is validated'

note "emergency: a different recovery id that won the race is refused, not adopted"
stage emergency-other-id root-unready.json 1 "$(printf '.metadata.annotations += (%s | .["kubeneuron.io/tls-emergency-recovery-id"] = "emergency-v3") | .spec.tls.serverSecretRef = {name: "%s"} | .spec.tls.clientSecretRef = {name: "%s"} | .metadata.generation += 1' "$emergency_annotations" "$root_name-controller-tls-emergency-v2" "$root_name-agent-tls-emergency-v2")"
run_emergency
expect_rc emergency-other-id 1
expect_attempts emergency-other-id 1 0
expect_out emergency-other-id stderr 'annotation kubeneuron.io/tls-emergency-recovery-id: validated null, now "emergency-v3"'

note "emergency: a concurrent identical invocation that already recorded the recovery is accepted without a second write"
stage emergency-already-applied root-unready.json 1 "$(printf '.metadata.annotations += %s | .spec.tls.serverSecretRef = {name: "%s"} | .spec.tls.clientSecretRef = {name: "%s"} | .metadata.generation += 1 | .status.observedGeneration = .metadata.generation | .status.conditions[0] |= (.status = "True" | .reason = "RuntimeAvailable" | .observedGeneration = 4)' "$emergency_annotations" "$root_name-controller-tls-emergency-v2" "$root_name-agent-tls-emergency-v2")"
run_emergency
expect_rc emergency-already-applied 0
expect_attempts emergency-already-applied 1 0
expect_out emergency-already-applied stdout 'already recorded exactly as planned at root generation 4'
expect_out emergency-already-applied stdout 'KubeNeuron integration-smoke is Ready'

note "emergency: the retry bound is enforced and reported"
stage emergency-exhausted root-unready.json 2 '.status.heartbeat = (.status.heartbeat // 0) + 1'
run_emergency PATCH_CONFLICT_RETRIES=1
expect_rc emergency-exhausted 1
expect_attempts emergency-exhausted 2 0
expect_out emergency-exhausted stderr 'still conflicted after 1 retry(ies)'

if ((fail)); then
	echo "verify-tls-scripts: FAILED" >&2
	exit 1
fi
note "OK: both TLS helpers retry only status/unrelated-metadata conflicts, accept exact already-applied state, and fail closed on every TLS-relevant drift"
