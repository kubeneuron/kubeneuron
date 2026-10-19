#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell, variables.
set -Eeuo pipefail

# Disposable kind fixture for the v0.7 GPU observability rows of the G2 gate.
#
# CPU-only and synthetic, by construction: the "exporter" is hack/dcgmfixture,
# a Go program that serves DCGM_FI_* text exposition with made-up values and
# the real dcgm-exporter label shape, and the two Alertmanager delivery
# targets are hack/alertsink, a recorder that answers 200 and acts on
# nothing. Nothing here touches NVML, DCGM, a GPU driver, GPU hardware or the
# KubeNeuron controller. A green run proves only that, on the pinned images:
#
#   * the shipped vmagent relabeling and recording rules wire synthetic source
#     series into the canonical and derived series the catalog promises;
#   * one shipped ShadowGpu* rule fires on a synthetic input and the shipped
#     Alertmanager route tree delivers it to human-default and never to the
#     controller webhook;
#   * Grafana provisions the five checked-in boards against a vmsingle
#     datasource and every panel and variable query executes without error.
#
# Those are rows of the G2 gate, not G2, and no hardware claim.

usage() {
	cat <<'USAGE'
Usage: hack/kind-gpu-observability.sh [--verify]

Creates a dedicated, digest-pinned kind cluster (one control plane, two
workers), runs a synthetic dcgm-exporter stand-in on each worker, brings up
vmagent -> vmsingle -> vmalert -> Alertmanager (VictoriaMetrics v1.147.0 and
Alertmanager v0.32.1, the tags pinned in
deploy/kubernetes/dependencies/versions.lock.yaml), two webhook recorder
sinks, and Grafana (tag+digest pinned in the same lock), all with the
checked-in scrape contract, recording rules, shadow rules, Alertmanager
route tree and dashboards, and asserts:

  Recording rules and label contract (VictoriaMetrics query API):
  * source series carry gpu_uuid/gpu_model beside the exporter's UUID/modelName
  * canonical temperature, power and memory series exist per device
  * MiB -> bytes is numerically exact for kubeneuron_gpu_memory_used_bytes
  * node_pool is present exactly on the node that carries a node-pool label
    (and EKS nodegroup wins over the kubeneuron.io fallback)
  * cluster is absent on every series by default
  * omitted sources (153, 393-396, NVLink 409+) give ABSENT canonical and
    derived series, never 0, while present zero-valued sources give 0
  * the thermal headroom falls back to slowdown - temperature without 153
  * exactly one deliberately slow Tesla T4 (of seven) gets
    kubeneuron_gpu_fail_slow_score_ratio, and a two-device NVIDIA A10G group
    that would otherwise qualify gets none

  Shadow routing (vmalert -> Alertmanager -> recorder sinks):
  * vmalert loads configs/vmalert/gpu-shadow-rules.yaml beside the recording
    rules; every alerting rule it holds is ShadowGpu* with the shadow label
  * the one device reporting unrepairable memory makes the shipped
    ShadowGpuMemoryBeyondRepair rule fire after its own `for`
  * Alertmanager, running the shipped route tree verbatim, resolves it to
    human-default only and delivers it to the human sink
  * the controller webhook sink (deployed under the shipped receiver's own
    Service name, namespace, port and bearer-token file) records exactly
    zero requests while that happens
  * two API-injected probes then prove the harness can tell: a shadow-labelled
    Gpu*-named alert still lands on the human sink, and a plain Gpu*-named
    alert reaches the controller sink with the bearer token, so the zero
    above was a routing result, not a dead receiver

  Dashboards (Grafana HTTP API through the API server proxy):
  * the provisioned datasource exists and its health check against vmsingle
    passes
  * all five deploy/grafana boards are provisioned with their checked-in
    uid, title, panel and variable count, and every datasource reference is
    bound to the provisioned datasource (the ${DS_PROMETHEUS} placeholder
    is the only edit, the same binding the Grafana import dialog performs)
  * every variable query and every panel expression (variables substituted
    with their "All" value) executes through Grafana's datasource API
    without a query error: a syntax and wiring check on synthetic data,
    not a statement about panel semantics or values

CPU-only synthetic boundary: this proves rule, routing and provisioning
WIRING with fabricated series and recorder sinks. It proves nothing about
DCGM, dcgm-exporter, GPU drivers, real GPU faults, a real pager or any
hardware, and it never involves the KubeNeuron controller.

  --verify     offline static checks only (bash -n, shellcheck, go vet/test of
               the fixture programs, scrape-config and Alertmanager-config
               extraction, route/rule/dashboard/image drift checks, manifest
               rendering; amtool route tests when amtool is available); no
               cluster, no docker, no network

Configuration is through environment variables:
  CLUSTER_NAME       kind cluster name (default: kubeneuron-gpu-observability);
                     the harness refuses a name that already exists
  KEEP_CLUSTER       1 to retain the cluster, kubeconfig and work dir on exit
  TIMEOUT_SECONDS    per-wait deadline in seconds (default: 300)
  KIND_BIN, KUBECTL_BIN, DOCKER_BIN, JQ_BIN, GO_BIN, PYTHON_BIN, AMTOOL_BIN
                     command paths (each must be one executable; AMTOOL_BIN
                     is optional and only used by --verify)

The cluster always uses kind v0.32.0 and this immutable node image:
  kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4

If the current login predates membership in the docker group, invoke it as:
  sg docker -c './hack/kind-gpu-observability.sh'
USAGE
}

MODE=run
case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
--verify)
	MODE=verify
	shift
	;;
esac
[[ $# -eq 0 ]] || {
	usage >&2
	exit 2
}

readonly EXPECTED_KIND_VERSION=v0.32.0
readonly NODE_IMAGE='kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4'
readonly EXPECTED_SERVER_VERSION=v1.33.12
# The namespace and Service the shipped vmagent relabeling keeps.
readonly FIXTURE_NAMESPACE=gpu-operator
readonly PIPELINE_NAMESPACE=kubeneuron-gpu-observability
readonly NODE_POOL_FALLBACK=fixture-fallback-pool
readonly NODE_POOL_EKS=fixture-eks-pool
# The -spec hack/dcgmfixture runs with on node A (node B runs a single healthy
# NVIDIA A10G). Pinned here, in fixture.yaml.tmpl and in
# hack/dcgmfixture/main_test.go; --verify fails if the three drift.
readonly HARNESS_SPEC_A='Tesla T4:7:6:2;NVIDIA A10G:1:0'
# Device identities the fixture derives from that spec: node A runs Tesla T4
# gpu 0..6 (6 slow, 2 reporting unrepairable memory) and NVIDIA A10G gpu 7
# (slow); node B runs NVIDIA A10G gpu 0 (healthy).
readonly T4_GPU0_UUID=GPU-fixture-tesla-t4-00000000
readonly T4_SLOW_UUID=GPU-fixture-tesla-t4-00000006
readonly T4_UNREPAIRABLE_UUID=GPU-fixture-tesla-t4-00000002
readonly A10G_SLOW_UUID=GPU-fixture-nvidia-a10g-00000007
readonly TOTAL_DEVICES=9
readonly NODE_A_DEVICES=8
# Values the fixture serves (pinned by hack/dcgmfixture/main_test.go too).
readonly FB_USED_GPU0_MIB=12345
readonly GPU_TEMP_GPU0=60
readonly SLOWDOWN_TEMP=85
readonly SLOW_SM_CLOCK=1000
readonly MAX_SM_CLOCK=1590
readonly SLOW_THROTTLE_SHARE=0.12

# Shadow routing. The trigger rule is the shipped one with the shortest
# `for`; its hold time is read from the rule file, never assumed.
readonly SHADOW_LABEL=kubeneuron_shadow
readonly SHADOW_TRIGGER_ALERT=ShadowGpuMemoryBeyondRepair
readonly HUMAN_RECEIVER=human-default
readonly CONTROLLER_RECEIVER=kubeneuron
readonly HUMAN_SINK_PATH=/fixture/human-default
# API-injected probe alerts (never rules): Gpu-prefixed on purpose, because
# the shipped controller route matches alertname =~ "Gpu.*".
readonly PROBE_ALERTNAME=GpuFixtureRoutingProbe

# Grafana. The datasource uid the harness binds ${DS_PROMETHEUS} to; must
# equal the uid in grafana-datasources.yaml (--verify checks).
readonly GRAFANA_DS_UID=kubeneuron-vmsingle
readonly DASHBOARD_DS_PLACEHOLDER='${DS_PROMETHEUS}'
readonly -a DASHBOARDS=(fleet-overview health-errors fail-slow-radar remediation gpu-passport)

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
readonly FIXTURE_DIR="$REPO_ROOT/test/integration/gpu-observability"
readonly OBSERVABILITY_DIR="$REPO_ROOT/deploy/kubernetes/dependencies/observability"
readonly VMAGENT_CR="$OBSERVABILITY_DIR/vmagent.yaml"
readonly VMALERTMANAGER_CR="$OBSERVABILITY_DIR/vmalertmanager.yaml"
readonly RULES_FILE="$REPO_ROOT/configs/vmalert/gpu-recording-rules.yaml"
readonly SHADOW_RULES_FILE="$REPO_ROOT/configs/vmalert/gpu-shadow-rules.yaml"
readonly LOCK_FILE="$REPO_ROOT/deploy/kubernetes/dependencies/versions.lock.yaml"
readonly DASHBOARD_DIR="$REPO_ROOT/deploy/grafana"

KIND_BIN=${KIND_BIN:-kind}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
DOCKER_BIN=${DOCKER_BIN:-docker}
JQ_BIN=${JQ_BIN:-jq}
GO_BIN=${GO_BIN:-go}
PYTHON_BIN=${PYTHON_BIN:-python3}
AMTOOL_BIN=${AMTOOL_BIN:-amtool}
CLUSTER_NAME=${CLUSTER_NAME:-kubeneuron-gpu-observability}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-300}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}
FIXTURE_IMAGE="dcgm-fixture:kind-gpu-observability-${RUN_ID}"
HUMAN_WEBHOOK_URL="http://alert-sink-human.${PIPELINE_NAMESPACE}.svc:8080${HUMAN_SINK_PATH}"

created_cluster=0
built_image=0
work_dir=
KUBECONFIG_PATH=
failures=0
# Filled by verify_offline from the checked-in files; used by run_cluster.
SHADOW_FOR_SECONDS=
SHADOW_RULE_COUNT=
declare -A CONTRACT=()

note() { printf '==> %s\n' "$*"; }
fail() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}
check() {
	# check <description> <ok:0|1>
	if [[ $2 -eq 0 ]]; then
		printf 'ok   %s\n' "$1"
	else
		printf 'FAIL %s\n' "$1" >&2
		failures=$((failures + 1))
	fi
}
assert_true() {
	# assert_true <description> <command...>: records the command's status
	# instead of letting errexit end the run, so every assertion is reported.
	local desc=$1
	shift
	if "$@"; then
		check "$desc" 0
	else
		check "$desc" 1
	fi
}

