# Upgrading and rolling back

This runbook covers upgrading KubeNeuron itself. Third-party dependencies
(GPU Operator, VictoriaMetrics stack) have their own pinned procedure in
[`deploy/kubernetes/dependencies/`](https://github.com/kubeneuron/kubeneuron/tree/main/deploy/kubernetes/dependencies).

The API is `v1alpha1`: minor releases may change it without conversion
support. Read the release notes for every version you skip.

## Moving from 0.x to v1.0

Upgrading from 0.x releases is not supported. Uninstall the 0.x installation, then install v1.0.

Helm skips existing CRDs from the chart’s `crds/` directory without checking their release line, so follow the same removal steps below before installing v1.0 with Helm.

Back up policies, configuration and incident history before uninstalling. Back up the state store separately. The old installer can remove install-managed policies shared by multiple installations. Removing a CRD deletes every object of that type across the cluster.

For each installation, use the installer from its installed 0.x release with its original name and namespace:

```sh
bash ./install.sh --uninstall --name "$NAME" --namespace "$NS"
```

After removing all 0.x installations, inspect the original release manifest before deleting it. Its resources can include shared CRDs and a namespace. Deleting a namespace also deletes its PVCs. Once backups are complete, remove the shared operator with `kubectl delete --ignore-not-found -f "$OLD_MANIFEST"`. For v0.7.0 and v0.8.0, remove any remaining CRDs with the command below. For another 0.x release, use the list in that release's manifest.

```sh
kubectl delete crd --ignore-not-found \
  acceleratorruntimeprofiles.kubeneuron.io gpuautonomyplans.kubeneuron.io \
  gpumaintenancewindows.kubeneuron.io gpunodeconfigs.kubeneuron.io \
  gpuplaybooks.kubeneuron.io gpuremediationpolicies.kubeneuron.io \
  gpusignalmappings.kubeneuron.io kubeneurons.kubeneuron.io
```

Check for retained PVCs and external databases before installing v1.0. Select fresh storage so the new installation starts without the old state.

For upgrades between 0.x releases, see the documentation shipped with that 0.x release.

The remaining instructions apply to installations running v1.0.0-rc.1 or later.

## Before any upgrade

1. Take a fresh workflow-store backup (see
   [operations](operations.md#sqlite-workflow-store-backup-and-restore)) and
   verify the snapshot opens. Schema migrations are **forward-only**: an
   older controller refuses a database touched by a newer one, so the only
   rollback path for the store is a restore.
2. Note the running versions: `kubectl -n kube-neuron get deployment
   kubeneuron-operator -o jsonpath='{.spec.template.spec.containers[0].image}'`
   and the `spec.controller.image` / `spec.agent.image` on your root object.
3. Check `kubectl get kubeneurons -o yaml` status conditions are all
   `Ready=True` — never start an upgrade from a degraded installation.

## Unreleased cloud agent permissions

On existing installations with node-level actions disabled, the replacement agent Pod loses access to `/dev/kmsg`. Numeric NVIDIA XIDs then depend on a reachable DCGM hostengine and field 230. The shipped exporter profile omits this field. Polling the last value can miss repeated identical XIDs and leave the `xid-app` threshold unmet. Without the hostengine, numeric XIDs are unavailable. AMD kernel ring timeouts and PCIe errors are also unavailable. ECC and page retirement can still be observed through accessible AMD SMI tooling. An unavailable source leaves the device's health unknown.

## Unreleased: kernel log from the node's journal

With node-level actions disabled, the agent now reads kernel entries from
the node's systemd journal, read-only (experimental). The agent has
read-only access to the node's systemd journal directories. It reads only
kernel entries (`_TRANSPORT=kernel`), matches them against its fault
patterns and sends only the classified event to the controller. It does not
store, forward or log journal entries, and does not read entries of other
transports. See [Kernel log from the node's journal](install.md#kernel-log-from-the-nodes-journal).

In every mode, including `DryRun`, KubeNeuron adds one label,
`kubeneuron.io/journal-dir`, to accelerator nodes to choose the agent's
journal mount. It runs no recovery action. Set
`spec.agent.kernelLog.journal.enabled: false` to remove the label and the
mount; set it before the upgrade to keep the agent without the journal.

What the upgrade does to the agent:

- `<name>-agent` keeps its name and UID. It rolls once to gain a node
  affinity that keeps it off labelled nodes and the label
  `kubeneuron.io/agent=true`.
- The operator adds `<name>-agent-detect-journal-var` and
  `<name>-agent-detect-journal-run`. When the controller labels a node, the
  node's agent Pod moves from `<name>-agent` to the matching DaemonSet. The
  agent's spool and cursors stay in `/var/lib/kube-neuron` across the move.
- Scripts that wait for agent rollouts should select every agent DaemonSet
  with `app.kubernetes.io/instance=<name>,kubeneuron.io/agent=true`, as
  `hack/tls-rotate.sh` does, not wait for `<name>-agent` alone.

## Unreleased: automatic replacement notes

- **Automatic replacement needs qualified profiles.** After the upgrade an
  `Automatic` policy replaces a node on its own only when the node's
  accelerator runtime profile is listed in `automatic.qualifiedProfiles` and
  the incident's fault location is `this-node`. An existing Automatic policy
  has no such list, so its replacement steps wait for approval until you add
  one; the earlier steps (cordon) still run as before, and the incident says
  why it waits. Check the policy's `AutomaticReplacement` condition: `False`
  with reason `NoQualifiedProfiles` means the list is empty. Listing a
  profile is the operator's decision; KubeNeuron does not verify the
  hardware or software behind it.
- **No schema migration and no new permission.** The incident evidence is
  an audit row; it reads the node and the pods on it with the access the
  controller already has.

## Unreleased: announced cloud interruptions

The cloud pack gains the `cloud-interruption` playbook and its policy (see
[Announced cloud interruptions](cloud-interruptions.md)). An installer rerun
on an existing installation keeps the policy objects it finds and does not
create new ones; it reports the two as missing. Until you create them, a
`cloud-interruption` incident is only observed: no job is asked and no node
is cordoned. To opt in, create the `GPUPlaybook` and the
`GPURemediationPolicy` for `cloud-interruption` from
`config/cloud-policies/`, with your installation's name in
`metadata.name`, `spec.kubeNeuronRef` and `spec.playbookRef`.

## Upgrade order

Always: **CRDs → operator → controller/agent images.**

1. **CRDs and operator** (from the release's install manifest):

   ```sh
   kubectl apply -f kubeneuron-install-vX.Y.Z.yaml
   kubectl -n kube-neuron rollout status deployment/kubeneuron-operator
   ```

   CRD schema additions are backward-compatible within `v1alpha1`; the
   operator tolerates older stored objects. CEL rules added by a release
   apply to *new* writes only — existing stored objects are not re-validated
   until modified.

2. **Controller and agent images**: edit the root object to the released,
   digest-pinned images:

   ```sh
   kubectl patch kubeneuron <name> --type=merge -p '{
     "spec": {
       "controller": {"image": "ghcr.io/kubeneuron/kubeneuron/controller:vX.Y.Z"},
       "agent":      {"image": "ghcr.io/kubeneuron/kubeneuron/agent:vX.Y.Z"}
     }
   }'
   ```

   On SQLite installs the controller Deployment uses `Recreate` with a
   single replica: expect a short public-API/webhook outage while the new
   Pod starts (Alertmanager retries deliveries). PostgreSQL HA installs run
   two replicas with rolling updates and leader-following readiness, so
   there is no such gap. The agent DaemonSet rolls node by node; each agent
   turns Ready only after a fresh durable registration acknowledgment from
   the new controller.

3. **Verify**: root `Ready=True`, controller `/metrics` serving, agent
   DaemonSet fully available, and a synthetic signal walks to an incident
   (see [quickstart](quickstart.md) step 5).

## Version skew

The agent posts with an exact versioned capability token: an upgraded agent
against an older controller (or vice versa) fails closed on registration
rather than corrupting inventory. Complete the controller rollout before or
together with the agent rollout inside one release; do not run mixed
versions longer than a rolling upgrade needs.

## Rolling back

- **Images only** (no schema migration in between): patch the root object
  back to the previous digests. The store is untouched.
- **After a schema migration**: the older controller will refuse the newer
  database (`database schema version N is newer than this binary`). Scale
  the controller to zero, restore the pre-upgrade snapshot onto the state
  PVC, then roll the images back. Incidents and audit rows written after the
  backup are lost — that is the RPO of your backup schedule.
- **Operator/CRDs**: re-apply the previous release's install manifest.
  Kubernetes does not remove already-stored fields; older operators ignore
  spec fields they do not know, and compilation stays fail-closed.

## Certificate material during upgrades

Upgrades do not rotate TLS Secrets. If an upgrade window coincides with a
planned rotation, finish the rotation first — `hack/tls-rotate.sh` phases
record exact root generations and will refuse to continue across an
unexpected generation change.

## Bare-metal alpha recovery

After upgrading, existing bare-metal installations switch to observation until
both node-level permissions are enabled. To resume node-level actions, set the Helm
value `operator.allowNodeLevelActions=true` and set `spec.safety.nodeLevelActions: true`
in each installation that requires them. With operator permission disabled, a
request in the CR produces an `OperatorPermissionRequired` warning and affected
playbooks run only their observation fallback.

A missing installation opt-in
appears as `InstallationOptInRequired`. In file-based mode, the controller
rejects a node-level playbook at startup when `safety.node_level_actions` is
disabled. Enable the setting or remove those playbooks before restarting.
