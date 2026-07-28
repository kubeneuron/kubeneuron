#!/usr/bin/env bash
# Performs one explicit phase of a routine KubeNeuron TLS rotation.
# Candidate Secrets are external, immutable, and versioned; this script never
# creates, mutates, prints, or deletes Secret data.
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq variables.
set -euo pipefail

KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
JQ_BIN=${JQ_BIN:-jq}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-240}
KUBECONFIG_PATH=${KUBECONFIG_PATH:-${KUBECONFIG:-}}

direction=
phase=
root_name=
rotation_id=
from_leaf_secret=
from_ca_secret=
from_ca_key=ca.crt
new_leaf_secret=
overlap_ca_secret=
overlap_ca_key=ca.crt
final_ca_secret=
final_ca_key=ca.crt
approve_retire=0
target_root_generation=

readonly annotation_id='kubeneuron.io/tls-rotation-id'
readonly annotation_direction='kubeneuron.io/tls-rotation-direction'
readonly annotation_phase='kubeneuron.io/tls-rotation-phase'
readonly annotation_from_leaf='kubeneuron.io/tls-rotation-from-leaf'
readonly annotation_from_leaf_uid='kubeneuron.io/tls-rotation-from-leaf-uid'
readonly annotation_from_ca='kubeneuron.io/tls-rotation-from-ca'
readonly annotation_from_ca_key='kubeneuron.io/tls-rotation-from-ca-key'
readonly annotation_from_ca_uid='kubeneuron.io/tls-rotation-from-ca-uid'
readonly annotation_new_leaf='kubeneuron.io/tls-rotation-new-leaf'
readonly annotation_new_leaf_uid='kubeneuron.io/tls-rotation-new-leaf-uid'
readonly annotation_overlap_ca='kubeneuron.io/tls-rotation-overlap-ca'
readonly annotation_overlap_ca_key='kubeneuron.io/tls-rotation-overlap-ca-key'
readonly annotation_overlap_ca_uid='kubeneuron.io/tls-rotation-overlap-ca-uid'
readonly annotation_final_ca='kubeneuron.io/tls-rotation-final-ca'
readonly annotation_final_ca_key='kubeneuron.io/tls-rotation-final-ca-key'
readonly annotation_final_ca_uid='kubeneuron.io/tls-rotation-final-ca-uid'

usage() {
	cat <<'EOF'
Usage:
  tls-rotate.sh <server|client> --phase <phase> --root NAME --rotation-id ID \
    --from-leaf-secret NAME --from-ca-secret NAME \
    --new-leaf-secret NAME --overlap-ca-secret NAME --final-ca-secret NAME \
    [--from-ca-key KEY] [--overlap-ca-key KEY] [--final-ca-key KEY] \
    [--approve-retire-old-trust] [--kubeconfig PATH] [--timeout-seconds N]

Routine phases (one invocation per phase):
  expand-trust      Roll the consumer onto an old+new CA bundle.
  activate-leaf     Roll the producer onto the new key pair.
  retire-old-trust  Roll the consumer onto the new-only CA bundle. Requires
                    --approve-retire-old-trust.
  rollback-retirement
                    After a failed trust contraction, restore overlap trust
                    while the new leaf remains active.
  rollback-leaf     Before trust retirement, restore the old key pair.
  rollback-trust    After the old leaf is active, restore the old CA bundle.

Server rotation maps CA changes to agents and leaf changes to the controller.
Client rotation maps CA changes to the controller and leaf changes to agents.
All three candidate Secrets must have unique names, immutable: true, and no
ownerReferences. The script never deletes old or candidate Secrets.
EOF
}

die() {
	printf 'tls-rotate: %s\n' "$*" >&2
	exit 1
}

note() {
	printf 'tls-rotate: %s\n' "$*"
}

require_value() {
	local option=$1
	local value=${2:-}
	[[ -n $value ]] || die "$option requires a value"
}

while (($#)); do
	case $1 in
	server | client)
		[[ -z $direction ]] || die "direction was provided more than once"
		direction=$1
		shift
		;;
	--phase)
		require_value "$1" "${2:-}"
		phase=$2
		shift 2
		;;
	--root)
		require_value "$1" "${2:-}"
		root_name=$2
		shift 2
		;;
	--rotation-id)
		require_value "$1" "${2:-}"
		rotation_id=$2
		shift 2
		;;
	--from-leaf-secret)
		require_value "$1" "${2:-}"
		from_leaf_secret=$2
		shift 2
		;;
	--from-ca-secret)
		require_value "$1" "${2:-}"
		from_ca_secret=$2
		shift 2
		;;
	--from-ca-key)
		require_value "$1" "${2:-}"
		from_ca_key=$2
		shift 2
		;;
	--new-leaf-secret)
		require_value "$1" "${2:-}"
		new_leaf_secret=$2
		shift 2
		;;
	--overlap-ca-secret)
		require_value "$1" "${2:-}"
		overlap_ca_secret=$2
		shift 2
		;;
	--overlap-ca-key)
		require_value "$1" "${2:-}"
		overlap_ca_key=$2
		shift 2
		;;
	--final-ca-secret)
		require_value "$1" "${2:-}"
		final_ca_secret=$2
		shift 2
		;;
	--final-ca-key)
		require_value "$1" "${2:-}"
		final_ca_key=$2
		shift 2
		;;
	--approve-retire-old-trust)
		approve_retire=1
		shift
		;;
	--kubeconfig)
		require_value "$1" "${2:-}"
		KUBECONFIG_PATH=$2
		shift 2
		;;
	--timeout-seconds)
		require_value "$1" "${2:-}"
		TIMEOUT_SECONDS=$2
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die "unknown argument: $1"
		;;
	esac