test_fails() {
	# test_fails <command...>: succeeds exactly when the command fails.
	! "$@"
}

require_cmd() {
	local var=$1 value=$2
	[[ $(printf '%s' "$value" | wc -w) -eq 1 ]] || fail "$var must be a single executable path, got '$value'"
	command -v -- "$value" >/dev/null 2>&1 || fail "$var ($value) is not executable"
}

cleanup() {
	local rc=$?
	set +e
	if [[ $KEEP_CLUSTER == 1 && $created_cluster == 1 ]]; then
		note "KEEP_CLUSTER=1: keeping cluster $CLUSTER_NAME; kubeconfig at $KUBECONFIG_PATH"
		note "delete it with: $KIND_BIN delete cluster --name $CLUSTER_NAME && rm -rf $work_dir"
		exit "$rc"
	fi
	if [[ $created_cluster == 1 ]]; then
		note "deleting kind cluster $CLUSTER_NAME"
		timeout 180 "$KIND_BIN" delete cluster --name "$CLUSTER_NAME" || printf 'WARN: cluster delete failed; run: %s delete cluster --name %s\n' "$KIND_BIN" "$CLUSTER_NAME" >&2
	fi
	if [[ $built_image == 1 ]]; then
		"$DOCKER_BIN" image rm -f "$FIXTURE_IMAGE" >/dev/null 2>&1 || true
	fi
	[[ -n $work_dir ]] && rm -rf -- "$work_dir"
	exit "$rc"
}

# ---------------------------------------------------------------------------
# Rendering helpers shared by --verify and the cluster run.
# ---------------------------------------------------------------------------
render_fixture_manifest() {
	# render_fixture_manifest <outdir> <image> <node-a> <node-b>
	sed -e "s|__IMAGE__|$2|g" -e "s|__NODE_A__|$3|g" -e "s|__NODE_B__|$4|g" \
		"$FIXTURE_DIR/fixture.yaml.tmpl" >"$1/fixture.yaml"
}

render_alertmanager() {
	# render_alertmanager <outdir>: alertmanager.yaml (config, from the
	# checked-in VMAlertmanager), contract.json (what the untouched kubeneuron
	# receiver needs), alertmanager-deploy.yaml and alert-sinks.yaml.
	local out=$1
	"$PYTHON_BIN" -I "$FIXTURE_DIR/extract-alertmanager-config.py" "$VMALERTMANAGER_CR" "$HUMAN_WEBHOOK_URL" \
		--contract "$out/contract.json" >"$out/alertmanager.yaml" || return 1
	local key
	for key in controller_service controller_namespace controller_port controller_path credentials_secret credentials_key credentials_mount_dir credentials_file; do
		CONTRACT[$key]=$("$JQ_BIN" -er --arg k "$key" '.[$k]' "$out/contract.json") || return 1
	done
	sed -e "s|__TOKEN_SECRET__|${CONTRACT[credentials_secret]}|g" -e "s|__TOKEN_MOUNT_DIR__|${CONTRACT[credentials_mount_dir]}|g" \
		"$FIXTURE_DIR/alertmanager.yaml.tmpl" >"$out/alertmanager-deploy.yaml"
	sed -e "s|__IMAGE__|$FIXTURE_IMAGE|g" -e "s|__PIPELINE_NAMESPACE__|$PIPELINE_NAMESPACE|g" \
		-e "s|__CONTROLLER_NAMESPACE__|${CONTRACT[controller_namespace]}|g" \
		-e "s|__CONTROLLER_SERVICE__|${CONTRACT[controller_service]}|g" \
		-e "s|__CONTROLLER_PORT__|${CONTRACT[controller_port]}|g" \
		"$FIXTURE_DIR/alert-sinks.yaml.tmpl" >"$out/alert-sinks.yaml"
}

render_dashboards() {
	# render_dashboards <outdir>: the five boards with the datasource
	# placeholder bound to the provisioned uid. Structural (jq walk over
	# every string value), so the edit is exactly that and nothing else.
	local out=$1 d
	mkdir -p "$out/dashboards"
	for d in "${DASHBOARDS[@]}"; do
		"$JQ_BIN" --arg ph "$DASHBOARD_DS_PLACEHOLDER" --arg uid "$GRAFANA_DS_UID" \
			'walk(if type == "string" and . == $ph then $uid else . end)' \
			"$DASHBOARD_DIR/$d.json" >"$out/dashboards/$d.json" || return 1
	done
}

