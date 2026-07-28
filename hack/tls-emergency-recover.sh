#!/usr/bin/env bash
# Recover an unavailable KubeNeuron installation by atomically replacing both
# expired or otherwise unusable leaf references. This is deliberately not a
# CA rotation, issuance mechanism, revocation mechanism, or Secret editor.
# Candidate Secrets must already exist and remain external to the operator.
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell.
set -Eeuo pipefail

KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
JQ_BIN=${JQ_BIN:-jq}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-240}
KUBECONFIG_PATH=${KUBECONFIG_PATH:-${KUBECONFIG:-}}

root_name=
recovery_id=
server_leaf_secret=
client_leaf_secret=
approved=0

readonly annotation_id='kubeneuron.io/tls-emergency-recovery-id'
readonly annotation_server_leaf='kubeneuron.io/tls-emergency-recovery-server-leaf'
readonly annotation_server_leaf_uid='kubeneuron.io/tls-emergency-recovery-server-leaf-uid'
readonly annotation_client_leaf='kubeneuron.io/tls-emergency-recovery-client-leaf'
readonly annotation_client_leaf_uid='kubeneuron.io/tls-emergency-recovery-client-leaf-uid'
readonly annotation_previous_server_leaf='kubeneuron.io/tls-emergency-recovery-previous-server-leaf'
readonly annotation_previous_client_leaf='kubeneuron.io/tls-emergency-recovery-previous-client-leaf'

usage() {
	cat <<'EOF'
Usage:
  tls-emergency-recover.sh --root NAME --recovery-id ID \
    --server-leaf-secret NAME --client-leaf-secret NAME \
    --approve-emergency-leaf-recovery \
    [--kubeconfig PATH] [--timeout-seconds N]

Use only after a KubeNeuron root is unavailable because its leaf certificate
is expired or otherwise unusable. This keeps the currently referenced server
and client CA bundles unchanged, then changes both leaf references to the two
pre-created candidate Secrets in one resource-version-checked patch.

Each candidate must be a distinct, unowned, immutable kubernetes.io/tls
Secret. The server leaf must validate for
<root>-controller.<spec.namespace>.svc under the current server CA; the client
leaf must validate under the current client CA and have exactly the current
installation URI SAN. The script never creates, mutates, prints, or deletes
Secret data. It cannot recover an expired, revoked, or compromised CA.
EOF
}

die() {
	printf 'tls-emergency-recover: %s\n' "$*" >&2
	exit 1
}

note() {
	printf 'tls-emergency-recover: %s\n' "$*"
}

require_value() {
	local option=$1
	[[ -n ${2:-} ]] || die "$option requires a value"
}

while (($#)); do
	case $1 in
	--root)
		require_value "$1" "${2:-}"
		root_name=$2
		shift 2
		;;
	--recovery-id)
		require_value "$1" "${2:-}"
		recovery_id=$2
		shift 2
		;;
	--server-leaf-secret)
		require_value "$1" "${2:-}"
		server_leaf_secret=$2
		shift 2
		;;
	--client-leaf-secret)
		require_value "$1" "${2:-}"
		client_leaf_secret=$2
		shift 2
		;;
	--approve-emergency-leaf-recovery)
		approved=1
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
	*) die "unknown argument: $1" ;;
	esac
done

for required in root_name recovery_id server_leaf_secret client_leaf_secret; do
	[[ -n ${!required} ]] || die "missing required option for ${required//_/-}"