done

[[ $direction == server || $direction == client ]] || {
	usage >&2
	die "direction must be server or client"
}
case $phase in
expand-trust | activate-leaf | retire-old-trust | rollback-retirement | rollback-leaf | rollback-trust) ;;
*) die "unsupported phase: ${phase:-empty}" ;;
esac
for required in root_name rotation_id from_leaf_secret from_ca_secret new_leaf_secret overlap_ca_secret final_ca_secret; do
	[[ -n ${!required} ]] || die "missing required option for ${required//_/-}"
done
[[ $TIMEOUT_SECONDS =~ ^[1-9][0-9]*$ ]] || die "timeout-seconds must be a positive integer"
[[ $rotation_id =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$ ]] || die "rotation-id is not a safe identifier"
for value in "$root_name" "$from_leaf_secret" "$from_ca_secret" "$new_leaf_secret" "$overlap_ca_secret" "$final_ca_secret"; do
	[[ $value =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || die "unsafe Kubernetes name: $value"
done
for value in "$from_ca_key" "$overlap_ca_key" "$final_ca_key"; do
	[[ $value =~ ^[-._a-zA-Z0-9]+$ ]] || die "unsafe Secret key: $value"
done
declare -A secret_name_roles=()
for role_and_name in \
	"old leaf=$from_leaf_secret" \
	"old CA=$from_ca_secret" \
	"new leaf=$new_leaf_secret" \
	"overlap CA=$overlap_ca_secret" \
	"final CA=$final_ca_secret"; do
	role=${role_and_name%%=*}
	name=${role_and_name#*=}
	[[ -z ${secret_name_roles[$name]:-} ]] || \
		die "$role and ${secret_name_roles[$name]} must use distinct Secret names"
	secret_name_roles[$name]=$role
done

for command in "$KUBECTL_BIN" "$JQ_BIN" sed; do
	command -v "$command" >/dev/null 2>&1 || die "required command not found: $command"
done

kubectl_command=("$KUBECTL_BIN")
if [[ -n $KUBECONFIG_PATH ]]; then
	[[ -r $KUBECONFIG_PATH ]] || die "kubeconfig is not readable: $KUBECONFIG_PATH"
	kubectl_command+=(--kubeconfig "$KUBECONFIG_PATH")
fi

kubectl_run() {
	"${kubectl_command[@]}" "$@"
}

root_json=$(kubectl_run get kubeneuron "$root_name" -o json)
namespace=$($JQ_BIN -r '.spec.namespace // empty' <<<"$root_json")
[[ -n $namespace ]] || die "KubeNeuron $root_name has no spec.namespace"

if [[ $direction == server ]]; then
	leaf_field=serverSecretRef
	ca_field=serverCASecretRef
	consumer_kind=daemonset
	consumer_name="${root_name}-agent"
	producer_kind=deployment
	producer_name="${root_name}-controller"
else
	leaf_field=clientSecretRef
	ca_field=clientCASecretRef
	consumer_kind=deployment
	consumer_name="${root_name}-controller"
	producer_kind=daemonset
	producer_name="${root_name}-agent"
fi

canonical_ca_key() {
	local key=$1
	if [[ -z $key ]]; then
		printf 'ca.crt'
	else
		printf '%s' "$key"
	fi
}

current_leaf=$($JQ_BIN -r --arg field "$leaf_field" '.spec.tls[$field].name // empty' <<<"$root_json")
current_ca=$($JQ_BIN -r --arg field "$ca_field" '.spec.tls[$field].name // empty' <<<"$root_json")
current_ca_key=$($JQ_BIN -r --arg field "$ca_field" '.spec.tls[$field].key // "ca.crt"' <<<"$root_json")
current_ca_key=$(canonical_ca_key "$current_ca_key")
active_id=$($JQ_BIN -r --arg key "$annotation_id" '.metadata.annotations[$key] // empty' <<<"$root_json")
active_direction=$($JQ_BIN -r --arg key "$annotation_direction" '.metadata.annotations[$key] // empty' <<<"$root_json")
active_phase=$($JQ_BIN -r --arg key "$annotation_phase" '.metadata.annotations[$key] // empty' <<<"$root_json")

read_annotation() {
	local key=$1
	$JQ_BIN -r --arg key "$key" '.metadata.annotations[$key] // empty' <<<"$root_json"
}

recorded_from_leaf=$(read_annotation "$annotation_from_leaf")
recorded_from_leaf_uid=$(read_annotation "$annotation_from_leaf_uid")
recorded_from_ca=$(read_annotation "$annotation_from_ca")
recorded_from_ca_key=$(read_annotation "$annotation_from_ca_key")
recorded_from_ca_uid=$(read_annotation "$annotation_from_ca_uid")
recorded_new_leaf=$(read_annotation "$annotation_new_leaf")
recorded_new_leaf_uid=$(read_annotation "$annotation_new_leaf_uid")
recorded_overlap_ca=$(read_annotation "$annotation_overlap_ca")
recorded_overlap_ca_key=$(read_annotation "$annotation_overlap_ca_key")
recorded_overlap_ca_uid=$(read_annotation "$annotation_overlap_ca_uid")
recorded_final_ca=$(read_annotation "$annotation_final_ca")
recorded_final_ca_key=$(read_annotation "$annotation_final_ca_key")
recorded_final_ca_uid=$(read_annotation "$annotation_final_ca_uid")

secret_json() {
	local name=$1
	kubectl_run -n "$namespace" get secret "$name" -o json
}

require_key_pair_secret() {
	local name=$1
	local immutable=$2
	local json
	json=$(secret_json "$name") || die "Secret $namespace/$name does not exist"
	$JQ_BIN -e '
      .type == "kubernetes.io/tls" and
      .data["tls.crt"] != null and .data["tls.crt"] != "" and
      .data["tls.key"] != null and .data["tls.key"] != "" and
      ((.metadata.ownerReferences // []) | length) == 0
    ' <<<"$json" >/dev/null || die "Secret $namespace/$name is not an unowned TLS key-pair Secret"
	if [[ $immutable == true ]]; then
		$JQ_BIN -e '.immutable == true' <<<"$json" >/dev/null || \
			die "candidate Secret $namespace/$name must set immutable: true"
	fi
	$JQ_BIN -r '.metadata.uid' <<<"$json"
}

require_ca_secret() {
	local name=$1
	local key=$2
	local immutable=$3
	local json
	json=$(secret_json "$name") || die "Secret $namespace/$name does not exist"
	$JQ_BIN -e --arg key "$key" '
      .data[$key] != null and .data[$key] != "" and
      ((.metadata.ownerReferences // []) | length) == 0
    ' <<<"$json" >/dev/null || die "Secret $namespace/$name lacks unowned CA bundle key $key"
	if [[ $immutable == true ]]; then
		$JQ_BIN -e '.immutable == true' <<<"$json" >/dev/null || \
			die "candidate Secret $namespace/$name must set immutable: true"
	fi
	$JQ_BIN -r '.metadata.uid' <<<"$json"
}

require_uid() {
	local label=$1
	local actual=$2
	local expected=$3
	[[ -n $expected ]] || die "active transaction has no recorded UID for $label"
	[[ $actual == "$expected" ]] || \
		die "$label Secret UID changed from $expected to $actual; versioned Secret names may not be reused"
}

require_bound_plan() {
	[[ $recorded_from_leaf == "$from_leaf_secret" ]] || die "rotation plan old leaf is ${recorded_from_leaf:-missing}, want $from_leaf_secret"
	[[ $recorded_from_ca == "$from_ca_secret" ]] || die "rotation plan old CA is ${recorded_from_ca:-missing}, want $from_ca_secret"
	[[ $recorded_from_ca_key == "$from_ca_key" ]] || die "rotation plan old CA key is ${recorded_from_ca_key:-missing}, want $from_ca_key"
	[[ $recorded_new_leaf == "$new_leaf_secret" ]] || die "rotation plan new leaf is ${recorded_new_leaf:-missing}, want $new_leaf_secret"
	[[ $recorded_overlap_ca == "$overlap_ca_secret" ]] || die "rotation plan overlap CA is ${recorded_overlap_ca:-missing}, want $overlap_ca_secret"
	[[ $recorded_overlap_ca_key == "$overlap_ca_key" ]] || die "rotation plan overlap CA key is ${recorded_overlap_ca_key:-missing}, want $overlap_ca_key"
	[[ $recorded_final_ca == "$final_ca_secret" ]] || die "rotation plan final CA is ${recorded_final_ca:-missing}, want $final_ca_secret"
	[[ $recorded_final_ca_key == "$final_ca_key" ]] || die "rotation plan final CA key is ${recorded_final_ca_key:-missing}, want $final_ca_key"
	for uid in \
		"$recorded_from_leaf_uid" "$recorded_from_ca_uid" "$recorded_new_leaf_uid" \
		"$recorded_overlap_ca_uid" "$recorded_final_ca_uid"; do
		[[ -n $uid ]] || die "active transaction is missing its Secret UID binding"
	done
}

load_initial_plan() {
	recorded_from_leaf_uid=$(require_key_pair_secret "$from_leaf_secret" false)
	recorded_from_ca_uid=$(require_ca_secret "$from_ca_secret" "$from_ca_key" false)
	recorded_new_leaf_uid=$(require_key_pair_secret "$new_leaf_secret" true)
	recorded_overlap_ca_uid=$(require_ca_secret "$overlap_ca_secret" "$overlap_ca_key" true)
	recorded_final_ca_uid=$(require_ca_secret "$final_ca_secret" "$final_ca_key" true)
	recorded_from_leaf=$from_leaf_secret
	recorded_from_ca=$from_ca_secret
	recorded_from_ca_key=$from_ca_key
	recorded_new_leaf=$new_leaf_secret
	recorded_overlap_ca=$overlap_ca_secret
	recorded_overlap_ca_key=$overlap_ca_key
	recorded_final_ca=$final_ca_secret
	recorded_final_ca_key=$final_ca_key
}

root_ready() {
	local json=${1:-$(kubectl_run get kubeneuron "$root_name" -o json)}
	$JQ_BIN -e '
      .metadata.generation as $generation |
      .status.observedGeneration == $generation and
      any(.status.conditions[]?;
        .type == "Ready" and .status == "True" and .reason == "RuntimeAvailable" and
        .observedGeneration == $generation)
    ' <<<"$json" >/dev/null
}

report_root_status() {
	local json
	json=$(kubectl_run get kubeneuron "$root_name" -o json 2>/dev/null || true)
	[[ -n $json ]] || return 0
	$JQ_BIN -r '
      "generation=\(.metadata.generation) observed=\(.status.observedGeneration // 0)",
      (.status.conditions[]? | select(.type == "Ready") |
        "Ready=\(.status)/\(.reason): \(.message)")
    ' <<<"$json" >&2
}

require_stable_root() {
	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		root_json=$(kubectl_run get kubeneuron "$root_name" -o json)
		if root_ready "$root_json"; then
			return
		fi
		sleep 2
	done
	report_root_status
	die "refusing a forward phase while KubeNeuron $root_name did not become Ready before timeout"
}

require_transaction() {
	local expected_phase=$1
	[[ $active_id == "$rotation_id" ]] || die "active rotation id is ${active_id:-none}, want $rotation_id"
	[[ $active_direction == "$direction" ]] || die "active rotation direction is ${active_direction:-none}, want $direction"
	[[ $active_phase == "$expected_phase" ]] || \
		die "active rotation phase is ${active_phase:-none}, want $expected_phase"
	require_bound_plan
}

ca_ref_json() {
	local name=$1
	local key=$2
	if [[ $key == ca.crt ]]; then
		$JQ_BIN -cn --arg name "$name" '{name:$name}'
	else
		$JQ_BIN -cn --arg name "$name" --arg key "$key" '{name:$name,key:$key}'
	fi
}

apply_phase() {
	local field=$1
	local ref_json=$2
	local recorded_phase=$3
	local resource_version patch generation patched_json
	resource_version=$($JQ_BIN -r '.metadata.resourceVersion' <<<"$root_json")
	patch=$($JQ_BIN -cn \
		--arg rv "$resource_version" \
		--arg id_key "$annotation_id" --arg id "$rotation_id" \
		--arg direction_key "$annotation_direction" --arg direction "$direction" \
		--arg phase_key "$annotation_phase" --arg phase "$recorded_phase" \
		--arg from_leaf_key "$annotation_from_leaf" --arg from_leaf "$recorded_from_leaf" \
		--arg from_leaf_uid_key "$annotation_from_leaf_uid" --arg from_leaf_uid "$recorded_from_leaf_uid" \
		--arg from_ca_key_name "$annotation_from_ca" --arg from_ca "$recorded_from_ca" \
		--arg from_ca_key_key "$annotation_from_ca_key" --arg from_ca_key "$recorded_from_ca_key" \
		--arg from_ca_uid_key "$annotation_from_ca_uid" --arg from_ca_uid "$recorded_from_ca_uid" \
		--arg new_leaf_key "$annotation_new_leaf" --arg new_leaf "$recorded_new_leaf" \
		--arg new_leaf_uid_key "$annotation_new_leaf_uid" --arg new_leaf_uid "$recorded_new_leaf_uid" \
		--arg overlap_ca_key_name "$annotation_overlap_ca" --arg overlap_ca "$recorded_overlap_ca" \
		--arg overlap_ca_key_key "$annotation_overlap_ca_key" --arg overlap_ca_key "$recorded_overlap_ca_key" \
		--arg overlap_ca_uid_key "$annotation_overlap_ca_uid" --arg overlap_ca_uid "$recorded_overlap_ca_uid" \
		--arg final_ca_key_name "$annotation_final_ca" --arg final_ca "$recorded_final_ca" \
		--arg final_ca_key_key "$annotation_final_ca_key" --arg final_ca_key "$recorded_final_ca_key" \
		--arg final_ca_uid_key "$annotation_final_ca_uid" --arg final_ca_uid "$recorded_final_ca_uid" \
		--arg field "$field" --argjson ref "$ref_json" '
      {
        metadata: {
          resourceVersion: $rv,
          annotations: {
	            ($id_key): $id,
	            ($direction_key): $direction,
	            ($phase_key): $phase,
	            ($from_leaf_key): $from_leaf,
	            ($from_leaf_uid_key): $from_leaf_uid,
	            ($from_ca_key_name): $from_ca,
	            ($from_ca_key_key): $from_ca_key,
	            ($from_ca_uid_key): $from_ca_uid,
	            ($new_leaf_key): $new_leaf,
	            ($new_leaf_uid_key): $new_leaf_uid,
	            ($overlap_ca_key_name): $overlap_ca,
	            ($overlap_ca_key_key): $overlap_ca_key,
	            ($overlap_ca_uid_key): $overlap_ca_uid,
	            ($final_ca_key_name): $final_ca,
	            ($final_ca_key_key): $final_ca_key,
	            ($final_ca_uid_key): $final_ca_uid
          }
        },
        spec: {tls: {($field): $ref}}
      }
    ')
	patched_json=$(kubectl_run patch kubeneuron "$root_name" --type=merge -p "$patch" -o json)
	generation=$($JQ_BIN -r '.metadata.generation' <<<"$patched_json")
	[[ $generation =~ ^[1-9][0-9]*$ ]] || die "phase patch returned an invalid root generation"
	target_root_generation=$generation
	note "$direction rotation $rotation_id recorded phase $recorded_phase at generation $generation"
}

root_has_bound_plan() {
	local json=$1
	$JQ_BIN -e \
		--arg from_leaf_key "$annotation_from_leaf" --arg from_leaf "$recorded_from_leaf" \
		--arg from_leaf_uid_key "$annotation_from_leaf_uid" --arg from_leaf_uid "$recorded_from_leaf_uid" \
		--arg from_ca_key_name "$annotation_from_ca" --arg from_ca "$recorded_from_ca" \
		--arg from_ca_key_key "$annotation_from_ca_key" --arg from_ca_key "$recorded_from_ca_key" \
		--arg from_ca_uid_key "$annotation_from_ca_uid" --arg from_ca_uid "$recorded_from_ca_uid" \
		--arg new_leaf_key "$annotation_new_leaf" --arg new_leaf "$recorded_new_leaf" \
		--arg new_leaf_uid_key "$annotation_new_leaf_uid" --arg new_leaf_uid "$recorded_new_leaf_uid" \
		--arg overlap_ca_key_name "$annotation_overlap_ca" --arg overlap_ca "$recorded_overlap_ca" \
		--arg overlap_ca_key_key "$annotation_overlap_ca_key" --arg overlap_ca_key "$recorded_overlap_ca_key" \
		--arg overlap_ca_uid_key "$annotation_overlap_ca_uid" --arg overlap_ca_uid "$recorded_overlap_ca_uid" \
		--arg final_ca_key_name "$annotation_final_ca" --arg final_ca "$recorded_final_ca" \
		--arg final_ca_key_key "$annotation_final_ca_key" --arg final_ca_key "$recorded_final_ca_key" \
		--arg final_ca_uid_key "$annotation_final_ca_uid" --arg final_ca_uid "$recorded_final_ca_uid" '
	  .metadata.annotations[$from_leaf_key] == $from_leaf and
	  .metadata.annotations[$from_leaf_uid_key] == $from_leaf_uid and
	  .metadata.annotations[$from_ca_key_name] == $from_ca and
	  .metadata.annotations[$from_ca_key_key] == $from_ca_key and
	  .metadata.annotations[$from_ca_uid_key] == $from_ca_uid and
	  .metadata.annotations[$new_leaf_key] == $new_leaf and
	  .metadata.annotations[$new_leaf_uid_key] == $new_leaf_uid and
	  .metadata.annotations[$overlap_ca_key_name] == $overlap_ca and
	  .metadata.annotations[$overlap_ca_key_key] == $overlap_ca_key and
	  .metadata.annotations[$overlap_ca_uid_key] == $overlap_ca_uid and
	  .metadata.annotations[$final_ca_key_name] == $final_ca and
	  .metadata.annotations[$final_ca_key_key] == $final_ca_key and
	  .metadata.annotations[$final_ca_uid_key] == $final_ca_uid
	' <<<"$json" >/dev/null
}

root_has_phase_refs() {
	local json=$1
	local recorded_phase=$2
	local expected_leaf expected_ca expected_ca_key
	case $recorded_phase in
	TrustExpanded)
		expected_leaf=$from_leaf_secret
		expected_ca=$overlap_ca_secret
		expected_ca_key=$overlap_ca_key
		;;
	LeafActivated)
		expected_leaf=$new_leaf_secret
		expected_ca=$overlap_ca_secret
		expected_ca_key=$overlap_ca_key
		;;
	OldTrustRetired)
		expected_leaf=$new_leaf_secret
		expected_ca=$final_ca_secret
		expected_ca_key=$final_ca_key
		;;
	LeafRolledBack)
		expected_leaf=$from_leaf_secret
		expected_ca=$overlap_ca_secret
		expected_ca_key=$overlap_ca_key
		;;
	RolledBack)
		expected_leaf=$from_leaf_secret
		expected_ca=$from_ca_secret
		expected_ca_key=$from_ca_key
		;;
	*) die "cannot verify unknown recorded phase $recorded_phase" ;;
	esac
	$JQ_BIN -e \
		--arg leaf_field "$leaf_field" --arg leaf "$expected_leaf" \
		--arg ca_field "$ca_field" --arg ca "$expected_ca" --arg ca_key "$expected_ca_key" '
	  .spec.tls[$leaf_field].name == $leaf and
	  .spec.tls[$ca_field].name == $ca and
	  (.spec.tls[$ca_field].key // "ca.crt") == $ca_key
	' <<<"$json" >/dev/null
}

volume_name_for_field() {
	local field=$1
	case $field in
	serverSecretRef) printf 'server-tls' ;;
	clientCASecretRef) printf 'client-ca' ;;
	clientSecretRef) printf 'client-tls' ;;
	serverCASecretRef) printf 'server-ca' ;;
	*) die "cannot map TLS field $field to a workload volume" ;;
	esac
}