# ---------------------------------------------------------------------------
# Offline verification. Also run first in `run` mode: a harness that cannot
# pass its own static checks must not create a cluster.
# ---------------------------------------------------------------------------
verify_offline() {
	note "static checks"
	assert_true "bash -n $(basename "${BASH_SOURCE[0]}")" bash -n "${BASH_SOURCE[0]}"
	if command -v shellcheck >/dev/null 2>&1; then
		assert_true "shellcheck $(basename "${BASH_SOURCE[0]}")" shellcheck "${BASH_SOURCE[0]}"
	else
		printf 'skip shellcheck (not installed)\n'
	fi

	require_cmd GO_BIN "$GO_BIN"
	require_cmd PYTHON_BIN "$PYTHON_BIN"
	require_cmd JQ_BIN "$JQ_BIN"
	go_check() { (cd "$REPO_ROOT" && "$GO_BIN" vet ./hack/dcgmfixture ./hack/alertsink && "$GO_BIN" test ./hack/dcgmfixture ./hack/alertsink); }
	assert_true "go vet + go test ./hack/dcgmfixture ./hack/alertsink" go_check

	# -- image pins --------------------------------------------------------------
	# Every image the fixture runs is the one the lock file names; Grafana by
	# tag and digest, nothing by a mutable tag.
	local pins
	if ! pins=$("$PYTHON_BIN" -I - "$LOCK_FILE" <<'PY'
import re, sys, yaml
lock = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
images = lock["images"]
tags = {images[k]["tag"] for k in ("victoriaMetricsSingle", "victoriaMetricsAgent", "victoriaMetricsAlert")}
if len(tags) != 1:
    sys.exit(f"lock file pins several VictoriaMetrics tags: {sorted(tags)}")
am = images["prometheusAlertmanager"]
gf = images["grafana"]
if not re.fullmatch(r"sha256:[0-9a-f]{64}", gf.get("digest", "")):
    sys.exit("lock file: images.grafana.digest must be sha256:<64 hex>")
if gf["tag"] in ("latest", "main", "master") or not re.fullmatch(r"\d+\.\d+\.\d+", gf["tag"]):
    sys.exit(f"lock file: images.grafana.tag {gf['tag']!r} is not a release tag")
print(tags.pop(), am["repository"] + ":" + am["tag"], gf["repository"] + ":" + gf["tag"] + "@" + gf["digest"])
PY
	); then
		check "versions.lock.yaml pins VictoriaMetrics, Alertmanager and Grafana" 1
		fail "cannot continue without the pins"
	fi
	local vm_tag am_image gf_image
	read -r vm_tag am_image gf_image <<<"$pins"
	check "versions.lock.yaml pins VictoriaMetrics $vm_tag, $am_image, $gf_image" 0
	local f ok=0
	for f in vmsingle.yaml vmagent.yaml vmalert.yaml; do
		grep -q "image: victoriametrics/[a-z-]*:${vm_tag}\$" "$FIXTURE_DIR/$f" || ok=1
	done
	check "VictoriaMetrics fixture manifests use only the pinned tag $vm_tag" $ok
	assert_true "alertmanager.yaml.tmpl uses the pinned $am_image" grep -q "image: ${am_image}\$" "$FIXTURE_DIR/alertmanager.yaml.tmpl"
	assert_true "grafana.yaml uses the pinned tag and digest" grep -q "image: ${gf_image}\$" "$FIXTURE_DIR/grafana.yaml"
	ok=0
	for f in "$FIXTURE_DIR"/*.yaml "$FIXTURE_DIR"/*.tmpl; do
		grep -Eq 'image: .*:(latest|main|master)\b' "$f" && ok=1
	done
	check "no fixture manifest names a mutable image tag" $ok
	assert_true "node image is digest-pinned" grep -qF -- "$NODE_IMAGE" "${BASH_SOURCE[0]}"

	local tmp
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/kind-gpu-observability-verify.XXXXXX")

	# -- scrape contract ---------------------------------------------------------
	extract_scrape() { "$PYTHON_BIN" -I "$FIXTURE_DIR/extract-scrape-config.py" "$VMAGENT_CR" >"$tmp/scrape.yaml"; }
	assert_true "scrape config extracted from checked-in VMAgent" extract_scrape
	contract_check() {
		"$PYTHON_BIN" -I - "$tmp/scrape.yaml" <<'PY'
import sys, yaml
cfg = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
jobs = {j["job_name"] for j in cfg["scrape_configs"]}
assert "dcgm-exporter" in jobs, jobs
assert "external_labels" not in cfg["global"], "shipped VMAgent must not set external labels (cluster)"
dcgm = next(j for j in cfg["scrape_configs"] if j["job_name"] == "dcgm-exporter")
targets = {r.get("target_label") for r in dcgm["relabel_configs"] + dcgm["metric_relabel_configs"]}
for want in ("node", "node_pool", "gpu_uuid", "gpu_model", "mig_profile"):
    assert want in targets, f"{want} missing from relabeling"
assert dcgm["kubernetes_sd_configs"][0]["attach_metadata"] == {"node": True}
PY
	}
	assert_true "extracted scrape config carries the M2 label contract" contract_check

	# -- fixture spec drift ------------------------------------------------------
	assert_true "fixture.yaml.tmpl runs node A with the pinned spec" grep -qF -- "- -spec=$HARNESS_SPEC_A" "$FIXTURE_DIR/fixture.yaml.tmpl"
	assert_true "hack/dcgmfixture/main_test.go pins the same spec" grep -qF -- "\"$HARNESS_SPEC_A\"" "$REPO_ROOT/hack/dcgmfixture/main_test.go"

	# -- Alertmanager: shipped route tree, test-only transport -------------------
	assert_true "Alertmanager config rendered from checked-in VMAlertmanager" render_alertmanager "$tmp"
	if [[ -n ${CONTRACT[controller_service]:-} ]]; then
		printf 'info controller webhook: %s.%s.svc:%s%s, token file %s\n' "${CONTRACT[controller_service]}" "${CONTRACT[controller_namespace]}" "${CONTRACT[controller_port]}" "${CONTRACT[controller_path]}" "${CONTRACT[credentials_file]}"
	fi
	route_check() {
		"$PYTHON_BIN" -I - "$VMALERTMANAGER_CR" "$tmp/alertmanager.yaml" "$HUMAN_RECEIVER" "$CONTROLLER_RECEIVER" "$HUMAN_WEBHOOK_URL" "$SHADOW_LABEL" <<'PY'
import sys, yaml
cr_path, rendered_path, human, controller, human_url, shadow_label = sys.argv[1:]
cr = next(d for d in yaml.safe_load_all(open(cr_path, encoding="utf-8")) if d and d.get("kind") == "VMAlertmanager")
shipped = yaml.safe_load(cr["spec"]["configRawYaml"])
rendered = yaml.safe_load(open(rendered_path, encoding="utf-8"))
assert rendered["route"] == shipped["route"], "route tree differs from the shipped one"
assert rendered.get("inhibit_rules") == shipped.get("inhibit_rules"), "inhibit rules differ from the shipped ones"
assert [r["name"] for r in rendered["receivers"]] == [r["name"] for r in shipped["receivers"]], "receiver set or order differs"
s_ctl = next(r for r in shipped["receivers"] if r["name"] == controller)
r_ctl = next(r for r in rendered["receivers"] if r["name"] == controller)
assert r_ctl == s_ctl, "kubeneuron receiver was edited"
r_hum = next(r for r in rendered["receivers"] if r["name"] == human)
assert r_hum == {"name": human, "webhook_configs": [{"url": human_url, "send_resolved": True}]}, f"human-default substitution is not exactly one webhook: {r_hum}"
assert set(rendered) == set(shipped), f"top-level keys differ: {sorted(set(rendered) ^ set(shipped))}"
# The shadow route is the first child, does not continue, and matches the
# shadow label only; the controller route comes after it.
children = shipped["route"]["routes"]
first = children[0]
assert first["receiver"] == human and first.get("continue") is False, f"first route is not a non-continuing {human} route: {first}"
assert [m.replace(" ", "") for m in first["matchers"]] == [f'{shadow_label}="true"'], f"first route matchers {first['matchers']}"
ctl_idx = next(i for i, r in enumerate(children) if r["receiver"] == controller)
assert ctl_idx > 0, "controller route is consulted before the shadow route"
assert shipped["route"]["receiver"] == human, "root receiver is not human-default"
PY
	}
	assert_true "rendered config keeps the shipped route tree, inhibit rules and kubeneuron receiver; only human-default gains one webhook" route_check
	if command -v -- "$AMTOOL_BIN" >/dev/null 2>&1; then
		assert_true "amtool check-config on the rendered config" "$AMTOOL_BIN" check-config "$tmp/alertmanager.yaml"
		am_route() {
			# am_route <expected-receiver> <label=value...>
			local want=$1
			shift
			[[ $("$AMTOOL_BIN" config routes test --config.file="$tmp/alertmanager.yaml" "$@" 2>/dev/null) == "$want" ]]
		}
		assert_true "amtool routes: $SHADOW_TRIGGER_ALERT with shadow label -> $HUMAN_RECEIVER" am_route "$HUMAN_RECEIVER" "alertname=$SHADOW_TRIGGER_ALERT" "$SHADOW_LABEL=true"
		assert_true "amtool routes: shadow-labelled Gpu*-named alert -> $HUMAN_RECEIVER" am_route "$HUMAN_RECEIVER" "alertname=$PROBE_ALERTNAME" "$SHADOW_LABEL=true"
		assert_true "amtool routes: plain Gpu*-named alert -> $CONTROLLER_RECEIVER" am_route "$CONTROLLER_RECEIVER" "alertname=$PROBE_ALERTNAME"
		assert_true "amtool routes: $SHADOW_TRIGGER_ALERT even without the label falls to the root $HUMAN_RECEIVER (a ShadowGpu* name never matches the controller route)" am_route "$HUMAN_RECEIVER" "alertname=$SHADOW_TRIGGER_ALERT"
	else
		printf 'skip amtool route tests (AMTOOL_BIN=%s not found; internal/depprofile/shadow_routing_test.go covers the route semantics)\n' "$AMTOOL_BIN"
	fi

	# -- shadow rules ------------------------------------------------------------
	local rule_facts
	if rule_facts=$("$PYTHON_BIN" -I - "$SHADOW_RULES_FILE" "$SHADOW_TRIGGER_ALERT" "$SHADOW_LABEL" <<'PY'
import re, sys, yaml
path, trigger, shadow_label = sys.argv[1:]
doc = yaml.safe_load(open(path, encoding="utf-8"))
rules = [r for g in doc["groups"] for r in g["rules"]]
assert rules and all("alert" in r for r in rules), "shadow file must hold alerting rules only"
for r in rules:
    assert r["alert"].startswith("ShadowGpu"), f"{r['alert']} is not ShadowGpu*"
    assert r["labels"].get(shadow_label) == "true", f"{r['alert']} lacks {shadow_label}=\"true\""
    assert "vector(0)" not in r["expr"] and "absent(" not in r["expr"], f"{r['alert']} invents data"
t = next(r for r in rules if r["alert"] == trigger)
assert "unrepairable" in t["expr"] and "== 1" in t["expr"], f"{trigger} expr changed: {t['expr']}"
m = re.fullmatch(r"(\d+)([smh])", str(t["for"]))
assert m, f"{trigger} for={t['for']!r} is not <n>[smh]"
seconds = int(m.group(1)) * {"s": 1, "m": 60, "h": 3600}[m.group(2)]
assert seconds == min(
    int(re.fullmatch(r"(\d+)([smh])", str(r["for"])).group(1)) * {"s": 1, "m": 60, "h": 3600}[re.fullmatch(r"(\d+)([smh])", str(r["for"])).group(2)]
    for r in rules
), f"{trigger} no longer has the shortest hold; pick the trigger again"
print(seconds, len(rules))
PY
	); then
		read -r SHADOW_FOR_SECONDS SHADOW_RULE_COUNT <<<"$rule_facts"
		check "gpu-shadow-rules.yaml: $SHADOW_RULE_COUNT ShadowGpu* rules with the shadow label; $SHADOW_TRIGGER_ALERT holds for ${SHADOW_FOR_SECONDS}s" 0
	else
		check "gpu-shadow-rules.yaml shape and trigger rule" 1
	fi
	vmalert_args() {
		local rules
		rules=$(grep -E '^ *- -rule=' "$FIXTURE_DIR/vmalert.yaml" | sed 's/^ *- -rule=//' | sort | tr '\n' ' ')
		[[ $rules == "/etc/vmalert/rules/gpu-recording-rules.yaml /etc/vmalert/rules/gpu-shadow-rules.yaml " ]] &&
			grep -q -- '- -notifier.url=http://alertmanager:9093$' "$FIXTURE_DIR/vmalert.yaml" &&
			! grep -q -- 'notifier.blackhole' "$FIXTURE_DIR/vmalert.yaml"
	}
	assert_true "fixture vmalert loads exactly the recording and shadow rule files and notifies the fixture Alertmanager" vmalert_args

	# -- dashboards --------------------------------------------------------------
	local d ds_refs reverted original
	ok=0
	for d in "${DASHBOARDS[@]}"; do
		[[ -f $DASHBOARD_DIR/$d.json ]] || {
			printf 'missing %s\n' "$DASHBOARD_DIR/$d.json" >&2
			ok=1
			continue
		}
		ds_refs=$("$JQ_BIN" -c '[.. | objects | select(has("datasource")) | .datasource | if type == "object" then .uid else . end] | unique' "$DASHBOARD_DIR/$d.json") || ok=1
		[[ $ds_refs == "[\"$DASHBOARD_DS_PLACEHOLDER\"]" ]] || {
			printf '%s: datasource references %s, want only %s\n' "$d" "$ds_refs" "$DASHBOARD_DS_PLACEHOLDER" >&2
			ok=1
		}
		"$JQ_BIN" -e '(.uid | length) > 0 and (.title | length) > 0 and (.panels | length) > 0 and (.templating.list | length) > 0' "$DASHBOARD_DIR/$d.json" >/dev/null || ok=1
	done
	check "all ${#DASHBOARDS[@]} checked-in boards parse and reference only the $DASHBOARD_DS_PLACEHOLDER datasource" $ok
	assert_true "boards render with the placeholder bound to $GRAFANA_DS_UID" render_dashboards "$tmp"
	ok=0
	for d in "${DASHBOARDS[@]}"; do
		[[ -f $tmp/dashboards/$d.json ]] || {
			ok=1
			continue
		}
		grep -q '\${DS_' "$tmp/dashboards/$d.json" && ok=1
		[[ $("$JQ_BIN" -r '[.. | objects | select(has("datasource")) | .datasource.uid] | unique | join(",")' "$tmp/dashboards/$d.json") == "$GRAFANA_DS_UID" ]] || ok=1
		# Nothing but the placeholder changed: undoing the substitution gives
		# back the checked-in document.
		reverted=$("$JQ_BIN" -S --arg ph "$DASHBOARD_DS_PLACEHOLDER" --arg uid "$GRAFANA_DS_UID" 'walk(if type == "string" and . == $uid then $ph else . end)' "$tmp/dashboards/$d.json")
		original=$("$JQ_BIN" -S . "$DASHBOARD_DIR/$d.json")
		[[ $reverted == "$original" ]] || ok=1
	done
	check "rendered boards differ from the checked-in ones only in the datasource uid" $ok
	ds_check() {
		"$PYTHON_BIN" -I - "$FIXTURE_DIR/grafana-datasources.yaml" "$FIXTURE_DIR/grafana-dashboards-provider.yaml" "$FIXTURE_DIR/grafana.yaml" "$GRAFANA_DS_UID" <<'PY'
import sys, yaml
ds_path, provider_path, deploy_path, uid = sys.argv[1:]
ds = yaml.safe_load(open(ds_path, encoding="utf-8"))["datasources"]
assert len(ds) == 1 and ds[0]["uid"] == uid and ds[0]["type"] == "prometheus" and ds[0]["url"] == "http://vmsingle:8428", ds
providers = yaml.safe_load(open(provider_path, encoding="utf-8"))["providers"]
assert len(providers) == 1 and providers[0]["type"] == "file", providers
path = providers[0]["options"]["path"]
deploy = next(d for d in yaml.safe_load_all(open(deploy_path, encoding="utf-8")) if d["kind"] == "Deployment")
c = deploy["spec"]["template"]["spec"]["containers"][0]
mounts = {m["mountPath"]: m["name"] for m in c["volumeMounts"]}
assert mounts.get(path) == "dashboards", f"provider path {path} is not the dashboards mount: {mounts}"
volumes = {v["name"]: v for v in deploy["spec"]["template"]["spec"]["volumes"]}
assert volumes["dashboards"]["configMap"]["name"] == "grafana-dashboards"
env = {e["name"]: e["value"] for e in c["env"]}
assert env["GF_AUTH_ANONYMOUS_ENABLED"] == "true" and env["GF_PLUGINS_PREINSTALL_DISABLED"] == "true" and env["GF_ANALYTICS_CHECK_FOR_UPDATES"] == "false"
PY
	}
	assert_true "Grafana provisioning files agree with each other and with the harness datasource uid" ds_check

	# -- Grafana panel query judgement -------------------------------------------
	# gf_panel_query with a stubbed poster: the transport's stderr must not
	# reach the JSON judgement (a cluster run once failed one of 48 panels
	# on kubectl's nonfatal websocket warning prepended to a 200 body), and
	# every real failure mode must still fail.
	local stub_out stub_err stub_rc
	stub_post() {
		printf '%s\n' "$stub_err" >&2
		printf '%s' "$stub_out"
		return "$stub_rc"
	}
	panel_case() {
		# panel_case <stdout> <stderr> <exit> -> gf_panel_query's status
		stub_out=$1 stub_err=$2 stub_rc=$3
		gf_panel_query stub_post "$tmp/query.json" "$tmp/query.err" 2>"$tmp/query.diag"
	}
	local good='{"results":{"A":{"status":200,"frames":[]}}}'
	local noise='E1018 12:00:00.000000 1 websocket.go:297] Unknown stream id 1, discarding message'
	printf '{}' >"$tmp/query.json"
	assert_true "panel query: nonfatal kubectl stderr beside a 200 body passes" panel_case "$good" "$noise" 0
	assert_true "panel query: empty result set passes" panel_case '{"results":{}}' "" 0
	assert_true "panel query: nonzero post with a 200-looking body fails" test_fails panel_case "$good" "alertsink: POST http://grafana:3000/api/ds/query: HTTP 502" 1
	assert_true "panel query: transport noise inside stdout fails" test_fails panel_case "$noise"$'\n'"$good" "" 0
	assert_true "panel query: non-JSON body fails" test_fails panel_case "<html>bad gateway</html>" "" 0
	assert_true "panel query: JSON without results fails" test_fails panel_case '{"message":"unauthorized"}' "" 0
	assert_true "panel query: per-result 400 fails" test_fails panel_case '{"results":{"A":{"status":400,"error":"bad_data: parse error"}}}' "" 0
	assert_true "panel query: per-result error with status 200 fails" test_fails panel_case '{"results":{"A":{"status":200,"error":"query timed out"}}}' "" 0
	diag_check() {
		panel_case "$good" "alertsink: POST http://grafana:3000/api/ds/query: HTTP 502" 1 && return 1
		grep -q 'post exited nonzero' "$tmp/query.diag" && grep -q 'HTTP 502' "$tmp/query.diag" && [[ $(wc -c <"$tmp/query.diag") -lt 1000 ]]
	}
	assert_true "panel query: failure diagnostic names the reason, keeps the stderr tail and stays bounded" diag_check

	# -- manifests render and parse ---------------------------------------------
	render_fixture_manifest "$tmp" "$FIXTURE_IMAGE" node-a node-b
	sed "s/__NAMESPACE__/$PIPELINE_NAMESPACE/g" "$FIXTURE_DIR/vmagent.yaml" >"$tmp/vmagent.yaml"
	manifest_check() {
		"$PYTHON_BIN" -I - "$tmp/fixture.yaml" "$tmp/vmagent.yaml" "$FIXTURE_DIR/vmsingle.yaml" "$FIXTURE_DIR/vmalert.yaml" \
			"$tmp/alertmanager-deploy.yaml" "$tmp/alert-sinks.yaml" "$FIXTURE_DIR/grafana.yaml" <<'PY'
import sys, yaml
for path in sys.argv[1:]:
    docs = [d for d in yaml.safe_load_all(open(path, encoding="utf-8")) if d]
    assert docs, path
    for d in docs:
        assert "kind" in d and "metadata" in d, (path, d)
        text = yaml.safe_dump(d)
        assert "__" not in text.replace("__name__", ""), f"unrendered placeholder in {path}"
PY
	}
	assert_true "manifests render and parse as Kubernetes objects" manifest_check
	omitted_read() { grep -q 'DCGM_FI_DEV_GPU_TEMP_LIMIT' "$RULES_FILE" && grep -q 'DCGM_FI_DEV_ROW_REMAP_PENDING' "$RULES_FILE" && grep -q 'DCGM_FI_DEV_NVLINK_' "$RULES_FILE"; }
	assert_true "recording rules still read the deliberately omitted fields" omitted_read
	headroom_waited() {
		# Regression (2026-10-18): the thermal headroom count was asserted
		# once, right after waiting on a different rule group, and came back
		# 8 of 9. Every count of that series in the cluster run must be a
		# bounded wait. The pattern is a regex so this line is not a hit.
		local hits
		hits=$(grep -E 'count\(kubeneuron_gpu_thermal_headroom_celsius\)' "${BASH_SOURCE[0]}") || return 1
		! grep -qvE '^[[:space:]]*wait_for_count ' <<<"$hits"
	}
	assert_true "the cluster run waits for the thermal headroom count instead of asserting it once" headroom_waited
	rm -rf -- "$tmp"
}

# ---------------------------------------------------------------------------
# Query helpers: every GET goes through the API server's service proxy, so
# there is no port-forward process to leak and every call is bounded. The
# few POSTs (probe alerts, Grafana panel queries) cannot: that proxy rewrites
# the request Content-Type to application/octet-stream, which Alertmanager
# (415) and Grafana (400) reject, so they run as `kubectl exec -i` of the
# fixture's own recorder binary in client mode (hack/alertsink -post) inside
# the human sink pod, with the body on stdin. Still bounded, still no
# port-forward.
# ---------------------------------------------------------------------------
kc() { "$KUBECTL_BIN" --kubeconfig "$KUBECONFIG_PATH" --request-timeout=30s "$@"; }

svc_proxy() {
	# svc_proxy <namespace> <service:port> <path-with-query> -> GET body
	kc get --raw "/api/v1/namespaces/$1/services/$2/proxy$3"
}

pod_post() {
	# pod_post <in-cluster-url> <json-file> -> POST response body; fails on a
	# non-2xx status.
	kc exec -i -n "$PIPELINE_NAMESPACE" deploy/alert-sink-human -- /alertsink -post "$1" <"$2"
}

vm_proxy() {
	# vm_proxy <service:port> <path-with-query>
	svc_proxy "$PIPELINE_NAMESPACE" "$1" "$2"
}

vm_query() {
	# vm_query <expr> -> JSON result array; empty array when no series.
	local encoded response
	encoded=$("$JQ_BIN" -rn --arg q "$1" '$q|@uri')
	response=$(vm_proxy vmsingle:http "/api/v1/query?query=$encoded") || return 1
	"$JQ_BIN" -ec 'select(.status=="success") | .data.result' <<<"$response" || {
		printf 'query failed: %s\n%s\n' "$1" "$response" >&2
		return 1
	}
}

series_count() {
	# Number of series an instant query returns (0 when there is no result).
	vm_query "$1" | "$JQ_BIN" -r 'length'
}

count_value() {
	# Value of a count(...) style query: 0 when it returns no series (an
	# aggregation over nothing is empty in PromQL/MetricsQL, never 0), the
	# single value otherwise, and "?" when it returns several series.
	local result n
	result=$(vm_query "$1") || return 1
	n=$("$JQ_BIN" -r 'length' <<<"$result")
	case $n in
	0) echo 0 ;;
	1) "$JQ_BIN" -r '.[0].value[1]' <<<"$result" ;;
	*) echo "?" ;;
	esac
}

scalar_value() {
	# The single value of a one-series instant query; fails otherwise.
	local result
	result=$(vm_query "$1")
	[[ $("$JQ_BIN" -r 'length' <<<"$result") -eq 1 ]] || {
		printf 'expected exactly one series for %s, got: %s\n' "$1" "$result" >&2
		return 1
	}
	"$JQ_BIN" -r '.[0].value[1]' <<<"$result"
}

label_of() {
	# label_of <expr> <label> -> value of <label> on the single result series
	# ("<absent>" when the label is not set).
	local result
	result=$(vm_query "$1")
	[[ $("$JQ_BIN" -r 'length' <<<"$result") -eq 1 ]] || {
		printf 'expected exactly one series for %s, got: %s\n' "$1" "$result" >&2
		return 1
	}
	"$JQ_BIN" -r --arg l "$2" '.[0].metric[$l] // "<absent>"' <<<"$result"
}

approx() {
	# approx <actual> <expected> <tolerance> -> 0 when |a-e| <= tol
	"$JQ_BIN" -en --argjson a "$1" --argjson e "$2" --argjson t "$3" '(($a-$e)|fabs) <= $t' >/dev/null
}

wait_for_count() {
	# wait_for_count <description> <count-expr> <want> [timeout]
	local desc=$1 expr=$2 want=$3 deadline=$((SECONDS + ${4:-$TIMEOUT_SECONDS})) got=
	while :; do
		got=$(count_value "$expr" 2>/dev/null || echo "?")
		if [[ $got == "$want" ]]; then
			printf 'ok   %s (count %s)\n' "$desc" "$got"
			return 0
		fi
		if ((SECONDS >= deadline)); then
			printf 'FAIL %s: want count %s, got %s after %ss: %s\n' "$desc" "$want" "$got" "${4:-$TIMEOUT_SECONDS}" "$expr" >&2
			return 1
		fi
		sleep 10
	done
}

wait_until() {
	# wait_until <description> <timeout> <command...>: polls the command
	# (its stdout is the last observed value, shown on failure) until it
	# succeeds or the deadline passes.
	local desc=$1 limit=$2 deadline out=
	shift 2
	deadline=$((SECONDS + limit))
	while :; do
		if out=$("$@" 2>/dev/null); then
			printf 'ok   %s\n' "$desc"
			return 0
		fi
		if ((SECONDS >= deadline)); then
			printf 'FAIL %s after %ss (last: %s)\n' "$desc" "$limit" "${out:-<no output>}" >&2
			return 1
		fi
		sleep 5
	done
}

assert_count() {
	# assert_count <description> <count-expr> <want>: the value of a
	# count(...) query, 0 when it returns nothing.
	local got
	got=$(count_value "$2" || echo "?")
	assert_true "$1 (want count $3, got $got)" test "$got" = "$3"
}

assert_absent() {
	# assert_absent <description> <expr>: no series at all, and not a 0.
	local got
	got=$(series_count "$2" || echo "?")
	assert_true "$1 is absent (got $got series)" test "$got" = 0
}

assert_value() {
	# assert_value <description> <expr> <expected> <tolerance>
	local got
	if got=$(scalar_value "$2"); then
		assert_true "$1 = $got (want $3 ± $4)" approx "$got" "$3" "$4"
	else
		check "$1 (no single value)" 1
	fi
}

assert_label() {
	# assert_label <description> <expr> <label> <expected>
	local got
	got=$(label_of "$2" "$3" 2>/dev/null || echo "<error>")
	assert_true "$1: $3=$got (want $4)" test "$got" = "$4"
}

assert_eq() {
	# assert_eq <description> <got> <want>
	assert_true "$1 (got $2, want $3)" test "$2" = "$3"
}

# -- sink helpers -------------------------------------------------------------
sink_requests() {
	# sink_requests <namespace> <service> <port> -> the sink's recorded
	# requests as a JSON array.
	svc_proxy "$1" "$2:$3" /requests | "$JQ_BIN" -c '.requests'
}

human_sink() { sink_requests "$PIPELINE_NAMESPACE" alert-sink-human 8080; }
controller_sink() { sink_requests "${CONTRACT[controller_namespace]}" "${CONTRACT[controller_service]}" "${CONTRACT[controller_port]}"; }

alerts_matching() {
	# alerts_matching <requests-json> <jq-filter-on-labels> -> number of
	# delivered alerts (across all recorded requests) whose labels match.
	"$JQ_BIN" -r '[.[] | .body.alerts[]? | select(.labels | ('"$2"'))] | length' <<<"$1"
}

sink_has_alert() {
	# sink_has_alert <sink-fn> <jq-filter-on-labels> -> prints the count,
	# succeeds when >= 1 (for wait_until).
	local n
	n=$(alerts_matching "$("$1")" "$2") || return 1
	printf '%s' "$n"
	[[ $n -ge 1 ]]
}

am_proxy() { svc_proxy "$PIPELINE_NAMESPACE" alertmanager:http "$1"; }

am_alerts() {
	# am_alerts <alertname> -> Alertmanager API v2 alerts with that name.
	local encoded
	encoded=$("$JQ_BIN" -rn --arg q "alertname=\"$1\"" '$q|@uri')
	am_proxy "/api/v2/alerts?filter=$encoded"
}

am_metric_sum() {
	# am_metric_sum <metric-name> [label-substring] -> sum of the samples of
	# that metric family (those whose label set contains the substring, when
	# one is given) in Alertmanager's own /metrics; 0 when none.
	am_proxy /metrics | awk -v m="$1" -v f="${2:-}" '$1 ~ ("^" m "([{]|$)") && (f == "" || index($1, f) > 0) { s += $2 } END { printf "%d\n", s }'
}

post_probe_alert() {
	# post_probe_alert <probe-id> <extra-label-json-object>: injects one
	# alert straight into Alertmanager's API (never through a rule), tagged
	# so the sinks can be asked about it by name.
	local file="$work_dir/probe-$1.json"
	"$JQ_BIN" -n --arg name "$PROBE_ALERTNAME" --arg id "$1" --arg node "$2" --argjson extra "$3" \
		'[{labels: ({alertname: $name, fixture_probe: $id, node: $node, UUID: ("fixture-probe-" + $id)} + $extra),
		   annotations: {summary: "kind fixture routing probe; API-injected, not a rule, acts on nothing"},
		   generatorURL: "http://kind-gpu-observability.fixture/probe"}]' >"$file"
	pod_post http://alertmanager:9093/api/v2/alerts "$file" >/dev/null
}

# -- Grafana helpers ----------------------------------------------------------
gf_get() { svc_proxy "$PIPELINE_NAMESPACE" grafana:http "$1"; }
gf_post() { pod_post "http://grafana:3000$1" "$2"; }

gf_panel_query() {
	# gf_panel_query <post-fn> <query-json-file> <stderr-file>: one
	# /api/ds/query POST, judged on its stdout alone. The transport's stderr
	# (kubectl exec can print a nonfatal "websocket.go: Unknown stream id"
	# warning on a successful call) goes to <stderr-file>, never into the
	# JSON that jq parses. Fails closed on a nonzero post, a body that is
	# not JSON, a per-result non-2xx status or a per-result error, and then
	# prints a bounded diagnostic: the reason, the first 400 bytes of the
	# body and the last 400 bytes of stderr (anonymous Grafana, no
	# credential travels either way). <post-fn> is gf_post in a cluster run
	# and a stub in verify_offline.
	local post=$1 query=$2 errfile=$3 response reason
	if ! response=$("$post" /api/ds/query "$query" 2>"$errfile"); then
		reason="post exited nonzero"
	elif ! "$JQ_BIN" -e 'type == "object"' <<<"$response" >/dev/null 2>&1; then
		reason="response body is not a JSON object"
	elif ! "$JQ_BIN" -e 'all(.results[]; (.error // "") == "" and ((.status // 200) | tostring | startswith("2")))' <<<"$response" >/dev/null 2>&1; then
		reason="a result carries an error or a non-2xx status"
	else
		return 0
	fi
	printf '     %s\n     body: %s\n     stderr: %s\n' "$reason" "${response:0:400}" "$(tail -c 400 "$errfile" | tr '\n' ' ')" >&2
	return 1
}

gf_dashboard_count() {
	local n
	n=$(gf_get '/api/search?type=dash-db&limit=100' | "$JQ_BIN" -r 'length') || return 1
	printf '%s' "$n"
	[[ $n -eq ${#DASHBOARDS[@]} ]]
}

# ---------------------------------------------------------------------------
# Cluster run.
# ---------------------------------------------------------------------------
run_cluster() {
	require_cmd KIND_BIN "$KIND_BIN"
	require_cmd KUBECTL_BIN "$KUBECTL_BIN"
	require_cmd DOCKER_BIN "$DOCKER_BIN"
	require_cmd JQ_BIN "$JQ_BIN"
	require_cmd GO_BIN "$GO_BIN"
	require_cmd PYTHON_BIN "$PYTHON_BIN"
	command -v timeout >/dev/null 2>&1 || fail "coreutils timeout is required"
	[[ -n $SHADOW_FOR_SECONDS && -n ${CONTRACT[controller_service]:-} ]] || fail "offline verification did not establish the shadow trigger and controller contract"

	local kind_version
	kind_version=$("$KIND_BIN" version | awk '{print $2}')
	[[ $kind_version == "$EXPECTED_KIND_VERSION" ]] || fail "kind $EXPECTED_KIND_VERSION required, got $kind_version"
	"$DOCKER_BIN" info >/dev/null 2>&1 || fail "docker is not reachable (try: sg docker -c '$0')"

	# Never touch a cluster this run did not create.
	if "$KIND_BIN" get clusters 2>/dev/null | grep -qx -- "$CLUSTER_NAME"; then
		fail "kind cluster '$CLUSTER_NAME' already exists; delete it or set CLUSTER_NAME to a new name"
	fi
	[[ $CLUSTER_NAME =~ ^[a-z0-9][a-z0-9-]{0,40}$ ]] || fail "CLUSTER_NAME '$CLUSTER_NAME' is not a valid kind cluster name"

	work_dir=$(mktemp -d "${TMPDIR:-/tmp}/kind-gpu-observability.${RUN_ID}.XXXXXX")
	KUBECONFIG_PATH="$work_dir/kubeconfig"
	trap cleanup EXIT

	note "building fixture image $FIXTURE_IMAGE (FROM scratch, host-built static exporter + recorder sink)"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO_BIN" build -trimpath -ldflags='-s -w' -o "$work_dir/dcgmfixture" ./hack/dcgmfixture)
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO_BIN" build -trimpath -ldflags='-s -w' -o "$work_dir/alertsink" ./hack/alertsink)
	mkdir -p "$work_dir/image"
	cp "$work_dir/dcgmfixture" "$work_dir/alertsink" "$FIXTURE_DIR/Dockerfile" "$FIXTURE_DIR/Dockerfile.dockerignore" "$work_dir/image/"
	"$DOCKER_BIN" build --quiet --pull=false -t "$FIXTURE_IMAGE" -f "$work_dir/image/Dockerfile" "$work_dir/image" >/dev/null
	built_image=1

	note "creating kind cluster $CLUSTER_NAME (1 control plane + 2 workers, $NODE_IMAGE)"
	cat >"$work_dir/kind.yaml" <<KIND
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
KIND
	created_cluster=1
	timeout 600 "$KIND_BIN" create cluster --name "$CLUSTER_NAME" --image "$NODE_IMAGE" \
		--config "$work_dir/kind.yaml" --kubeconfig "$KUBECONFIG_PATH" --wait 180s
	local server_version
	server_version=$(kc version -o json | "$JQ_BIN" -r '.serverVersion.gitVersion')
	[[ $server_version == "$EXPECTED_SERVER_VERSION" ]] || fail "server $EXPECTED_SERVER_VERSION expected, got $server_version"

	local -a workers
	mapfile -t workers < <(kc get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort)
	[[ ${#workers[@]} -eq 2 ]] || fail "expected 2 worker nodes, got ${#workers[@]}"
	local node_a=${workers[0]} node_b=${workers[1]}
	note "node A (node-pool labels): $node_a; node B (no node-pool label): $node_b"
	# Both the fallback and the EKS label on node A: the contract says EKS wins.
	kc label node "$node_a" "kubeneuron.io/node-pool=$NODE_POOL_FALLBACK" "eks.amazonaws.com/nodegroup=$NODE_POOL_EKS" >/dev/null

	timeout 300 "$KIND_BIN" load docker-image --name "$CLUSTER_NAME" "$FIXTURE_IMAGE"

	local ns
	for ns in "$FIXTURE_NAMESPACE" "$PIPELINE_NAMESPACE" "${CONTRACT[controller_namespace]}"; do
		if kc get namespace "$ns" >/dev/null 2>&1; then
			fail "namespace $ns already exists in the new cluster; refusing to overwrite"
		fi
		kc create namespace "$ns" >/dev/null
	done

	note "applying fixture exporters, vmsingle, vmagent (shipped scrape contract), vmalert (shipped recording + shadow rules), Alertmanager (shipped route tree), recorder sinks and Grafana"
	render_fixture_manifest "$work_dir" "$FIXTURE_IMAGE" "$node_a" "$node_b"
	kc apply -n "$FIXTURE_NAMESPACE" -f "$work_dir/fixture.yaml" >/dev/null
	"$PYTHON_BIN" -I "$FIXTURE_DIR/extract-scrape-config.py" "$VMAGENT_CR" >"$work_dir/scrape.yaml"
	kc apply -n "$PIPELINE_NAMESPACE" -f "$FIXTURE_DIR/vmsingle.yaml" >/dev/null
	kc create configmap vmagent-scrape -n "$PIPELINE_NAMESPACE" --from-file=scrape.yaml="$work_dir/scrape.yaml" >/dev/null
	kc create configmap vmalert-rules -n "$PIPELINE_NAMESPACE" --from-file=gpu-recording-rules.yaml="$RULES_FILE" --from-file=gpu-shadow-rules.yaml="$SHADOW_RULES_FILE" >/dev/null
	sed "s/__NAMESPACE__/$PIPELINE_NAMESPACE/g" "$FIXTURE_DIR/vmagent.yaml" | kc apply -n "$PIPELINE_NAMESPACE" -f - >/dev/null
	kc apply -n "$PIPELINE_NAMESPACE" -f "$FIXTURE_DIR/vmalert.yaml" >/dev/null

	# Alertmanager: the config rendered from the checked-in VMAlertmanager, a
	# per-run bearer token where the shipped receiver's credentials_file
	# expects it, and the two recorder sinks (the controller one under the
	# shipped receiver's own Service name, namespace and port).
	render_alertmanager "$work_dir" || fail "could not render the Alertmanager config"
	local fixture_token="fixture-token-$RUN_ID"
	kc create secret generic "${CONTRACT[credentials_secret]}" -n "$PIPELINE_NAMESPACE" --from-literal="${CONTRACT[credentials_key]}=$fixture_token" >/dev/null
	kc create configmap alertmanager-config -n "$PIPELINE_NAMESPACE" --from-file=alertmanager.yaml="$work_dir/alertmanager.yaml" >/dev/null
	kc apply -n "$PIPELINE_NAMESPACE" -f "$work_dir/alertmanager-deploy.yaml" >/dev/null
	kc apply -f "$work_dir/alert-sinks.yaml" >/dev/null

	# Grafana: provisioning files from the fixture, boards from deploy/grafana
	# with the datasource placeholder bound.
	render_dashboards "$work_dir" || fail "could not render the dashboards"
	kc create configmap grafana-provisioning-datasources -n "$PIPELINE_NAMESPACE" --from-file=datasources.yaml="$FIXTURE_DIR/grafana-datasources.yaml" >/dev/null
	kc create configmap grafana-provisioning-dashboards -n "$PIPELINE_NAMESPACE" --from-file=dashboards.yaml="$FIXTURE_DIR/grafana-dashboards-provider.yaml" >/dev/null
	kc create configmap grafana-dashboards -n "$PIPELINE_NAMESPACE" --from-file="$work_dir/dashboards" >/dev/null
	kc apply -n "$PIPELINE_NAMESPACE" -f "$FIXTURE_DIR/grafana.yaml" >/dev/null

	local d
	for d in dcgm-fixture-a dcgm-fixture-b; do
		kc rollout status -n "$FIXTURE_NAMESPACE" "deployment/$d" --timeout="${TIMEOUT_SECONDS}s"
	done
	for d in vmsingle vmagent vmalert alertmanager alert-sink-human; do
		kc rollout status -n "$PIPELINE_NAMESPACE" "deployment/$d" --timeout="${TIMEOUT_SECONDS}s"
	done
	kc rollout status -n "${CONTRACT[controller_namespace]}" deployment/alert-sink-controller --timeout="${TIMEOUT_SECONDS}s"
	# The first run pulls the Grafana image; allow it twice the usual wait.
	kc rollout status -n "$PIPELINE_NAMESPACE" deployment/grafana --timeout="$((TIMEOUT_SECONDS * 2))s"

	note "fixture exposition sanity (straight from the exporter pod, before any pipeline)"
	local exposition
	exposition=$(kc get --raw "/api/v1/namespaces/$FIXTURE_NAMESPACE/services/nvidia-dcgm-exporter:gpu-metrics/proxy/metrics")
	assert_true "fixture Service serves DCGM_FI_* exposition" grep -q '^DCGM_FI_DEV_GPU_TEMP{' <<<"$exposition"
	if grep -Eq 'DCGM_FI_DEV_GPU_TEMP_LIMIT|REMAPPED_ROWS|ROW_REMAP|NVLINK' <<<"$exposition"; then
		check "exposition omits 153, 393-396 and NVLink fields" 1
	else
		check "exposition omits 153, 393-396 and NVLink fields" 0
	fi

	note "vmalert loaded the shipped recording and shadow rules without errors"
	local rules groups errored alerting shadow_named shadow_labelled alerting_in_recording alerting_files
	rules=$(vm_proxy vmalert:http "/api/v1/rules")
	groups=$("$JQ_BIN" -r '.data.groups | length' <<<"$rules")
	errored=$("$JQ_BIN" -r '[.data.groups[].rules[] | select(.lastError != "" and .lastError != null)] | length' <<<"$rules")
	alerting=$("$JQ_BIN" -r '[.data.groups[].rules[] | select(.type=="alerting")] | length' <<<"$rules")
	shadow_named=$("$JQ_BIN" -r '[.data.groups[].rules[] | select(.type=="alerting") | select(.name | startswith("ShadowGpu"))] | length' <<<"$rules")
	shadow_labelled=$("$JQ_BIN" -r --arg l "$SHADOW_LABEL" '[.data.groups[].rules[] | select(.type=="alerting") | select(.labels[$l] == "true")] | length' <<<"$rules")
	alerting_in_recording=$("$JQ_BIN" -r '[.data.groups[] | select(.file | endswith("gpu-recording-rules.yaml")) | .rules[] | select(.type=="alerting")] | length' <<<"$rules")
	alerting_files=$("$JQ_BIN" -r '[.data.groups[] | select(any(.rules[]; .type=="alerting")) | .file] | unique | join(",")' <<<"$rules")
	assert_true "vmalert reports $groups rule groups (want >= 11)" test "$groups" -ge 11
	assert_true "no rule reports lastError ($errored do)" test "$errored" -eq 0
	assert_eq "recording rule file holds no alerting rule" "$alerting_in_recording" 0
	assert_eq "alerting rules loaded equal the shadow file's rule count" "$alerting" "$SHADOW_RULE_COUNT"
	assert_eq "every alerting rule is named ShadowGpu*" "$shadow_named" "$alerting"
	assert_eq "every alerting rule carries $SHADOW_LABEL=\"true\"" "$shadow_labelled" "$alerting"
	assert_true "alerting rules come only from gpu-shadow-rules.yaml ($alerting_files)" test "$alerting_files" = "/etc/vmalert/rules/gpu-shadow-rules.yaml"

	# -- Source series through the shipped scrape contract --------------------
	note "waiting for source series (scrape interval 30s)"
	wait_for_count "source series DCGM_FI_DEV_GPU_TEMP for every device" 'count(DCGM_FI_DEV_GPU_TEMP)' "$TOTAL_DEVICES" || failures=$((failures + 1))
	assert_count "gpu_uuid/gpu_model copied from UUID/modelName on every source series" \
		'count(DCGM_FI_DEV_GPU_TEMP{gpu_uuid!="",gpu_model!="",UUID!="",modelName!=""})' "$TOTAL_DEVICES"
	assert_count "node label on node A sources" "count(DCGM_FI_DEV_GPU_TEMP{node=\"$node_a\"})" "$NODE_A_DEVICES"
	assert_count "node label on node B sources" "count(DCGM_FI_DEV_GPU_TEMP{node=\"$node_b\"})" 1
	assert_count "exporter workload labels flow through (pod/namespace/container)" \
		'count(DCGM_FI_DEV_GPU_TEMP{pod="train-0",namespace="ml-fixture",container="trainer"})' 1
	assert_absent "DCGM_FI_DEV_GPU_TEMP_LIMIT (153) source" 'DCGM_FI_DEV_GPU_TEMP_LIMIT'
	assert_absent "mig_profile on non-MIG fixture" 'DCGM_FI_DEV_GPU_TEMP{mig_profile!=""}'

	# -- Canonical series --------------------------------------------------------
	note "waiting for canonical series (rule interval 30s)"
	wait_for_count "kubeneuron_gpu_temperature_celsius per device" 'count(kubeneuron_gpu_temperature_celsius)' "$TOTAL_DEVICES" || failures=$((failures + 1))
	assert_count "kubeneuron_gpu_power_watts per device" 'count(kubeneuron_gpu_power_watts)' "$TOTAL_DEVICES"
	assert_count "kubeneuron_gpu_memory_used_bytes per device" 'count(kubeneuron_gpu_memory_used_bytes)' "$TOTAL_DEVICES"
	local t4_slow="{gpu_uuid=\"$T4_SLOW_UUID\"}" t4_gpu0="{gpu_uuid=\"$T4_GPU0_UUID\"}"
	assert_label "canonical temperature carries gpu_model" "kubeneuron_gpu_temperature_celsius$t4_slow" gpu_model "Tesla T4"
	assert_label "canonical temperature keeps exporter UUID" "kubeneuron_gpu_temperature_celsius$t4_slow" UUID "$T4_SLOW_UUID"
	assert_label "canonical temperature keeps exporter modelName" "kubeneuron_gpu_temperature_celsius$t4_slow" modelName "Tesla T4"
	assert_label "canonical temperature keeps exporter gpu index" "kubeneuron_gpu_temperature_celsius$t4_slow" gpu 6
	assert_label "canonical temperature carries node" "kubeneuron_gpu_temperature_celsius$t4_slow" node "$node_a"
	assert_label "idle device has no pod label (empty exporter value dropped)" "kubeneuron_gpu_temperature_celsius$t4_slow" pod "<absent>"
	assert_label "held device keeps pod label" "kubeneuron_gpu_temperature_celsius$t4_gpu0" pod train-0
	assert_label "held device keeps container label" "kubeneuron_gpu_temperature_celsius$t4_gpu0" container trainer
	assert_value "MiB -> bytes: kubeneuron_gpu_memory_used_bytes for gpu 0" "kubeneuron_gpu_memory_used_bytes$t4_gpu0" $((FB_USED_GPU0_MIB * 1048576)) 0
	assert_value "MiB -> bytes agrees with the source arithmetically" \
		"kubeneuron_gpu_memory_used_bytes$t4_gpu0 - on(gpu_uuid) DCGM_FI_DEV_FB_USED$t4_gpu0 * 1048576" 0 0
	assert_value "temperature passes through unscaled (gpu 0)" "kubeneuron_gpu_temperature_celsius$t4_gpu0" "$GPU_TEMP_GPU0" 0

	# -- node_pool and cluster ----------------------------------------------------
	note "optional placement labels"
	assert_count "node_pool present on every node A series, EKS nodegroup wins over fallback" \
		"count(kubeneuron_gpu_temperature_celsius{node=\"$node_a\",node_pool=\"$NODE_POOL_EKS\"})" "$NODE_A_DEVICES"
	assert_absent "node_pool with the lower-precedence fallback value" "kubeneuron_gpu_temperature_celsius{node_pool=\"$NODE_POOL_FALLBACK\"}"
	assert_absent "node_pool on node B (no node-pool label)" "kubeneuron_gpu_temperature_celsius{node=\"$node_b\",node_pool!=\"\"}"
	assert_count "node B series still present without node_pool" "count(kubeneuron_gpu_temperature_celsius{node=\"$node_b\"})" 1
	assert_absent "cluster label on any source or canonical series by default" '{__name__=~"DCGM_FI_.*|kubeneuron_gpu_.*",cluster!=""}'

	# -- Clock events (bitmask decode) -------------------------------------------
	note "clock-event bitmask decode"
	assert_value "slow T4 sw_thermal event active" "kubeneuron_gpu_clock_event_active{reason=\"sw_thermal\",gpu_uuid=\"$T4_SLOW_UUID\"}" 1 0
	assert_value "slow T4 hw_slowdown event active" "kubeneuron_gpu_clock_event_active{reason=\"hw_slowdown\",gpu_uuid=\"$T4_SLOW_UUID\"}" 1 0
	assert_count "exactly the two slow devices report sw_thermal" 'count(kubeneuron_gpu_clock_event_active{reason="sw_thermal"} == 1)' 2
	assert_count "every device reports hw_thermal = 0 (present, not absent)" 'count(kubeneuron_gpu_clock_event_active{reason="hw_thermal"} == 0)' "$TOTAL_DEVICES"

	# -- Derived series --------------------------------------------------------------
	note "waiting for derived series (rate windows need two canonical samples)"
	wait_for_count "kubeneuron_gpu_throttle_share_ratio{reason=power} per device" \
		'count(kubeneuron_gpu_throttle_share_ratio{reason="power"})' "$TOTAL_DEVICES" || failures=$((failures + 1))
	assert_value "slow T4 power throttle share" "kubeneuron_gpu_throttle_share_ratio{reason=\"power\",gpu_uuid=\"$T4_SLOW_UUID\"}" "$SLOW_THROTTLE_SHARE" 0.02
	assert_value "healthy T4 power throttle share is 0, not absent" "kubeneuron_gpu_throttle_share_ratio{reason=\"power\",gpu_uuid=\"$T4_GPU0_UUID\"}" 0 0
	assert_value "slow T4 SM clock ratio" "kubeneuron_gpu_clock_ratio{domain=\"sm\",gpu_uuid=\"$T4_SLOW_UUID\"}" "$("$JQ_BIN" -n "$SLOW_SM_CLOCK / $MAX_SM_CLOCK")" 0.0001
	# gpu-derived-thermal is its own rule group on its own evaluation clock;
	# the throttle-share wait above proves gpu-derived-power caught up, not
	# that this group did (2026-10-18: 8 of 9 devices, one evaluation behind).
	# Kept on one line: verify_offline's regression check reads this call.
	wait_for_count "thermal headroom falls back to slowdown - temperature on every device" 'count(kubeneuron_gpu_thermal_headroom_celsius)' "$TOTAL_DEVICES" || failures=$((failures + 1))
	assert_value "thermal headroom value for gpu 0" "kubeneuron_gpu_thermal_headroom_celsius$t4_gpu0" $((SLOWDOWN_TEMP - GPU_TEMP_GPU0)) 0
	assert_count "memory_used_ratio per device" 'count(kubeneuron_gpu_memory_used_ratio)' "$TOTAL_DEVICES"
	assert_value "memory_used_ratio for gpu 0" "kubeneuron_gpu_memory_used_ratio$t4_gpu0" "$("$JQ_BIN" -n "$FB_USED_GPU0_MIB / 15360")" 0.0001
	# 1507 is present on every device: a literal 0 on all but the one the
	# spec marks unrepairable, which is the shadow-routing input below.
	assert_count "hard-fault component from a present zero source is 0, not absent" 'count(kubeneuron_gpu_memory_hard_fault_active{component="unrepairable"} == 0)' $((TOTAL_DEVICES - 1))
	assert_count "unrepairable component is 1 on exactly one device" 'count(kubeneuron_gpu_memory_hard_fault_active{component="unrepairable"} == 1)' 1
	assert_label "the unrepairable device is Tesla T4 gpu 2" 'kubeneuron_gpu_memory_hard_fault_active{component="unrepairable"} == 1' gpu_uuid "$T4_UNREPAIRABLE_UUID"

	# -- Omitted sources stay absent -------------------------------------------------
	note "omitted sources (153, 393-396, NVLink) must give absent series, never 0"
	assert_absent "kubeneuron_gpu_thermal_margin_celsius (153)" 'kubeneuron_gpu_thermal_margin_celsius'
	assert_absent "kubeneuron_gpu_remapped_rows_total (393/394)" 'kubeneuron_gpu_remapped_rows_total'
	assert_absent "kubeneuron_gpu_row_remap_failed (395)" 'kubeneuron_gpu_row_remap_failed'
	assert_absent "kubeneuron_gpu_row_remap_pending (396)" 'kubeneuron_gpu_row_remap_pending'
	assert_absent "kubeneuron_gpu_nvlink_errors_total (409+)" 'kubeneuron_gpu_nvlink_errors_total'
	assert_absent "kubeneuron_gpu_nvlink_errors_per_second (derived from 409+)" 'kubeneuron_gpu_nvlink_errors_per_second'
	assert_absent "hard-fault components derived from 393/395/396" \
		'kubeneuron_gpu_memory_hard_fault_active{component=~"uncorrectable_row_remap|row_remap_failed|row_remap_pending"}'
	assert_count "hard-fault series exist only for present components (4 of 7 per device)" 'count(kubeneuron_gpu_memory_hard_fault_active)' $((TOTAL_DEVICES * 4))

	# -- Fail-slow -------------------------------------------------------------------
	note "waiting for the fail-slow score"
	wait_for_count "exactly one kubeneuron_gpu_fail_slow_score_ratio series" 'count(kubeneuron_gpu_fail_slow_score_ratio)' 1 "$((TIMEOUT_SECONDS + 120))" || failures=$((failures + 1))
	assert_label "score belongs to the slow T4" 'kubeneuron_gpu_fail_slow_score_ratio' gpu_uuid "$T4_SLOW_UUID"
	assert_label "score carries gpu_model" 'kubeneuron_gpu_fail_slow_score_ratio' gpu_model "Tesla T4"
	assert_label "score carries node" 'kubeneuron_gpu_fail_slow_score_ratio' node "$node_a"
	assert_value "score value 1 - slow/median" 'kubeneuron_gpu_fail_slow_score_ratio' "$("$JQ_BIN" -n "1 - $SLOW_SM_CLOCK / $MAX_SM_CLOCK")" 0.001
	assert_count "Tesla T4 peer group has 7 devices" 'count(count by (gpu_uuid) (kubeneuron_gpu_clock_ratio{domain="sm",gpu_model="Tesla T4"}))' 7
	assert_absent "no score for any healthy T4" "kubeneuron_gpu_fail_slow_score_ratio{gpu_model=\"Tesla T4\",gpu_uuid!=\"$T4_SLOW_UUID\"}"
	# The A10G pair: one slow device that passes the deviation and throttle
	# floors on its own, blocked only by the three-peer floor.
	assert_count "NVIDIA A10G peer group has 2 devices" 'count(count by (gpu_uuid) (kubeneuron_gpu_clock_ratio{domain="sm",gpu_model="NVIDIA A10G"}))' 2
	assert_count "slow A10G would pass the 15% deviation floor against its pair" \
		'count((1 - max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_clock_ratio{domain="sm",gpu_model="NVIDIA A10G"}) / on (gpu_model) group_left() (median by (gpu_model) (max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_clock_ratio{domain="sm",gpu_model="NVIDIA A10G"})) > 0)) >= 0.15)' 1
	assert_count "slow A10G passes the throttle floor" "count(kubeneuron_gpu_throttle_share_ratio{gpu_uuid=\"$A10G_SLOW_UUID\"} >= 0.05)" 1
	assert_absent "score for the two-device A10G group" 'kubeneuron_gpu_fail_slow_score_ratio{gpu_model="NVIDIA A10G"}'

	# -- Shadow routing -------------------------------------------------------------
	# The unrepairable device has satisfied the trigger rule since the first
	# canonical sample; vmalert fires it once the rule's own `for` has held,
	# then Alertmanager (shipped route tree) must deliver it to the human
	# sink and only there. The controller sink is live and reachable under
	# the shipped receiver's URL the whole time; the probes at the end prove
	# that, so its zero is a routing result.
	note "shadow routing: waiting for $SHADOW_TRIGGER_ALERT to fire (rule holds for ${SHADOW_FOR_SECONDS}s, measured from the first canonical sample)"
	local shadow_wait=$((SHADOW_FOR_SECONDS + 240))
	local trigger_filter=".alertname == \"$SHADOW_TRIGGER_ALERT\" and .$SHADOW_LABEL == \"true\""
	wait_for_count "vmalert records ALERTS{$SHADOW_TRIGGER_ALERT, firing} in vmsingle" \
		"count(ALERTS{alertname=\"$SHADOW_TRIGGER_ALERT\",alertstate=\"firing\"})" 1 "$shadow_wait" || failures=$((failures + 1))
	assert_count "exactly one ShadowGpu* alert is firing (the input triggers exactly one rule)" 'count(ALERTS{alertname=~"ShadowGpu.*",alertstate="firing"})' 1
	# The fail-slow score also satisfies ShadowGpuFailSlow, whose 30m hold is
	# longer than this run: it must be pending, never firing, here.
	wait_for_count "ShadowGpuFailSlow is pending on the fail-slow score (30m hold, so never firing in this run)" \
		'count(ALERTS{alertname="ShadowGpuFailSlow",alertstate="pending"})' 1 120 || failures=$((failures + 1))
	wait_until "human sink received $SHADOW_TRIGGER_ALERT (group_wait 15s + delivery)" 180 sink_has_alert human_sink "$trigger_filter" || failures=$((failures + 1))

	local human_reqs controller_reqs delivered
	human_reqs=$(human_sink)
	delivered=$("$JQ_BIN" -c --arg a "$SHADOW_TRIGGER_ALERT" '[.[] | select(.body.alerts[]?.labels.alertname == $a)] | first // empty' <<<"$human_reqs")
	if [[ -n $delivered ]]; then
		assert_eq "delivery names the receiver" "$("$JQ_BIN" -r '.body.receiver' <<<"$delivered")" "$HUMAN_RECEIVER"
		assert_eq "delivery path is the human sink path" "$("$JQ_BIN" -r '.path' <<<"$delivered")" "$HUMAN_SINK_PATH"
		assert_eq "delivered alert status" "$("$JQ_BIN" -r '.body.status' <<<"$delivered")" firing
		assert_eq "delivered alert names the unrepairable device" "$("$JQ_BIN" -r --arg a "$SHADOW_TRIGGER_ALERT" '[.body.alerts[] | select(.labels.alertname == $a) | .labels.gpu_uuid] | unique | join(",")' <<<"$delivered")" "$T4_UNREPAIRABLE_UUID"
		assert_eq "delivered alert names the component" "$("$JQ_BIN" -r --arg a "$SHADOW_TRIGGER_ALERT" '[.body.alerts[] | select(.labels.alertname == $a) | .labels.component] | unique | join(",")' <<<"$delivered")" unrepairable
		assert_eq "delivered alert severity is info" "$("$JQ_BIN" -r --arg a "$SHADOW_TRIGGER_ALERT" '[.body.alerts[] | select(.labels.alertname == $a) | .labels.severity] | unique | join(",")' <<<"$delivered")" info
		assert_true "delivered annotation is the shadow wording" "$JQ_BIN" -e --arg a "$SHADOW_TRIGGER_ALERT" 'all(.body.alerts[] | select(.labels.alertname == $a); .annotations.summary | startswith("Shadow:"))' <<<"$delivered" >/dev/null
	else
		check "delivery payload for $SHADOW_TRIGGER_ALERT present on the human sink" 1
	fi
	local am_view
	am_view=$(am_alerts "$SHADOW_TRIGGER_ALERT" || echo '[]')
	assert_eq "Alertmanager holds exactly one active $SHADOW_TRIGGER_ALERT" "$("$JQ_BIN" -r 'length' <<<"$am_view")" 1
	assert_eq "Alertmanager resolved its receivers to $HUMAN_RECEIVER only" "$("$JQ_BIN" -r '[.[].receivers[].name] | unique | join(",")' <<<"$am_view")" "$HUMAN_RECEIVER"
	assert_eq "Alertmanager alert state" "$("$JQ_BIN" -r '[.[].status.state] | unique | join(",")' <<<"$am_view")" active
	controller_reqs=$(controller_sink)
	assert_eq "controller webhook sink recorded exactly zero requests while the shadow alert fired" "$("$JQ_BIN" -r 'length' <<<"$controller_reqs")" 0
	assert_eq "Alertmanager reports zero failed notifications (so nothing was attempted and lost)" "$(am_metric_sum alertmanager_notifications_failed_total)" 0
	assert_true "Alertmanager reports at least one webhook notification attempt" test "$(am_metric_sum alertmanager_notifications_total 'integration="webhook"')" -ge 1

	note "shadow routing: API-injected probes (never rules) prove both sinks and the route order are live"
	post_probe_alert shadow-route "$node_a" "{\"$SHADOW_LABEL\": \"true\"}" || check "post shadow-labelled probe to Alertmanager" 1
	wait_until "shadow-labelled Gpu*-named probe reached the human sink" 120 sink_has_alert human_sink '.fixture_probe == "shadow-route"' || failures=$((failures + 1))
	controller_reqs=$(controller_sink)
	assert_eq "controller sink still has zero requests after the shadow-labelled probe" "$("$JQ_BIN" -r 'length' <<<"$controller_reqs")" 0
	post_probe_alert controller-route "$node_a" '{}' || check "post plain Gpu*-named probe to Alertmanager" 1
	wait_until "plain Gpu*-named probe reached the controller sink" 120 sink_has_alert controller_sink '.fixture_probe == "controller-route"' || failures=$((failures + 1))
	controller_reqs=$(controller_sink)
	human_reqs=$(human_sink)
	assert_eq "controller sink recorded exactly one request in total (the probe)" "$("$JQ_BIN" -r 'length' <<<"$controller_reqs")" 1
	assert_eq "controller sink request used the shipped webhook path" "$("$JQ_BIN" -r '[.[].path] | unique | join(",")' <<<"$controller_reqs")" "${CONTRACT[controller_path]}"
	assert_eq "controller sink request carried the bearer token from the shipped credentials_file" "$("$JQ_BIN" -r '[.[].authorization] | unique | join(",")' <<<"$controller_reqs")" "Bearer $fixture_token"
	assert_eq "controller sink delivery names the receiver" "$("$JQ_BIN" -r '[.[].body.receiver] | unique | join(",")' <<<"$controller_reqs")" "$CONTROLLER_RECEIVER"
	assert_eq "controller sink saw no alert with the shadow label" "$(alerts_matching "$controller_reqs" ".$SHADOW_LABEL == \"true\"")" 0
	assert_eq "controller sink saw no ShadowGpu* alert" "$(alerts_matching "$controller_reqs" '.alertname | startswith("ShadowGpu")')" 0
	assert_eq "human sink never saw the plain Gpu*-named probe" "$(alerts_matching "$human_reqs" '.fixture_probe == "controller-route"')" 0
	assert_eq "Alertmanager still reports zero failed notifications" "$(am_metric_sum alertmanager_notifications_failed_total)" 0

	# -- Grafana ---------------------------------------------------------------------
	note "Grafana: datasource and provisioned boards (HTTP API through the service proxy)"
	local health ds
	health=$(gf_get /api/health || echo '{}')
	assert_eq "Grafana /api/health database" "$("$JQ_BIN" -r '.database // "?"' <<<"$health")" ok
	assert_eq "Grafana version is the pinned one" "$("$JQ_BIN" -r '.version // "?"' <<<"$health")" "$(grep -o 'image: grafana/grafana:[0-9.]*' "$FIXTURE_DIR/grafana.yaml" | sed 's/.*://')"
	ds=$(gf_get /api/datasources || echo '[]')
	assert_eq "exactly one datasource is provisioned" "$("$JQ_BIN" -r 'length' <<<"$ds")" 1
	assert_eq "datasource uid" "$("$JQ_BIN" -r '.[0].uid' <<<"$ds")" "$GRAFANA_DS_UID"
	assert_eq "datasource type/url" "$("$JQ_BIN" -r '.[0].type + " " + .[0].url' <<<"$ds")" "prometheus http://vmsingle:8428"
	assert_eq "datasource health against vmsingle" "$(gf_get "/api/datasources/uid/$GRAFANA_DS_UID/health" | "$JQ_BIN" -r '.status // "?"' || echo '?')" OK
	wait_until "Grafana lists ${#DASHBOARDS[@]} provisioned boards" "$TIMEOUT_SECONDS" gf_dashboard_count || failures=$((failures + 1))

	local board uid want_title want_panels want_vars got
	local queries_run=0 queries_failed=0 var_queries_run=0 var_queries_failed=0
	for board in "${DASHBOARDS[@]}"; do
		uid=$("$JQ_BIN" -r '.uid' "$DASHBOARD_DIR/$board.json")
		want_title=$("$JQ_BIN" -r '.title' "$DASHBOARD_DIR/$board.json")
		want_panels=$("$JQ_BIN" -r '.panels | length' "$DASHBOARD_DIR/$board.json")
		want_vars=$("$JQ_BIN" -r '.templating.list | length' "$DASHBOARD_DIR/$board.json")
		if ! got=$(gf_get "/api/dashboards/uid/$uid"); then
			check "$board: GET /api/dashboards/uid/$uid" 1
			continue
		fi
		assert_eq "$board: title" "$("$JQ_BIN" -r '.dashboard.title' <<<"$got")" "$want_title"
		assert_eq "$board: provisioned (read-only) board" "$("$JQ_BIN" -r '.meta.provisioned' <<<"$got")" true
		assert_eq "$board: panel count" "$("$JQ_BIN" -r '.dashboard.panels | length' <<<"$got")" "$want_panels"
		assert_eq "$board: variable count" "$("$JQ_BIN" -r '.dashboard.templating.list | length' <<<"$got")" "$want_vars"
		assert_eq "$board: every datasource reference is bound to the provisioned uid" \
			"$("$JQ_BIN" -r '[.dashboard | .. | objects | select(has("datasource")) | .datasource.uid] | unique | join(",")' <<<"$got")" "$GRAFANA_DS_UID"

		# Variable queries: label_values(metric, label) exactly as Grafana's
		# Prometheus datasource issues them, through Grafana's datasource proxy.
		local var_name metric label encoded
		while IFS=$'\t' read -r var_name metric label; do
			var_queries_run=$((var_queries_run + 1))
			encoded=$("$JQ_BIN" -rn --arg q "$metric" '$q|@uri')
			if ! gf_get "/api/datasources/proxy/uid/$GRAFANA_DS_UID/api/v1/label/$label/values?match%5B%5D=$encoded" | "$JQ_BIN" -e '.status == "success"' >/dev/null 2>&1; then
				printf 'FAIL %s: variable %s query label_values(%s, %s)\n' "$board" "$var_name" "$metric" "$label" >&2
				var_queries_failed=$((var_queries_failed + 1))
			fi
		done < <("$JQ_BIN" -r '.templating.list[] | [.name, (.definition | capture("label_values\\((?<m>[^,]+),\\s*(?<l>[^)]+)\\)") | .m, .l)] | @tsv' "$DASHBOARD_DIR/$board.json")

		# Panel queries: every target expression with each variable replaced by
		# its "All" value, executed through Grafana's query API against the
		# synthetic store. A query error (bad syntax, unknown function,
		# datasource mismatch) fails; an empty result does not, because
		# several boards read controller series this fixture never produces.
		local target body
		while IFS= read -r target; do
			queries_run=$((queries_run + 1))
			body=$("$JQ_BIN" -n --argjson t "$target" --arg uid "$GRAFANA_DS_UID" \
				'{from: "now-15m", to: "now", queries: [{refId: ($t.refId // "A"), datasource: {type: "prometheus", uid: $uid}, expr: $t.expr, instant: ($t.instant // false), range: ($t.range // true), format: ($t.format // "time_series"), intervalMs: 30000, maxDataPoints: 300}]}')
			printf '%s' "$body" >"$work_dir/query.json"
			if ! gf_panel_query gf_post "$work_dir/query.json" "$work_dir/query.err" 2>"$work_dir/query.diag"; then
				printf 'FAIL %s: panel %s: %s\n' "$board" "$("$JQ_BIN" -r '.panel' <<<"$target")" "$("$JQ_BIN" -r '.expr' <<<"$target")" >&2
				cat "$work_dir/query.diag" >&2
				queries_failed=$((queries_failed + 1))
			fi
		done < <("$JQ_BIN" -c '
			(.templating.list | map({key: .name, value: (.allValue // ".*")}) | from_entries) as $all
			| .panels[] | .title as $panel | .targets[]? | select(.expr)
			| {panel: $panel, refId, instant, range, format,
			   expr: (.expr | reduce ($all | to_entries[]) as $v (.; gsub("\\$\\{" + $v.key + "\\}|\\$" + $v.key + "\\b"; $v.value)))}' "$DASHBOARD_DIR/$board.json")
	done
	assert_eq "every variable query executed without error ($var_queries_run run)" "$var_queries_failed" 0
	assert_true "every panel expression executed through Grafana without a query error ($queries_run run, $queries_failed failed)" test "$queries_failed" -eq 0 -a "$queries_run" -ge 40

	if ((failures > 0)); then
		fail "$failures assertion(s) failed"
	fi
	note "PASS: synthetic CPU-only fixture; rule, routing and provisioning wiring only, no DCGM/GPU/pager/controller evidence"
}

verify_offline
if ((failures > 0)); then
	fail "$failures static check(s) failed"
fi
if [[ $MODE == verify ]]; then
	note "offline verification passed (no cluster was created)"
	exit 0
fi
run_cluster
