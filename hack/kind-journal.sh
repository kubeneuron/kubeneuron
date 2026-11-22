#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# shellcheck disable=SC2016 # Single-quoted jq programs expand jq, not shell, variables.
# The agent's journal mount on a real kind cluster: one control plane and two
# workers with a synthetic accelerator advertised on each worker. Checks:
#
#   1. upgrade from BASE_REF (default main) to this tree in the documented
#      order (CRDs -> operator -> controller/agent images): the "-agent"
#      DaemonSet keeps its UID, the controller labels each worker
#      kubeneuron.io/journal-dir=run (kind nodes keep the journal in
#      /run/log/journal), and each worker moves from "-agent" to
#      "-agent-detect-journal-run" without holding two agent Pods, or no
#      Ready one, for longer than ROLLOUT_SECONDS;
#   2. one agent Pod per node, and the selector and mount of the variant;
#   3. a node override on one worker moves only that worker: the agent Pod on
#      the other worker keeps its UID;
#   4. an override to "var" on a worker with no /var/log/journal: the Pod
#      cannot start, and after three minutes the controller takes the label
#      off with reason journal-mount-failed;
#   5. spec.agent.kernelLog.journal.enabled=false removes every label and
#      both variants, and "-agent" still has its UID;
#   6. with KERNEL_RECORD=1, a synthetic XID 79 line with this run's own PCI
#      address, written to the kernel log, reaches the unprivileged agent
#      through the journal and opens a new incident for that address on the
#      worker, with XID 79 in its evidence.
#
# CPU-only: the accelerator is a status patch and the kernel record is a
# synthetic line, not a GPU fault.
# Needs docker, kind, kubectl, jq, openssl and git.
set -euo pipefail
REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
KIND_BIN=${KIND_BIN:-kind}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
DOCKER_BIN=${DOCKER_BIN:-docker}
JQ_BIN=${JQ_BIN:-jq}
BASE_REF=${BASE_REF:-main}
CLUSTER_NAME=${CLUSTER_NAME:-kn-journal-$$}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-300}
# How long a worker may hold two agent Pods, or no Ready one, during a move.
ROLLOUT_SECONDS=${ROLLOUT_SECONDS:-120}
# KERNEL_RECORD=1 turns on ReadKMsg in one worker's journald (the kind image
# turns it off) and writes a synthetic XID 79 line to /dev/kmsg. kind nodes
# share the host kernel, so every kind cluster on the host sees the line:
# by default it runs only in CI (CI=true), where the host is disposable.
KERNEL_RECORD=${KERNEL_RECORD:-$([[ ${CI:-} == true ]] && echo 1 || echo 0)}
# The same immutable node image hack/kind-integration.sh pins.
readonly NODE_IMAGE='kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4'
readonly NAME=journal NS=kubeneuron-journal LABEL=kubeneuron.io/journal-dir
readonly AGENTS="app.kubernetes.io/instance=$NAME,app.kubernetes.io/component in (agent,agent-detect-journal-var,agent-detect-journal-run)"
tag=journal-$$

note() { printf 'kind-journal: %s\n' "$*"; }
die() { printf 'kind-journal FAIL: %s\n' "$*" >&2; exit 1; }