workload_uses_secret() {
	local json=$1
	local volume=$2
	local secret=$3
	$JQ_BIN -e --arg volume "$volume" --arg secret "$secret" '
	  any(.spec.template.spec.volumes[]?;
	    .name == $volume and .secret.secretName == $secret)
	' <<<"$json" >/dev/null
}

root_matches_target() {
	local json=$1
	local field=$2
	local secret=$3
	local key=$4
	local recorded_phase=$5
	$JQ_BIN -e \
		--argjson generation "$target_root_generation" \
		--arg id_key "$annotation_id" --arg id "$rotation_id" \
		--arg direction_key "$annotation_direction" --arg direction "$direction" \
		--arg phase_key "$annotation_phase" --arg phase "$recorded_phase" \
		--arg field "$field" --arg secret "$secret" --arg key "$key" '
	  .metadata.generation == $generation and
	  .status.observedGeneration == $generation and
	  .metadata.annotations[$id_key] == $id and
	  .metadata.annotations[$direction_key] == $direction and
	  .metadata.annotations[$phase_key] == $phase and
	  .spec.tls[$field].name == $secret and
	  ($key == "" or (.spec.tls[$field].key // "ca.crt") == $key)
	' <<<"$json" >/dev/null && \
		root_has_bound_plan "$json" && root_has_phase_refs "$json" "$recorded_phase"
}

wait_fresh_agent_acknowledgments() {
	local controller_json controller_started_at
	controller_json=$(kubectl_run -n "$namespace" get pods \
		-l "app.kubernetes.io/instance=${root_name},app.kubernetes.io/component=controller" -o json)
	controller_started_at=$($JQ_BIN -r '
	  [.items[] | select(
	    .metadata.deletionTimestamp == null and
	    any(.status.conditions[]?; .type == "Ready" and .status == "True")
	  )] | if length == 1 then .[0].status.startTime else empty end
	' <<<"$controller_json")
	[[ -n $controller_started_at ]] || die "could not identify the one ready controller Pod after rollout"

	local deadline=$((SECONDS + TIMEOUT_SECONDS))
	local daemonset_json daemonset_uid desired pods_json pod_name pod_uid response sequence all_fresh
	local -A baseline_uids=()
	local -A baseline_sequences=()
	# Take the baseline only after the replacement controller is Ready. A later
	# increment must therefore have been acknowledged by that controller; this
	# avoids comparing clocks on different Kubernetes nodes.
	while ((SECONDS < deadline)); do
		daemonset_json=$(kubectl_run -n "$namespace" get daemonset "${root_name}-agent" -o json 2>/dev/null || true)
		# Do not use ${var:-{}} here: bash treats the first closing brace as the
		# end of the parameter expansion, so a non-empty value gains a trailing
		# `}` and becomes invalid JSON. Keep the empty fallback explicit.
		if [[ -z $daemonset_json ]]; then
			daemonset_json='{}'
		fi
		desired=$($JQ_BIN -r '.status.desiredNumberScheduled // 0' <<<"$daemonset_json")
		daemonset_uid=$($JQ_BIN -r '.metadata.uid // empty' <<<"$daemonset_json")
		pods_json=$(kubectl_run -n "$namespace" get pods \
			-l "app.kubernetes.io/instance=${root_name},app.kubernetes.io/component=agent" -o json 2>/dev/null || true)
		if [[ -z $pods_json ]]; then
			pods_json='{"items":[]}'
		fi
		if [[ $desired =~ ^[1-9][0-9]*$ ]] && \
			[[ -n $daemonset_uid ]] && \
			$JQ_BIN -e --argjson desired "$desired" --arg name "${root_name}-agent" --arg uid "$daemonset_uid" '
			  [.items[] | select(.metadata.deletionTimestamp == null)] as $pods |
			  ($pods | length) == $desired and
			  all($pods[];
			    any(.metadata.ownerReferences[]?;
			      .controller == true and .kind == "DaemonSet" and
			      .name == $name and .uid == $uid))
			' <<<"$pods_json" >/dev/null; then
			all_fresh=1
			while IFS=$'\t' read -r pod_name pod_uid; do
				[[ -n $pod_name ]] || continue
				response=$(kubectl_run --request-timeout=5s get --raw \
					"/api/v1/namespaces/${namespace}/pods/${pod_name}:9402/proxy/readyz" 2>/dev/null || true)
				sequence=$(sed -n 's/^ack_sequence=\([1-9][0-9]*\)$/\1/p' <<<"$response")
				if [[ ! $sequence =~ ^[1-9][0-9]*$ ]]; then
					all_fresh=0
					break
				fi
				baseline_uids["$pod_name"]=$pod_uid
				baseline_sequences["$pod_name"]=$sequence
			done < <($JQ_BIN -r '.items[] | select(.metadata.deletionTimestamp == null) | [.metadata.name, .metadata.uid] | @tsv' <<<"$pods_json")
			if ((all_fresh)); then
				break
			fi
		fi
		sleep 2
	done
	((${#baseline_sequences[@]} == desired)) || \
		die "could not read a current acknowledgment sequence from every managed agent"

	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		pods_json=$(kubectl_run -n "$namespace" get pods \
			-l "app.kubernetes.io/instance=${root_name},app.kubernetes.io/component=agent" -o json 2>/dev/null || true)
		if [[ -z $pods_json ]]; then
			pods_json='{"items":[]}'
		fi
		if $JQ_BIN -e --argjson desired "$desired" --arg name "${root_name}-agent" --arg uid "$daemonset_uid" '
		  [.items[] | select(.metadata.deletionTimestamp == null)] as $pods |
		  ($pods | length) == $desired and
		  all($pods[];
		    any(.metadata.ownerReferences[]?;
		      .controller == true and .kind == "DaemonSet" and
		      .name == $name and .uid == $uid))
		' <<<"$pods_json" >/dev/null; then
			all_fresh=1
			while IFS=$'\t' read -r pod_name pod_uid; do
				response=$(kubectl_run --request-timeout=5s get --raw \
					"/api/v1/namespaces/${namespace}/pods/${pod_name}:9402/proxy/readyz" 2>/dev/null || true)
				sequence=$(sed -n 's/^ack_sequence=\([1-9][0-9]*\)$/\1/p' <<<"$response")
				if [[ ! $sequence =~ ^[1-9][0-9]*$ ]]; then
					all_fresh=0
					break
				fi
				if [[ ${baseline_uids[$pod_name]:-} == "$pod_uid" ]] && \
					((sequence <= ${baseline_sequences[$pod_name]:-0})); then
					all_fresh=0
					break
				fi
			done < <($JQ_BIN -r '.items[] | select(.metadata.deletionTimestamp == null) | [.metadata.name, .metadata.uid] | @tsv' <<<"$pods_json")
			if ((all_fresh)); then
				note "every managed agent completed a durable registration heartbeat after replacement controller $controller_started_at was Ready"
				return 0
			fi
		fi
		sleep 2
	done
	die "agents did not advance their registration acknowledgment sequences after controller replacement"
}

wait_rollout_and_ready() {
	local kind=$1
	local name=$2
	local field=$3
	local secret=$4
	local key=$5
	local recorded_phase=$6
	local volume deadline workload_json
	volume=$(volume_name_for_field "$field")
	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		workload_json=$(kubectl_run -n "$namespace" get "$kind" "$name" -o json 2>/dev/null || true)
		if [[ -n $workload_json ]] && workload_uses_secret "$workload_json" "$volume" "$secret"; then
			break
		fi
		sleep 2
	done
	if [[ -z ${workload_json:-} ]] || ! workload_uses_secret "$workload_json" "$volume" "$secret"; then
		report_root_status
		die "$kind/$namespace/$name did not reconcile TLS field $field to Secret $secret"
	fi
	if ! kubectl_run -n "$namespace" rollout status "$kind/$name" --timeout="${TIMEOUT_SECONDS}s" >/dev/null; then
		report_root_status
		die "$kind/$namespace/$name did not complete its rollout; run the matching rollback phase"
	fi
	deadline=$((SECONDS + TIMEOUT_SECONDS))
	local json
	while ((SECONDS < deadline)); do
		json=$(kubectl_run get kubeneuron "$root_name" -o json)
		if root_ready "$json" && root_matches_target "$json" "$field" "$secret" "$key" "$recorded_phase"; then
			workload_json=$(kubectl_run -n "$namespace" get "$kind" "$name" -o json)
			$JQ_BIN -e '.metadata.generation == .status.observedGeneration' <<<"$workload_json" >/dev/null || {
				sleep 2
				continue
			}
			workload_uses_secret "$workload_json" "$volume" "$secret" || \
				die "$kind/$namespace/$name changed away from expected Secret $secret"
			if [[ $kind == deployment && $name == "${root_name}-controller" ]]; then
				wait_fresh_agent_acknowledgments
			fi
			note "$kind/$namespace/$name is rolled out and KubeNeuron $root_name is Ready"
			return 0
		fi
		sleep 2
	done
	report_root_status
	die "KubeNeuron $root_name did not become Ready; run the matching rollback phase"
}

case $phase in
expand-trust)
	if [[ $current_leaf == "$from_leaf_secret" && $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" && \
		$active_id == "$rotation_id" && $active_direction == "$direction" && $active_phase == TrustExpanded ]]; then
		require_bound_plan
		target_root_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
		note "$direction rotation $rotation_id trust target is already recorded; verifying rollout and readiness"
		wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$overlap_ca_secret" "$overlap_ca_key" TrustExpanded
		exit 0
	fi
	require_stable_root
	if [[ -n $active_id || -n $active_direction || -n $active_phase ]]; then
		if [[ $active_phase != OldTrustRetired && $active_phase != RolledBack ]]; then
			die "rotation ${active_id:-unknown}/${active_direction:-unknown} is still at ${active_phase:-unknown}; only one direction may rotate at a time"
		fi
		[[ $active_id != "$rotation_id" ]] || \
			die "rotation id $rotation_id is already terminal at $active_phase; choose a new globally unique id"
	fi
	[[ $current_leaf == "$from_leaf_secret" ]] || die "current $leaf_field is $current_leaf, want old leaf $from_leaf_secret"
	[[ $current_ca == "$from_ca_secret" && $current_ca_key == "$from_ca_key" ]] || \
		die "current $ca_field is $current_ca:$current_ca_key, want $from_ca_secret:$from_ca_key"
	load_initial_plan
	apply_phase "$ca_field" "$(ca_ref_json "$overlap_ca_secret" "$overlap_ca_key")" TrustExpanded
	wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$overlap_ca_secret" "$overlap_ca_key" TrustExpanded
	;;
activate-leaf)
	if [[ $current_leaf == "$new_leaf_secret" && $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" && \
		$active_id == "$rotation_id" && $active_direction == "$direction" && $active_phase == LeafActivated ]]; then
		require_bound_plan
		target_root_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
		note "$direction rotation $rotation_id leaf target is already recorded; verifying rollout and readiness"
		wait_rollout_and_ready "$producer_kind" "$producer_name" "$leaf_field" "$new_leaf_secret" '' LeafActivated
		exit 0
	fi
	require_stable_root
	require_transaction TrustExpanded
	require_uid "new leaf" "$(require_key_pair_secret "$new_leaf_secret" true)" "$recorded_new_leaf_uid"
	require_uid "overlap CA" "$(require_ca_secret "$overlap_ca_secret" "$overlap_ca_key" true)" "$recorded_overlap_ca_uid"
	[[ $current_leaf == "$from_leaf_secret" ]] || die "current $leaf_field is $current_leaf, want old leaf $from_leaf_secret"
	[[ $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" ]] || \
		die "trust is not on the declared overlap CA Secret"
	apply_phase "$leaf_field" "$($JQ_BIN -cn --arg name "$new_leaf_secret" '{name:$name}')" LeafActivated
	wait_rollout_and_ready "$producer_kind" "$producer_name" "$leaf_field" "$new_leaf_secret" '' LeafActivated
	;;
retire-old-trust)
	((approve_retire == 1)) || die "retire-old-trust requires --approve-retire-old-trust"
	if [[ $current_leaf == "$new_leaf_secret" && $current_ca == "$final_ca_secret" && $current_ca_key == "$final_ca_key" && \
		$active_id == "$rotation_id" && $active_direction == "$direction" && $active_phase == OldTrustRetired ]]; then
		require_bound_plan
		target_root_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
		note "$direction rotation $rotation_id retirement target is already recorded; verifying rollout and readiness"
		wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$final_ca_secret" "$final_ca_key" OldTrustRetired
		exit 0
	fi
	require_stable_root
	require_transaction LeafActivated
	require_uid "new leaf" "$(require_key_pair_secret "$new_leaf_secret" true)" "$recorded_new_leaf_uid"
	require_uid "overlap CA" "$(require_ca_secret "$overlap_ca_secret" "$overlap_ca_key" true)" "$recorded_overlap_ca_uid"
	require_uid "final CA" "$(require_ca_secret "$final_ca_secret" "$final_ca_key" true)" "$recorded_final_ca_uid"
	[[ $current_leaf == "$new_leaf_secret" ]] || die "new leaf is not active"
	[[ $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" ]] || \
		die "trust is not on the declared overlap CA Secret"
	apply_phase "$ca_field" "$(ca_ref_json "$final_ca_secret" "$final_ca_key")" OldTrustRetired
	wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$final_ca_secret" "$final_ca_key" OldTrustRetired
	;;
	rollback-retirement)
	if [[ $current_leaf == "$new_leaf_secret" && $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" && \
		$active_id == "$rotation_id" && $active_direction == "$direction" && $active_phase == LeafActivated ]]; then
		require_bound_plan
		target_root_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
		note "$direction rotation $rotation_id overlap target is already recorded; verifying rollout and readiness"
		wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$overlap_ca_secret" "$overlap_ca_key" LeafActivated
		exit 0
	fi
	require_transaction OldTrustRetired
	require_uid "overlap CA" "$(require_ca_secret "$overlap_ca_secret" "$overlap_ca_key" true)" "$recorded_overlap_ca_uid"
	[[ $current_leaf == "$new_leaf_secret" ]] || die "current leaf is not the declared new leaf"
	[[ $current_ca == "$final_ca_secret" && $current_ca_key == "$final_ca_key" ]] || \
		die "rollback-retirement requires the declared final CA reference"
	apply_phase "$ca_field" "$(ca_ref_json "$overlap_ca_secret" "$overlap_ca_key")" LeafActivated
	wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$overlap_ca_secret" "$overlap_ca_key" LeafActivated
	;;
	rollback-leaf)
	if [[ $current_leaf == "$from_leaf_secret" && $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" && \
		$active_id == "$rotation_id" && $active_direction == "$direction" && $active_phase == LeafRolledBack ]]; then
		require_bound_plan
		target_root_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
		note "$direction rotation $rotation_id old leaf target is already recorded; verifying rollout and readiness"
		wait_rollout_and_ready "$producer_kind" "$producer_name" "$leaf_field" "$from_leaf_secret" '' LeafRolledBack
		exit 0
	fi
	require_transaction LeafActivated
	require_uid "old leaf" "$(require_key_pair_secret "$from_leaf_secret" false)" "$recorded_from_leaf_uid"
	[[ $current_leaf == "$new_leaf_secret" ]] || die "current leaf is not the declared new leaf"
	[[ $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" ]] || \
		die "rollback-leaf is safe only while overlap trust remains active"
	apply_phase "$leaf_field" "$($JQ_BIN -cn --arg name "$from_leaf_secret" '{name:$name}')" LeafRolledBack
	wait_rollout_and_ready "$producer_kind" "$producer_name" "$leaf_field" "$from_leaf_secret" '' LeafRolledBack
	;;
rollback-trust)
	if [[ $current_leaf == "$from_leaf_secret" && $current_ca == "$from_ca_secret" && $current_ca_key == "$from_ca_key" && \
		$active_id == "$rotation_id" && $active_direction == "$direction" && $active_phase == RolledBack ]]; then
		require_bound_plan
		target_root_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
		note "$direction rotation $rotation_id old trust target is already recorded; verifying rollout and readiness"
		wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$from_ca_secret" "$from_ca_key" RolledBack
		exit 0
	fi
	[[ $active_id == "$rotation_id" && $active_direction == "$direction" ]] || \
		die "active rotation does not match $rotation_id/$direction"
	require_bound_plan
	[[ $active_phase == TrustExpanded || $active_phase == LeafRolledBack ]] || \
		die "rollback-trust requires TrustExpanded or LeafRolledBack, got ${active_phase:-none}"
	[[ $current_leaf == "$from_leaf_secret" ]] || die "restore the old leaf before retiring overlap trust"
	[[ $current_ca == "$overlap_ca_secret" && $current_ca_key == "$overlap_ca_key" ]] || \
		die "current trust is not the declared overlap CA"
	if [[ $active_phase == LeafRolledBack ]]; then
		target_root_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
		note "$direction rotation $rotation_id is verifying the old leaf rollout before contracting overlap trust"
		wait_rollout_and_ready "$producer_kind" "$producer_name" "$leaf_field" "$from_leaf_secret" '' LeafRolledBack
	fi
	require_uid "old CA" "$(require_ca_secret "$from_ca_secret" "$from_ca_key" false)" "$recorded_from_ca_uid"
	apply_phase "$ca_field" "$(ca_ref_json "$from_ca_secret" "$from_ca_key")" RolledBack
	wait_rollout_and_ready "$consumer_kind" "$consumer_name" "$ca_field" "$from_ca_secret" "$from_ca_key" RolledBack
	;;
esac

if [[ $phase == activate-leaf && $producer_kind == deployment ]]; then
	note "the single Recreate controller caused a real ingress outage; fresh post-start agent acknowledgments were verified, but an operator-chosen soak interval still precedes trust retirement"
fi
if [[ $phase == retire-old-trust ]]; then
	note "old trust is retired only after the old consumer process was replaced; keep all external Secrets until the rollback window closes"
fi