done
((approved == 1)) || die "--approve-emergency-leaf-recovery is required"
[[ $TIMEOUT_SECONDS =~ ^[1-9][0-9]*$ ]] || die "timeout-seconds must be a positive integer"
[[ $recovery_id =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$ ]] || die "recovery-id is not a safe identifier"
for name in "$root_name" "$server_leaf_secret" "$client_leaf_secret"; do
	[[ $name =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || die "unsafe Kubernetes name: $name"
done
[[ $server_leaf_secret != "$client_leaf_secret" ]] || die "server and client candidate Secret names must differ"

for command in "$KUBECTL_BIN" "$JQ_BIN" openssl base64 mktemp; do
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
installation_uid=$($JQ_BIN -r '.metadata.uid // empty' <<<"$root_json")
[[ -n $namespace && -n $installation_uid ]] || die "KubeNeuron $root_name is missing spec.namespace or metadata.uid"

current_server_leaf=$($JQ_BIN -r '.spec.tls.serverSecretRef.name // empty' <<<"$root_json")
current_client_leaf=$($JQ_BIN -r '.spec.tls.clientSecretRef.name // empty' <<<"$root_json")
server_ca_secret=$($JQ_BIN -r '.spec.tls.serverCASecretRef.name // empty' <<<"$root_json")
server_ca_key=$($JQ_BIN -r '.spec.tls.serverCASecretRef.key // "ca.crt"' <<<"$root_json")
client_ca_secret=$($JQ_BIN -r '.spec.tls.clientCASecretRef.name // empty' <<<"$root_json")
client_ca_key=$($JQ_BIN -r '.spec.tls.clientCASecretRef.key // "ca.crt"' <<<"$root_json")
for value in current_server_leaf current_client_leaf server_ca_secret server_ca_key client_ca_secret client_ca_key; do
	[[ -n ${!value} ]] || die "KubeNeuron $root_name has an incomplete TLS configuration"
done
[[ $server_leaf_secret != "$current_server_leaf" ]] || die "server candidate is already the current leaf; use the normal readiness investigation instead"
[[ $client_leaf_secret != "$current_client_leaf" ]] || die "client candidate is already the current leaf; use the normal readiness investigation instead"

root_ready() {
	local json=$1
	$JQ_BIN -e '
      .status.observedGeneration == .metadata.generation and
      any(.status.conditions[]?; .type == "Ready" and .status == "True" and
        .reason == "RuntimeAvailable" and .observedGeneration == .metadata.generation)
    ' <<<"$json" >/dev/null
}

if root_ready "$root_json"; then
	die "KubeNeuron $root_name is currently Ready; routine rotation is required instead of emergency recovery"
fi

routine_id=$($JQ_BIN -r '.metadata.annotations["kubeneuron.io/tls-rotation-id"] // empty' <<<"$root_json")
routine_phase=$($JQ_BIN -r '.metadata.annotations["kubeneuron.io/tls-rotation-phase"] // empty' <<<"$root_json")
if [[ -n $routine_id && $routine_phase != OldTrustRetired && $routine_phase != RolledBack ]]; then
	die "routine TLS rotation $routine_id is still at ${routine_phase:-unknown}; complete its documented rollback before emergency recovery"
fi

secret_json() {
	kubectl_run -n "$namespace" get secret "$1" -o json
}

require_candidate() {
	local name=$1 json
	json=$(secret_json "$name") || die "candidate Secret $namespace/$name does not exist"
	$JQ_BIN -e '
      .type == "kubernetes.io/tls" and .immutable == true and
      .data["tls.crt"] != null and .data["tls.crt"] != "" and
      .data["tls.key"] != null and .data["tls.key"] != "" and
      ((.metadata.ownerReferences // []) | length) == 0
    ' <<<"$json" >/dev/null || die "candidate Secret $namespace/$name must be unowned, immutable kubernetes.io/tls data"
	$JQ_BIN -r '.metadata.uid' <<<"$json"
}

server_leaf_uid=$(require_candidate "$server_leaf_secret")
client_leaf_uid=$(require_candidate "$client_leaf_secret")

tmp_dir=$(mktemp -d)
umask 077
# shellcheck disable=SC2317 # Invoked through the EXIT trap below.
cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT

write_secret_data() {
	local secret=$1 key=$2 output=$3 encoded
	encoded=$(secret_json "$secret" | $JQ_BIN -r --arg key "$key" '.data[$key] // empty')
	[[ -n $encoded ]] || die "Secret $namespace/$secret has no data key $key"
	printf '%s' "$encoded" | base64 --decode >"$output" || die "Secret $namespace/$secret data key $key is not valid base64"
}

write_secret_data "$server_ca_secret" "$server_ca_key" "$tmp_dir/server-ca.pem"
write_secret_data "$client_ca_secret" "$client_ca_key" "$tmp_dir/client-ca.pem"
write_secret_data "$server_leaf_secret" tls.crt "$tmp_dir/server.crt"
write_secret_data "$client_leaf_secret" tls.crt "$tmp_dir/client.crt"

service_dns="${root_name}-controller.${namespace}.svc"
openssl verify -purpose sslserver -verify_hostname "$service_dns" \
	-CAfile "$tmp_dir/server-ca.pem" "$tmp_dir/server.crt" >/dev/null 2>&1 || \
	die "server candidate does not validate for $service_dns under the current server CA"
openssl x509 -checkend 0 -noout -in "$tmp_dir/server.crt" >/dev/null 2>&1 || \
	die "server candidate is already expired"
openssl verify -purpose sslclient -CAfile "$tmp_dir/client-ca.pem" "$tmp_dir/client.crt" >/dev/null 2>&1 || \
	die "client candidate does not validate under the current client CA"
openssl x509 -checkend 0 -noout -in "$tmp_dir/client.crt" >/dev/null 2>&1 || \
	die "client candidate is already expired"
client_uri="spiffe://kubeneuron.io/installation/${installation_uid}/agent"
san_uris=$(openssl x509 -noout -ext subjectAltName -in "$tmp_dir/client.crt" 2>/dev/null | \
	grep -o 'URI:[^,[:space:]]*' | sed 's/^URI://' || true)
[[ $san_uris == "$client_uri" ]] || die "client candidate must have exactly installation URI SAN $client_uri"

recorded_id=$($JQ_BIN -r --arg key "$annotation_id" '.metadata.annotations[$key] // empty' <<<"$root_json")
recorded_server=$($JQ_BIN -r --arg key "$annotation_server_leaf" '.metadata.annotations[$key] // empty' <<<"$root_json")
recorded_server_uid=$($JQ_BIN -r --arg key "$annotation_server_leaf_uid" '.metadata.annotations[$key] // empty' <<<"$root_json")
recorded_client=$($JQ_BIN -r --arg key "$annotation_client_leaf" '.metadata.annotations[$key] // empty' <<<"$root_json")
recorded_client_uid=$($JQ_BIN -r --arg key "$annotation_client_leaf_uid" '.metadata.annotations[$key] // empty' <<<"$root_json")

target_generation=
if [[ $recorded_id == "$recovery_id" ]]; then
	[[ $recorded_server == "$server_leaf_secret" && $recorded_server_uid == "$server_leaf_uid" ]] || die "recorded emergency server leaf plan does not match this candidate"
	[[ $recorded_client == "$client_leaf_secret" && $recorded_client_uid == "$client_leaf_uid" ]] || die "recorded emergency client leaf plan does not match this candidate"
	$JQ_BIN -e --arg server "$server_leaf_secret" --arg client "$client_leaf_secret" '
      .spec.tls.serverSecretRef.name == $server and .spec.tls.clientSecretRef.name == $client
    ' <<<"$root_json" >/dev/null || die "recorded emergency recovery does not match current leaf references"
	target_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
	note "emergency recovery $recovery_id is already recorded; verifying rollout and readiness"
else
	if [[ -n $recorded_id ]]; then
		$JQ_BIN -e --arg server "$recorded_server" --arg client "$recorded_client" '
          .status.observedGeneration == .metadata.generation and
          .spec.tls.serverSecretRef.name == $server and .spec.tls.clientSecretRef.name == $client
        ' <<<"$root_json" >/dev/null ||
			die "recorded emergency recovery $recorded_id is still reconciling or its leaf references changed; rerun it with the same recovery id"
		note "superseding completed emergency recovery $recorded_id after a new unready state"
	fi
	resource_version=$($JQ_BIN -r '.metadata.resourceVersion' <<<"$root_json")
	patch=$($JQ_BIN -cn \
		--arg rv "$resource_version" \
		--arg id_key "$annotation_id" --arg id "$recovery_id" \
		--arg server_key "$annotation_server_leaf" --arg server "$server_leaf_secret" \
		--arg server_uid_key "$annotation_server_leaf_uid" --arg server_uid "$server_leaf_uid" \
		--arg client_key "$annotation_client_leaf" --arg client "$client_leaf_secret" \
		--arg client_uid_key "$annotation_client_leaf_uid" --arg client_uid "$client_leaf_uid" \
		--arg previous_server_key "$annotation_previous_server_leaf" --arg previous_server "$current_server_leaf" \
		--arg previous_client_key "$annotation_previous_client_leaf" --arg previous_client "$current_client_leaf" '
      {
        metadata: {resourceVersion: $rv, annotations: {
          ($id_key): $id,
          ($server_key): $server, ($server_uid_key): $server_uid,
          ($client_key): $client, ($client_uid_key): $client_uid,
          ($previous_server_key): $previous_server,
          ($previous_client_key): $previous_client
        }},
        spec: {tls: {serverSecretRef: {name: $server}, clientSecretRef: {name: $client}}}
      }
    ')
	root_json=$(kubectl_run patch kubeneuron "$root_name" --type=merge -p "$patch" -o json)
	target_generation=$($JQ_BIN -r '.metadata.generation' <<<"$root_json")
	[[ $target_generation =~ ^[1-9][0-9]*$ ]] || die "emergency patch returned an invalid root generation"
	note "emergency recovery $recovery_id recorded at root generation $target_generation"
fi

wait_workload_secret() {
	local kind=$1 name=$2 volume=$3 secret=$4 deadline json
	deadline=$((SECONDS + TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		json=$(kubectl_run -n "$namespace" get "$kind" "$name" -o json 2>/dev/null || true)
		if [[ -n $json ]] && $JQ_BIN -e --arg volume "$volume" --arg secret "$secret" '
          any(.spec.template.spec.volumes[]?; .name == $volume and .secret.secretName == $secret)
        ' <<<"$json" >/dev/null; then
			kubectl_run -n "$namespace" rollout status "$kind/$name" --timeout="${TIMEOUT_SECONDS}s" >/dev/null ||
				die "$kind/$namespace/$name did not complete emergency leaf rollout"
			return
		fi
		sleep 2
	done
	die "$kind/$namespace/$name did not reconcile emergency Secret $secret"
}

wait_workload_secret deployment "${root_name}-controller" server-tls "$server_leaf_secret"
wait_workload_secret daemonset "${root_name}-agent" client-tls "$client_leaf_secret"

deadline=$((SECONDS + TIMEOUT_SECONDS))
while ((SECONDS < deadline)); do
	root_json=$(kubectl_run get kubeneuron "$root_name" -o json)
	if $JQ_BIN -e \
		--argjson generation "$target_generation" \
		--arg id_key "$annotation_id" --arg id "$recovery_id" \
		--arg server_key "$annotation_server_leaf" --arg server "$server_leaf_secret" \
		--arg server_uid_key "$annotation_server_leaf_uid" --arg server_uid "$server_leaf_uid" \
		--arg client_key "$annotation_client_leaf" --arg client "$client_leaf_secret" \
		--arg client_uid_key "$annotation_client_leaf_uid" --arg client_uid "$client_leaf_uid" '
      .metadata.generation == $generation and .status.observedGeneration == $generation and
      .metadata.annotations[$id_key] == $id and
      .metadata.annotations[$server_key] == $server and .metadata.annotations[$server_uid_key] == $server_uid and
      .metadata.annotations[$client_key] == $client and .metadata.annotations[$client_uid_key] == $client_uid and
      .spec.tls.serverSecretRef.name == $server and .spec.tls.clientSecretRef.name == $client and
      any(.status.conditions[]?; .type == "Ready" and .status == "True" and .reason == "RuntimeAvailable" and .observedGeneration == $generation)
    ' <<<"$root_json" >/dev/null; then
		note "controller and agents rolled out with externally owned emergency leaves; KubeNeuron $root_name is Ready"
		exit 0
	fi
	sleep 2
done

ready_status=$($JQ_BIN -r '.status.conditions[]? | select(.type == "Ready") | "Ready=\(.status)/\(.reason): \(.message)"' <<<"$root_json")
die "KubeNeuron $root_name did not recover readiness after emergency leaf patch${ready_status:+ ($ready_status)}"
