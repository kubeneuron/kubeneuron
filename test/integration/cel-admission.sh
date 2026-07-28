#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell, variables.
set -euo pipefail

# Exercises the KubeNeuron CRD CEL rules against a real Kubernetes API server.
# The caller must install config/crd first, provide KUBECONFIG, and explicitly
# acknowledge that the script creates and deletes a cluster-scoped fixture.
# This test intentionally does not install or exercise the KubeNeuron operator.

KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
JQ_BIN=${JQ_BIN:-jq}
FIXTURE_NAME=${CEL_FIXTURE_NAME:-cel-integration}

passed=0
fixture_created=0

log() {
	printf 'CEL: %s\n' "$*"
}

fail() {
	printf 'CEL FAIL: %s\n' "$*" >&2
	exit 1
}

pass() {
	passed=$((passed + 1))
	printf 'CEL PASS [%02d] %s\n' "$passed" "$1"
}

cleanup() {
	if ((fixture_created)); then
		"$KUBECTL_BIN" delete kubeneuron "$FIXTURE_NAME" \
			--ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

for command in "$KUBECTL_BIN" "$JQ_BIN"; do
	command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done
[[ -n ${KUBECONFIG:-} ]] || fail "KUBECONFIG must point at the integration cluster"
[[ ${CEL_ALLOW_CLUSTER_MUTATION:-0} == 1 ]] || \
	fail "set CEL_ALLOW_CLUSTER_MUTATION=1 for the dedicated integration cluster"

server_json=$("$KUBECTL_BIN" version -o json)
server_version=$("$JQ_BIN" -r '.serverVersion.gitVersion' <<<"$server_json")
server_minor=$("$JQ_BIN" -r '.serverVersion.minor | sub("[^0-9].*$"; "") | tonumber' <<<"$server_json")
((server_minor >= 29)) || fail "Kubernetes 1.29 or newer is required; server is $server_version"

"$KUBECTL_BIN" get crd/kubeneurons.kubeneuron.io >/dev/null 2>&1 || \
	fail "KubeNeuron CRDs are not installed"
if "$KUBECTL_BIN" get kubeneuron "$FIXTURE_NAME" >/dev/null 2>&1; then
	fail "refusing to replace existing KubeNeuron fixture $FIXTURE_NAME"
fi

BASE=$("$JQ_BIN" -cn --arg name "$FIXTURE_NAME" '{
  apiVersion: "kubeneuron.io/v1alpha1",
  kind: "KubeNeuron",
  metadata: {name: $name},
  spec: {
    namespace: "cel-target",
    controller: {image: "example.invalid/controller:test"},
    agent: {image: "example.invalid/agent:test"},
    safety: {executionMode: "DryRun"},
    workflowStore: {type: "SQLite", sqlite: {}},
	    notifications: {operatorAPIToken: {name: "cel-api-token"}, webhookToken: {name: "cel-webhook-token"}},
	    observability: {
	      victoriaMetrics: {mode: "External", endpoint: "http://vm.invalid"},
	      alertmanager: {mode: "External", endpoint: "http://am.invalid"}
	    },
	    tls: {
	      serverSecretRef: {name: "controller-tls"},
	      clientCASecretRef: {name: "agent-client-ca"},
	      clientSecretRef: {name: "agent-tls"},
	      serverCASecretRef: {name: "controller-server-ca"}
	    }
	  }
	}')

check_root() {
	local label=$1
	local expected=$2
	local filter=$3
	local document output rc
	document=$("$JQ_BIN" -c "$filter" <<<"$BASE")
	set +e
	output=$(printf '%s\n' "$document" | "$KUBECTL_BIN" apply --dry-run=server -f - 2>&1)
	rc=$?
	set -e
	if [[ -z $expected ]]; then
		((rc == 0)) || fail "$label unexpectedly rejected: $output"
		pass "$label accepted"
		return
	fi
	((rc != 0)) || fail "$label unexpectedly accepted"
	grep -Fq "$expected" <<<"$output" || \
		fail "$label rejected without expected message '$expected': $output"
	pass "$label rejected: $expected"
}

check_document() {
	local label=$1
	local expected=$2
	local document=$3
	local output rc
	set +e
	output=$(printf '%s\n' "$document" | "$KUBECTL_BIN" apply --dry-run=server -f - 2>&1)
	rc=$?
	set -e
	if [[ -z $expected ]]; then
		((rc == 0)) || fail "$label unexpectedly rejected: $output"
		pass "$label accepted"
		return
	fi
	((rc != 0)) || fail "$label unexpectedly accepted"
	grep -Fq "$expected" <<<"$output" || \
		fail "$label rejected without expected message '$expected': $output"
	pass "$label rejected: $expected"
}

check_patch() {
	local label=$1
	local expected=$2
	local patch=$3
	local output rc
	set +e
	output=$("$KUBECTL_BIN" patch kubeneuron "$FIXTURE_NAME" \
		--type=merge --dry-run=server -p "$patch" 2>&1)
	rc=$?
	set -e
	if [[ -z $expected ]]; then
		((rc == 0)) || fail "$label unexpectedly rejected: $output"
		pass "$label accepted"
		return
	fi
	((rc != 0)) || fail "$label unexpectedly accepted"
	grep -Fq "$expected" <<<"$output" || \
		fail "$label rejected without expected message '$expected': $output"
	pass "$label rejected: $expected"
}

printf '%s\n' "$BASE" | "$KUBECTL_BIN" apply -f - >/dev/null
fixture_created=1
pass "valid SQLite root persisted"

stored_size=$("$KUBECTL_BIN" get kubeneuron "$FIXTURE_NAME" \
	-o jsonpath='{.spec.workflowStore.sqlite.size}')
[[ $stored_size == 5Gi ]] || fail "sqlite.size default was '$stored_size', want 5Gi"
pass "sqlite.size default persisted as 5Gi"

check_root "SQLite without sqlite settings" \
	"sqlite settings are required for SQLite" \
	'.metadata.name="cel-no-sqlite" | del(.spec.workflowStore.sqlite)'
check_root "zero SQLite quantity" \
	"size must be a positive Kubernetes quantity" \
	'.metadata.name="cel-zero" | .spec.workflowStore.sqlite.size="0"'
check_root "malformed SQLite quantity" \
	"size must be a positive Kubernetes quantity" \
	'.metadata.name="cel-bogus" | .spec.workflowStore.sqlite.size="bogus"'
check_root "External VictoriaMetrics without endpoint" \
	"External dependencies require an endpoint" \
	'.metadata.name="cel-no-endpoint" | del(.spec.observability.victoriaMetrics.endpoint)'
check_root "external ClickHouse archive" \
	"clickHouse currently supports only Disabled mode" \
	'.metadata.name="cel-clickhouse-external" | .spec.archive.clickHouse={mode:"External",endpoint:"http://clickhouse.invalid"}'
check_root "disabled ClickHouse archive" "" \
	'.metadata.name="cel-clickhouse-disabled" | .spec.archive.clickHouse={mode:"Disabled"}'
check_root "structurally valid Postgres store" "" \
	'.metadata.name="cel-postgres" | .spec.workflowStore={type:"Postgres",secretRef:{name:"pg"}}'
check_root "Postgres store without a DSN secret" \
	"secretRef is required for Postgres" \
	'.metadata.name="cel-postgres-nosecret" | .spec.workflowStore={type:"Postgres"}'
check_root "cross-namespace Postgres DSN secret" \
	"the Postgres DSN Secret must omit namespace" \
	'.metadata.name="cel-postgres-ns" | .spec.workflowStore={type:"Postgres",secretRef:{name:"pg",namespace:"other"}}'
check_root "missing TLS configuration" \
	"tls: Required value" \
	'.metadata.name="cel-no-tls" | del(.spec.tls)'
check_root "incomplete TLS references" \
	"clientSecretRef: Required value" \
	'.metadata.name="cel-tls-incomplete" | del(.spec.tls.clientSecretRef)'
check_root "key-pair TLS key selector" \
	"TLS key-pair Secret references cannot select one key" \
	'.metadata.name="cel-tls-key" | .spec.tls.serverSecretRef.key="tls.crt"'
check_root "client key-pair TLS key selector" \
	"TLS key-pair Secret references cannot select one key" \
	'.metadata.name="cel-tls-client-key" | .spec.tls.clientSecretRef.key="tls.crt"'
check_root "cross-namespace server TLS reference" \
	"TLS Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-tls-server-namespace" | .spec.tls.serverSecretRef.namespace="other"'
check_root "cross-namespace client CA reference" \
	"TLS Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-tls-client-ca-namespace" | .spec.tls.clientCASecretRef.namespace="other"'
check_root "cross-namespace client TLS reference" \
	"TLS Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-tls-client-namespace" | .spec.tls.clientSecretRef.namespace="other"'
check_root "cross-namespace server CA reference" \
	"TLS Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-tls-server-ca-namespace" | .spec.tls.serverCASecretRef.namespace="other"'
check_root "custom CA bundle keys" "" \
	'.metadata.name="cel-tls-ca-keys" | .spec.tls.clientCASecretRef.key="clients.pem" | .spec.tls.serverCASecretRef.key="servers.pem"'
check_root "optional public TLS reference" "" \
	'.metadata.name="cel-tls-public" | .spec.tls.publicServerSecretRef={name:"public-tls"}'
check_root "public TLS key selector" \
	"TLS key-pair Secret references cannot select one key" \
	'.metadata.name="cel-tls-public-key" | .spec.tls.publicServerSecretRef={name:"public-tls",key:"tls.crt"}'
check_root "cross-namespace public TLS reference" \
	"TLS Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-tls-public-namespace" | .spec.tls.publicServerSecretRef={name:"public-tls",namespace:"other"}'
check_root "Enabled without notification channel" \
	"executionMode Enabled is disabled" \
	'.metadata.name="cel-enabled-no-notifier" | .spec.safety.executionMode="Enabled"'
check_root "Enabled with Slack notifier" "executionMode Enabled is disabled" \
	'.metadata.name="cel-enabled-notifier" | .spec.safety.executionMode="Enabled" | .spec.notifications.slack={name:"slack-webhook"}'
check_root "missing authenticated Alertmanager webhook" \
	"notifications.webhookToken is required" \
	'.metadata.name="cel-no-webhook-token" | del(.spec.notifications.webhookToken)'
check_root "Paused without authenticated operator API" \
	"notifications.operatorAPIToken is required" \
	'.metadata.name="cel-paused-no-api-token" | .spec.safety.executionMode="Paused" | del(.spec.notifications.operatorAPIToken)'
check_root "cross-namespace Slack reference" \
	"notification Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-slack-namespace" | .spec.notifications.slack={name:"slack-webhook",namespace:"other"}'
check_root "cross-namespace API token reference" \
	"notification Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-api-token-namespace" | .spec.notifications.operatorAPIToken.namespace="other"'
check_root "cross-namespace webhook token reference" \
	"notification Secret references must omit namespace and use spec.namespace" \
	'.metadata.name="cel-webhook-token-namespace" | .spec.notifications.webhookToken.namespace="other"'
check_root "floating :latest controller image" \
	"image must not use the floating :latest tag" \
	'.metadata.name="cel-controller-latest" | .spec.controller.image="example.invalid/controller:latest"'
check_root "floating :latest agent image" \
	"image must not use the floating :latest tag" \
	'.metadata.name="cel-agent-latest" | .spec.agent.image="example.invalid/agent:latest"'
check_root "untagged controller image" \
	"image must be pinned by an explicit tag or digest" \
	'.metadata.name="cel-controller-untagged" | .spec.controller.image="example.invalid/controller"'
check_root "digest-pinned agent image" "" \
	'.metadata.name="cel-agent-digest" | .spec.agent.image="example.invalid/agent@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"'
check_root "host tooling with defaults" "" \
	'.metadata.name="cel-host-tooling" | .spec.agent.hostTooling={}'
check_root "relative host tooling binDir" \
	"binDir must be an absolute host path" \
	'.metadata.name="cel-host-tooling-bin" | .spec.agent.hostTooling={binDir: "usr/bin"}'
check_root "host tooling libDir with colon" \
	"libDirs entries must be absolute host paths" \
	'.metadata.name="cel-host-tooling-lib" | .spec.agent.hostTooling={libDirs: ["/usr/lib64", "/opt:/evil"]}'
check_root "relative host tooling scriptsDir" \
	"scriptsDir must be an absolute host path" \
	'.metadata.name="cel-host-tooling-scripts" | .spec.agent.hostTooling={scriptsDir: "scripts"}'
check_root "notification webhook channel" "" \
	'.metadata.name="cel-notify-webhook" | .spec.notifications.webhook={name: "notify-hook"} | .spec.notifications.pagerduty={name: "pd-key"}'
check_root "cross-namespace notification webhook reference" \
	"notification Secret references must omit namespace" \
	'.metadata.name="cel-notify-webhook-ns" | .spec.notifications.webhook={name: "notify-hook", namespace: "other"}'
check_root "cross-namespace PagerDuty reference" \
	"notification Secret references must omit namespace" \
	'.metadata.name="cel-notify-pd-ns" | .spec.notifications.pagerduty={name: "pd-key", namespace: "other"}'
check_root "auth with users and OIDC" "" \
	'.metadata.name="cel-auth" | .spec.auth={users: {name: "panel-users"}, oidc: {issuerURL: "https://sso.example.com", clientID: "kn", clientSecretRef: {name: "oidc-client"}, redirectURL: "https://panel.example.com/api/v1/auth/oidc/callback"}}'
check_root "cross-namespace auth users Secret" \
	"auth Secret references must omit namespace" \
	'.metadata.name="cel-auth-users-ns" | .spec.auth={users: {name: "panel-users", namespace: "other"}}'
check_root "plain-http OIDC issuer" \
	"issuerURL must be https" \
	'.metadata.name="cel-auth-oidc-http" | .spec.auth={oidc: {issuerURL: "http://sso.example.com", clientID: "kn", clientSecretRef: {name: "oidc-client"}, redirectURL: "https://panel.example.com/cb"}}'

"$KUBECTL_BIN" patch kubeneuron "$FIXTURE_NAME" --type=merge \
	-p '{"spec":{"workflowStore":{"sqlite":{"size":"6Gi"}}}}' >/dev/null
grown_size=$("$KUBECTL_BIN" get kubeneuron "$FIXTURE_NAME" \
	-o jsonpath='{.spec.workflowStore.sqlite.size}')
[[ $grown_size == 6Gi ]] || fail "accepted grow stored '$grown_size', want 6Gi"
pass "SQLite quantity growth from 5Gi to 6Gi persisted"

check_patch "SQLite shrink" "size cannot decrease" \
	'{"spec":{"workflowStore":{"sqlite":{"size":"4Gi"}}}}'
check_patch "SQLite size removal/default bypass" "size cannot decrease" \
	'{"spec":{"workflowStore":{"sqlite":{"size":null}}}}'
check_patch "storageClassName presence change" "storageClassName is immutable" \
	'{"spec":{"workflowStore":{"sqlite":{"storageClassName":"fast"}}}}'
check_patch "workflow store type change" "workflow store type is immutable" \
	'{"spec":{"workflowStore":{"type":"Postgres","sqlite":null,"secretRef":{"name":"pg"}}}}'
check_patch "target namespace change" "namespace is immutable" \
	'{"spec":{"namespace":"cel-other"}}'

SIGNAL_VALID=$("$JQ_BIN" -cn '{
  apiVersion:"kubeneuron.io/v1alpha1",
  kind:"GPUSignalMapping",
  metadata:{name:"cel-signal"},
  spec:{kubeNeuronRef:"no-such",source:"dcgm",xidCodes:[79],class:"xid-79",severity:"warning"}
}')
check_document "single xidCodes signal matcher" "" "$SIGNAL_VALID"
check_document "both signal matchers" \
	"exactly one of xidCodes or alertName must be set" \
	"$("$JQ_BIN" -c '.metadata.name="cel-signal-both" | .spec.alertName="GPUXidError"' <<<"$SIGNAL_VALID")"
check_document "no signal matcher" \
	"exactly one of xidCodes or alertName must be set" \
	"$("$JQ_BIN" -c '.metadata.name="cel-signal-neither" | del(.spec.xidCodes)' <<<"$SIGNAL_VALID")"

MAINTENANCE_VALID=$("$JQ_BIN" -cn '{
  apiVersion:"kubeneuron.io/v1alpha1",
  kind:"GPUMaintenanceWindow",
  metadata:{name:"cel-maintenance"},
  spec:{
    kubeNeuronRef:"no-such",
    nodeSelector:{},
    startsAt:"2026-07-12T00:00:00Z",
    endsAt:"2026-07-12T01:00:00Z",
    pauseAutomation:true
  }
}')
check_document "ordered maintenance window" "" "$MAINTENANCE_VALID"
check_document "reversed maintenance window" \
	"startsAt must be before endsAt" \
	"$("$JQ_BIN" -c '.metadata.name="cel-maintenance-reversed" | .spec.endsAt="2026-07-11T23:00:00Z"' <<<"$MAINTENANCE_VALID")"

POLICY_VALID=$("$JQ_BIN" -cn '{
  apiVersion:"kubeneuron.io/v1alpha1",
  kind:"GPURemediationPolicy",
  metadata:{name:"cel-policy"},
  spec:{kubeNeuronRef:"no-such",priority:10,match:{class:"xid-79"},playbookRef:"drain-and-reset"}
}')
check_document "class-only policy match" "" "$POLICY_VALID"
check_document "policy with unsupported severity match" \
	"match fields source/severity/nodeSelector are not supported" \
	"$("$JQ_BIN" -c '.metadata.name="cel-policy-severity" | .spec.match.severity="critical"' <<<"$POLICY_VALID")"

PLAYBOOK_VALID=$("$JQ_BIN" -cn '{
  apiVersion:"kubeneuron.io/v1alpha1",
  kind:"GPUPlaybook",
  metadata:{name:"cel-playbook"},
  spec:{
    kubeNeuronRef:"no-such",
    target:"Node",
    cooldown:"6h",
    steps:[
      {name:"drain",action:"Drain",timeout:"10m"},
      {name:"reboot",action:"Reboot",approval:"Required"}
    ]
  }
}')
check_document "reboot playbook with required approval" "" "$PLAYBOOK_VALID"
check_document "reboot step without approval" \
	"Reboot steps require approval Required" \
	"$("$JQ_BIN" -c '.metadata.name="cel-playbook-noapproval" | .spec.steps[1].approval="None"' <<<"$PLAYBOOK_VALID")"
check_document "malformed playbook cooldown" \
	"should match" \
	"$("$JQ_BIN" -c '.metadata.name="cel-playbook-cooldown" | .spec.cooldown="6 hours"' <<<"$PLAYBOOK_VALID")"

NODECONFIG_VALID=$("$JQ_BIN" -cn '{
  apiVersion:"kubeneuron.io/v1alpha1",
  kind:"GPUNodeConfig",
  metadata:{name:"cel-nodeconfig"},
  spec:{kubeNeuronRef:"no-such",nodeName:"n1",paused:true}
}')
check_document "paused node config" "" "$NODECONFIG_VALID"
check_document "node config with SSH credentials" \
	"sshSecretRef/bmcSecretRef are not supported" \
	"$("$JQ_BIN" -c '.metadata.name="cel-nodeconfig-ssh" | .spec.sshSecretRef={name:"ssh-key"}' <<<"$NODECONFIG_VALID")"

"$KUBECTL_BIN" delete kubeneuron "$FIXTURE_NAME" --wait=true --timeout=60s >/dev/null
fixture_created=0
if "$KUBECTL_BIN" get kubeneuron "$FIXTURE_NAME" >/dev/null 2>&1; then
	fail "CEL fixture cleanup left $FIXTURE_NAME behind"
fi
pass "persisted CEL fixture cleaned up"

((passed == 63)) || fail "internal check count is $passed, want 63"
log "admission matrix complete: $passed checks passed on server $server_version"