umask 077
work=$(mktemp -d "${TMPDIR:-/tmp}/kubeneuron-journal.XXXXXX")
base_tree=
sampler=
pf_pid=
created=0
# collect_diagnostics: on a failed run, save what the CI artifact step would
# otherwise miss, since the cluster is deleted before that step runs.
collect_diagnostics() {
	local dir=${KIND_ARTIFACTS_DIR:-} pod
	[[ -n $dir ]] || return 0
	mkdir -p "$dir" || return 0
	"$KUBECTL_BIN" get nodes -o wide -L "$LABEL" >"$dir/journal-nodes.txt" 2>&1 || true
	"$KUBECTL_BIN" -n "$NS" get kubeneurons,deployments,daemonsets,pods -o wide >"$dir/journal-resources.txt" 2>&1 || true
	"$KUBECTL_BIN" -n "$NS" get daemonsets -o yaml >"$dir/journal-daemonsets.yaml" 2>&1 || true
	"$KUBECTL_BIN" get events --all-namespaces --sort-by=.lastTimestamp >"$dir/journal-events.txt" 2>&1 || true
	for pod in $("$KUBECTL_BIN" -n "$NS" get pods -o name 2>/dev/null); do
		"$KUBECTL_BIN" -n "$NS" logs "$pod" --all-containers --tail=-1 >"$dir/journal-${pod#pod/}.log" 2>&1 || true
	done
	if [[ -n ${workers[0]:-} ]]; then
		"$DOCKER_BIN" exec "${workers[0]}" journalctl -k -q --no-pager -n 200 -o short-monotonic \
			>"$dir/journal-kernel-${workers[0]}.txt" 2>&1 || true
	fi
}
cleanup() {
	local status=$?
	[[ -z $pf_pid ]] || { kill "$pf_pid" 2>/dev/null || true; wait "$pf_pid" 2>/dev/null || true; }
	[[ -z $sampler ]] || { kill "$sampler" 2>/dev/null || true; wait "$sampler" 2>/dev/null || true; }
	if ((status != 0 && created)); then
		"$KUBECTL_BIN" get nodes -L "$LABEL" 2>&1 || true
		"$KUBECTL_BIN" -n "$NS" get daemonsets,pods -o wide 2>&1 || true
		collect_diagnostics
	fi
	if ((created)) && [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
		"$KIND_BIN" delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
	fi
	[[ -z $base_tree ]] || git -C "$REPO_ROOT" worktree remove --force "$base_tree" >/dev/null 2>&1 || true
	"$DOCKER_BIN" rmi "kubeneuron-operator:base-$tag" "kubeneuron-controller:base-$tag" "kubeneuron-agent:base-$tag" \
		"kubeneuron-operator:head-$tag" "kubeneuron-controller:head-$tag" "kubeneuron-agent:head-$tag" >/dev/null 2>&1 || true
	rm -rf -- "$work"
}
trap cleanup EXIT

for command in "$KIND_BIN" "$KUBECTL_BIN" "$DOCKER_BIN" "$JQ_BIN" openssl git; do
	command -v "$command" >/dev/null 2>&1 || die "required command not found: $command"
done
[[ $CLUSTER_NAME =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] || die "unsafe CLUSTER_NAME: $CLUSTER_NAME"

base_commit=$(git -C "$REPO_ROOT" rev-parse --verify --quiet "$BASE_REF^{commit}" || true)
if [[ -z $base_commit ]]; then
	# A shallow CI checkout has no main: fetch only that one commit.
	git -C "$REPO_ROOT" fetch --no-tags --depth=1 origin "$BASE_REF" >/dev/null 2>&1 ||
		die "cannot resolve BASE_REF $BASE_REF"
	base_commit=$(git -C "$REPO_ROOT" rev-parse FETCH_HEAD)
fi
upgrade=1
if git -C "$REPO_ROOT" cat-file -e "$base_commit:internal/controller/journal_labels.go" 2>/dev/null; then
	# The base already splits the agent: there is nothing to upgrade from.
	upgrade=0
	note "BASE_REF $BASE_REF already has the journal variants; installing this tree directly"
fi

build() {
	local tree=$1 which=$2 target
	for target in operator controller agent; do
		"$DOCKER_BIN" build --quiet --target "$target" --tag "kubeneuron-$target:$which-$tag" \
			--file "$tree/build/Dockerfile" "$tree" >/dev/null
	done
}
note "building this tree's images"
build "$REPO_ROOT" head
if ((upgrade)); then
	base_tree="$work/base"
	git -C "$REPO_ROOT" worktree add --detach "$base_tree" "$base_commit" >/dev/null 2>&1
	note "building the images of $BASE_REF ($(git -C "$REPO_ROOT" rev-parse --short "$base_commit"))"
	build "$base_tree" base
fi

printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n- role: worker\n- role: worker\n' >"$work/kind.yaml"
"$KIND_BIN" create cluster --name "$CLUSTER_NAME" --image "$NODE_IMAGE" --config "$work/kind.yaml" \
	--kubeconfig "$work/kubeconfig" --wait "${TIMEOUT_SECONDS}s" >/dev/null 2>&1
created=1
export KUBECONFIG="$work/kubeconfig"
images=()
for which in head ${base_tree:+base}; do
	for target in operator controller agent; do images+=("kubeneuron-$target:$which-$tag"); done
done
"$KIND_BIN" load docker-image --name "$CLUSTER_NAME" "${images[@]}" >/dev/null 2>&1
mapfile -t workers < <("$KUBECTL_BIN" get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort)
((${#workers[@]} == 2)) || die "want two workers, got ${#workers[@]}"
for node in "${workers[@]}"; do
	"$KUBECTL_BIN" patch node "$node" --subresource=status --type=json \
		-p '[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"1"}]' >/dev/null
done

# deploy_operator applies a tree's CRDs, RBAC and operator Deployment with
# that tree's local image.
deploy_operator() {
	local tree=$1 which=$2
	"$KUBECTL_BIN" apply -k "$tree/config/crd" >/dev/null
	"$KUBECTL_BIN" apply -f "$tree/config/default/namespace.yaml" >/dev/null
	"$KUBECTL_BIN" apply -k "$tree/config/rbac" >/dev/null
	sed -E "s|image: [A-Za-z0-9._/-]+/operator:[A-Za-z0-9._-]+(@sha256:[a-f0-9]+)?|image: kubeneuron-operator:$which-$tag|" \
		"$tree/config/default/operator_deployment.yaml" >"$work/operator.yaml"
	grep -Fq "image: kubeneuron-operator:$which-$tag" "$work/operator.yaml" || die "operator image substitution failed"
	"$KUBECTL_BIN" apply -f "$work/operator.yaml" >/dev/null
	"$KUBECTL_BIN" -n kube-neuron patch deployment kubeneuron-operator --type=strategic \
		-p '{"spec":{"template":{"spec":{"containers":[{"name":"operator","imagePullPolicy":"Never"}]}}}}' >/dev/null
	"$KUBECTL_BIN" -n kube-neuron rollout status deployment/kubeneuron-operator --timeout="${TIMEOUT_SECONDS}s" >/dev/null
}

# wait_for runs a check until it passes; on timeout it shows the check's
# last error output.
wait_for() {
	local what=$1 deadline=$((SECONDS + TIMEOUT_SECONDS))
	shift
	until "$@" 2>"$work/check.err"; do
		if ((SECONDS >= deadline)); then
			[[ -s $work/check.err ]] && sed 's/^/  check: /' "$work/check.err" | tail -20 >&2
			die "timed out waiting for $what"
		fi
		sleep 2
	done
}
agent_pods() { "$KUBECTL_BIN" -n "$NS" get pods -l "$AGENTS" -o json; }
label_of() { "$KUBECTL_BIN" get node "$1" -o jsonpath="{.metadata.labels.kubeneuron\\.io/journal-dir}"; }
labelled() { [[ $(label_of "$1") == "$2" ]]; }
# settled: every node runs exactly one Ready agent Pod, from the given
# DaemonSet on the given nodes, and no other agent Pod exists.
settled() {
	local ds=$1
	shift
	agent_pods | "$JQ_BIN" -e --arg ds "$NAME-$ds" --args '
	  [.items[] | select(.metadata.deletionTimestamp == null)] as $pods |
	  ($pods | group_by(.spec.nodeName) | all(length == 1)) and
	  all($ARGS.positional[] as $n | $pods[] | select(.spec.nodeName == $n);
	    (.metadata.ownerReferences[0].name == $ds) and
	    any(.status.conditions[]?; .type == "Ready" and .status == "True")) and
	  ($ARGS.positional | all(. as $n | any($pods[]; .spec.nodeName == $n))) and
	  ($pods | length) == (.items | length)' "$@" >/dev/null
}
pod_uid() {
	agent_pods | "$JQ_BIN" -r --arg n "$1" '.items[] | select(.spec.nodeName == $n and .metadata.deletionTimestamp == null) | .metadata.uid'
}
ds_uid() { "$KUBECTL_BIN" -n "$NS" get daemonset "$NAME-$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
variants_gone() { [[ -z $(ds_uid agent-detect-journal-var)$(ds_uid agent-detect-journal-run) ]]; }

# sample records, once a second, how many agent Pods each worker holds and
# how many of them are Ready.
sample() {
	while :; do
		agent_pods 2>/dev/null | "$JQ_BIN" -r --arg t "$SECONDS" --args '
		  $ARGS.positional[] as $n |
		  [.items[] | select(.spec.nodeName == $n and (.status.phase == "Pending" or .status.phase == "Running"))] |
		  "\($t) \($n) \(length) \([.[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length)"' \
			"${workers[@]}" >>"$work/samples" || true
		sleep 1
	done
}
# check_moves fails when a worker held two agent Pods, or no Ready one, for
# longer than ROLLOUT_SECONDS.
check_moves() {
	local node worst
	for node in "${workers[@]}"; do
		worst=$(awk -v n="$node" '$2 == n { bad = ($3 > 1 || $4 == 0); if (bad && start == "") start = $1; if (!bad) start = ""; if (bad && $1 - start > max) max = $1 - start } END { print max + 0 }' "$work/samples")
		((worst <= ROLLOUT_SECONDS)) || die "$node held two agent Pods or no Ready one for ${worst}s (limit ${ROLLOUT_SECONDS}s)"
		note "$node: longest stretch with two agent Pods or none Ready: ${worst}s"
	done
}

install_tree() {
	local tree=$1 which=$2
	deploy_operator "$tree" "$which"
	# Install log carries generated credentials: never print it.
	(cd "$work" && bash "$tree/deploy/install.sh" --name "$NAME" --namespace "$NS" \
		--manifest "$tree/config/default/namespace.yaml" --admin '' \
		--controller-image "kubeneuron-controller:$which-$tag" --agent-image "kubeneuron-agent:$which-$tag") \
		>"$work/install.log" 2>&1 || die "install.sh failed (credential-bearing log withheld)"
}

if ((upgrade)); then
	note "installing $BASE_REF"
	install_tree "$base_tree" base
	wait_for "the base agents" settled agent "${workers[@]}"
	agent_uid=$(ds_uid agent)
	sample & sampler=$!
	note "upgrading to this tree: CRDs, operator, then images"
	deploy_operator "$REPO_ROOT" head
	"$KUBECTL_BIN" patch kubeneuron "$NAME" --type=merge -p "{\"spec\":{\"controller\":{\"image\":\"kubeneuron-controller:head-$tag\"},\"agent\":{\"image\":\"kubeneuron-agent:head-$tag\"}}}" >/dev/null
else
	install_tree "$REPO_ROOT" head
	agent_uid=$(ds_uid agent)
	sample & sampler=$!
fi
for node in "${workers[@]}"; do wait_for "label run on $node" labelled "$node" run; done
wait_for "the journal-run agents" settled agent-detect-journal-run "${workers[@]}"
kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true
sampler=
[[ $(ds_uid agent) == "$agent_uid" ]] || die "the -agent DaemonSet was recreated"
check_moves
cp_node=$("$KUBECTL_BIN" get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')
[[ -z $(label_of "$cp_node") ]] || die "the control plane, which has no accelerator, was labelled"
note "PASS: upgrade kept -agent ($agent_uid) and moved both workers to the journal-run agent"

"$KUBECTL_BIN" -n "$NS" get daemonset "$NAME-agent-detect-journal-run" -o json | "$JQ_BIN" -e --arg name "$NAME" '
  .spec.selector.matchLabels == {"app.kubernetes.io/name": "kube-neuron", "app.kubernetes.io/instance": $name,
    "app.kubernetes.io/component": "agent-detect-journal-run", "app.kubernetes.io/managed-by": "kubeneuron-operator"} and
  .metadata.labels["kubeneuron.io/agent"] == "true" and
  ([.spec.template.spec.volumes[] | select(.hostPath.path == "/run/log/journal")] | length == 1 and .[0].hostPath.type == "Directory") and
  ([.spec.template.spec.containers[].volumeMounts[]? | select(.mountPath == "/host/run/log/journal")] | length == 1 and .[0].readOnly == true and (.[0].subPath // "") == "") and
  (.spec.template.spec.containers[0].securityContext.privileged // false) == false' >/dev/null ||
	die "the journal-run DaemonSet has the wrong selector, label or mount"
note "PASS: journal-run mounts only /run/log/journal, read-only, unprivileged"
# The node's agent says its journal source is open, so a missing incident
# after the kernel record cannot be a source that never started.
journal_source_open() {
	local pod
	pod=$("$KUBECTL_BIN" -n "$NS" get pods -l "$AGENTS" --field-selector "spec.nodeName=${workers[0]}" -o name) &&
		[[ -n $pod ]] &&
		"$KUBECTL_BIN" -n "$NS" logs "$pod" |
		"$JQ_BIN" -R -c 'fromjson? | select(.msg == "journal kernel log source open" and .dir == "/host/run/log/journal")' |
			grep -q .
}
wait_for "the journal source of the agent on ${workers[0]}" journal_source_open
note "PASS: the agent on ${workers[0]} opened its journal source"

# incidents_on: the node's incidents as a JSON array (an older controller
# answers null for none); fails when the API does not answer with a list, so an error
# never reads as "no incident".
incidents_on() {
	curl --silent --show-error --fail --noproxy '*' --max-time 10 -H "@$work/header" \
		"http://127.0.0.1:$port/api/v1/incidents?node=$1" |
		"$JQ_BIN" -e -c 'if . == null then [] elif type == "array" then . else .incidents end | arrays'
}
if ((KERNEL_RECORD)); then
	"$KUBECTL_BIN" -n "$NS" port-forward "service/$NAME-controller" :8080 >"$work/forward.log" 2>&1 &
	pf_pid=$!
	forwarded() {
		port=$(sed -n 's/.*127.0.0.1:\([0-9]*\) -> 8080.*/\1/p' "$work/forward.log" | head -1)
		[[ -n $port ]]
	}
	wait_for "the controller port-forward" forwarded
	printf 'Authorization: Bearer %s\n' "$("$KUBECTL_BIN" -n "$NS" get secret "$NAME-operator-api-token" -o jsonpath='{.data.token}' | base64 --decode)" >"$work/header"
	api_answers() { before=$(incidents_on "${workers[0]}"); }
	wait_for "the incident API" api_answers
	[[ $("$JQ_BIN" length <<<"$before") == 0 ]] || die "an incident was open on ${workers[0]} before the kernel record"
	# The kind node image sets ReadKMsg=no, so journald keeps no kernel
	# records; turn it on for this worker only, as a real node has it.
	"$DOCKER_BIN" exec "${workers[0]}" sh -c 'mkdir -p /etc/systemd/journald.conf.d &&
		printf "[Journal]\nReadKMsg=yes\n" >/etc/systemd/journald.conf.d/kubeneuron-e2e-kmsg.conf &&
		systemctl restart systemd-journald'
	kernel_in_journal() { "$DOCKER_BIN" exec "${workers[0]}" journalctl -k -q --no-pager -n 1 -o cat | grep -q .; }
	wait_for "kernel records in the journal of ${workers[0]}" kernel_in_journal
	# On restart journald copies the kernel buffer, which may hold XID lines
	# from earlier runs or other kind clusters on this host. This run's line
	# names its own PCI domain and is looked for only after the journal's
	# position from before it was written.
	pci=$(printf '%04x:3b:00' $((0x1000 + RANDOM % 0x8000)))
	journal_cursor=$("$DOCKER_BIN" exec "${workers[0]}" journalctl -k -q --no-pager -n 1 -o cat --show-cursor | sed -n 's/^-- cursor: //p')
	[[ -n $journal_cursor ]] || die "no journal cursor on ${workers[0]}"
	# A line written to /dev/kmsg from user space has the user facility, and
	# journald moves its leading "word:" into SYSLOG_IDENTIFIER. The driver's
	# own lines have the kernel facility: identifier "kernel", MESSAGE starting
	# with "NVRM:". The leading "kernel:" makes journald store this line so.
	"$DOCKER_BIN" exec "${workers[0]}" sh -c "echo 'kernel: NVRM: Xid (PCI:$pci): 79, pid=1, GPU has fallen off the bus.' >/dev/kmsg"
	xid_in_journal() {
		"$DOCKER_BIN" exec "${workers[0]}" journalctl -k -q --no-pager -o json --after-cursor "$journal_cursor" >"$work/after.json" &&
			"$JQ_BIN" -c --arg pci "PCI:$pci" \
				'select(.MESSAGE | type == "string" and contains($pci)) | {_TRANSPORT, SYSLOG_IDENTIFIER, MESSAGE}' \
				"$work/after.json" >"$work/xid.json" && [[ -s $work/xid.json ]]
	}
	wait_for "the XID 79 record for $pci in the journal of ${workers[0]}" xid_in_journal
	"$JQ_BIN" -e -s --arg line "NVRM: Xid (PCI:$pci): 79," \
		'length == 1 and (.[0] | ._TRANSPORT == "kernel" and .SYSLOG_IDENTIFIER == "kernel" and (.MESSAGE | startswith($line)))' \
		"$work/xid.json" >/dev/null || die "the XID record is not stored as a driver line: $(cat "$work/xid.json")"
	xid_incident() {
		local list
		list=$(incidents_on "${workers[0]}") || return 1
		incident_id=$("$JQ_BIN" -r --arg pci "$pci" '[.[] | select(.target.pci_addr == $pci)][0].id // empty' <<<"$list")
		[[ -n $incident_id ]]
	}
	wait_for "an incident on ${workers[0]} for $pci from the journal" xid_incident
	curl --silent --show-error --fail --noproxy '*' --max-time 10 -H "@$work/header" \
		"http://127.0.0.1:$port/api/v1/incidents/$incident_id" >"$work/incident.json" ||
		die "reading incident $incident_id failed"
	"$JQ_BIN" -e --arg id "$incident_id" --arg n "${workers[0]}" --arg pci "$pci" '
		.incident.id == $id and .incident.target.node == $n and .incident.target.pci_addr == $pci and
		any(.audit[]; .action == "open" and .params.xid == "79" and .params.vendor == "nvidia")' \
		"$work/incident.json" >/dev/null || {
		"$JQ_BIN" -c '{incident: {id: .incident.id, class: .incident.class, target: .incident.target}, open: [.audit[] | select(.action == "open") | .params]}' "$work/incident.json" >&2 || true
		die "incident $incident_id is not the XID 79 for $pci on ${workers[0]}"
	}
	note "PASS: this run's kernel XID 79 ($pci) read from the journal opened incident $incident_id on ${workers[0]}"
else
	note "skip: KERNEL_RECORD=0 (the kernel record is shared with every kind cluster on this host)"
fi

moved=${workers[0]} neighbour=${workers[1]}
neighbour_uid=$(pod_uid "$neighbour")
"$KUBECTL_BIN" patch kubeneuron "$NAME" --type=merge -p "{\"spec\":{\"agent\":{\"kernelLog\":{\"journal\":{\"nodeOverrides\":[{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$moved\"},\"dir\":\"none\"}]}}}}}" >/dev/null
wait_for "the label off $moved" labelled "$moved" ""
wait_for "$moved back on -agent" settled agent "$moved"
wait_for "one agent on $neighbour" settled agent-detect-journal-run "$neighbour"
[[ $(pod_uid "$neighbour") == "$neighbour_uid" ]] || die "an override on $moved restarted the agent on $neighbour"
note "PASS: an override on $moved left the agent Pod on $neighbour as it was ($neighbour_uid)"

if "$DOCKER_BIN" exec "$moved" test -d /var/log/journal; then
	note "skip: $moved has /var/log/journal, so a var mount does not fail there"
else
	"$KUBECTL_BIN" patch kubeneuron "$NAME" --type=merge -p "{\"spec\":{\"agent\":{\"kernelLog\":{\"journal\":{\"nodeOverrides\":[{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$moved\"},\"dir\":\"var\"}]}}}}}" >/dev/null
	wait_for "label var on $moved" labelled "$moved" var
	started=$SECONDS
	# The wait is three minutes plus a janitor pass; give it its own budget.
	TIMEOUT_SECONDS=$((TIMEOUT_SECONDS + 240)) wait_for "the label off $moved after the failed mount" labelled "$moved" ""
	((SECONDS - started >= 170)) || die "the label came off $moved after $((SECONDS - started))s, before the three-minute wait"
	wait_for "$moved back on -agent" settled agent "$moved"
	[[ $(pod_uid "$neighbour") == "$neighbour_uid" ]] || die "the failed mount on $moved restarted the agent on $neighbour"
	note "PASS: a var mount that cannot start lost its label after $((SECONDS - started))s"
fi

"$KUBECTL_BIN" patch kubeneuron "$NAME" --type=merge -p '{"spec":{"agent":{"kernelLog":{"journal":{"enabled":false,"nodeOverrides":null}}}}}' >/dev/null
for node in "${workers[@]}"; do wait_for "the label off $node" labelled "$node" ""; done
wait_for "the variants removed" variants_gone
wait_for "every node on -agent" settled agent "${workers[@]}"
[[ $(ds_uid agent) == "$agent_uid" ]] || die "disabling the journal recreated -agent"
"$KUBECTL_BIN" -n "$NS" get daemonset "$NAME-agent" -o json | "$JQ_BIN" -e '.spec.template.spec.affinity == null' >/dev/null ||
	die "-agent kept its node affinity with the journal disabled"
note "PASS: enabled=false removed both labels and both variants; -agent kept its UID"
echo "kind-journal: PASS"
