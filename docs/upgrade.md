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

0.x releases are no longer published. Use the copy of the installer and of the release manifest that you downloaded when you installed 0.x. For each installation, run that installer with its original name and namespace:

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

If you no longer have the 0.x installer or manifest, remove the installation by hand after the backups: delete each `KubeNeuron` object and wait for its workloads to go, delete the installation namespaces (this deletes their PVCs; volumes with a `Retain` policy and external databases stay, as noted above), delete the CRDs of the `kubeneuron.io` group (`kubectl get crd -o name | grep kubeneuron.io` lists them), then find and delete what is left at cluster scope:

```sh
kubectl get clusterrole,clusterrolebinding,validatingwebhookconfiguration,mutatingwebhookconfiguration -o name | grep -i kubeneuron
```

The remaining instructions apply to installations running v1.0.0-rc.1 or later.

## Before any upgrade

After v1.0.0-rc.1, installations that omit `spec.safety.impactPreview.enabled` compute and store an advisory preview at each approval round and include its summary in configured approval notifications. Set it to `false` to retain the previous behavior. An existing explicit `false` remains effective. The default budget is 15 seconds. Workload-context resolvers and mandatory previews remain opt-in. For a round already awaiting approval, the controller computes a preview on explicit recompute or at the next park for approval; this default change preserves a timely approval already recorded for the same round and step.

After v1.0.0-rc.1, the agent reads NVIDIA GPUs through the NVIDIA container runtime by default (`spec.agent.deviceAccess: NvidiaRuntime`). The upgrade rolls every agent DaemonSet of an installation without node-level actions once, node by node, to add two environment variables and one argument; no host path is added. On nodes with the NVIDIA runtime as default, the agents then report each GPU's model and UUID and the driver version (see [Data and privacy](data-and-privacy.md)). Set `deviceAccess: None` before upgrading to keep the previous behaviour: the agent reads no NVIDIA devices unless node-level actions and host tooling are enabled.

If a policy's circuit breaker tripped on rc.1, upgrading preserves the trip for that policy revision. Look for `circuit-breaker-tripped` in the policy's operational audit. Review the pool's health before setting a new `spec.automatic.enabledAt` to resume automatic authorisation for new incidents. An incident already in `AWAITING_APPROVAL` still needs an operator decision.

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
   there is no such gap. Each agent DaemonSet rolls node by node; each agent
   turns Ready only after a fresh durable registration acknowledgment from
   the new controller.

3. **Verify**: root `Ready=True`, controller `/metrics` serving, agent
   DaemonSets fully available, and a synthetic signal walks to an incident
   (see [quickstart](quickstart.md) step 5). With node-level actions disabled, check the kernel-log source in `kubeneuronctl health` after rollout. See [Kernel log from the node's journal](install.md#kernel-log-from-the-nodes-journal) for the unprivileged agent's read-only mount and fault coverage.

## Version skew

The agent posts with an exact versioned capability token: an upgraded agent
against an older controller (or vice versa) fails closed on registration
rather than corrupting inventory. Complete the controller rollout before or
together with the agent rollout inside one release; do not run mixed
versions longer than a rolling upgrade needs.

## Rolling back

### From v1.0.0-rc.2 to v1.0.0-rc.1

Let active multi-zone add-first replacements finish, then follow these steps
in order.

1. Apply the rc.1 install manifest and wait for the rc.1 operator rollout
   to complete. This also restores the rc.1 CRDs.

   ```sh
   kubectl apply -f https://github.com/kubeneuron/kubeneuron/releases/download/v1.0.0-rc.1/kubeneuron-install-v1.0.0-rc.1.yaml
   kubectl -n kube-neuron rollout status deployment/kubeneuron-operator
   ```

2. Set `spec.controller.image` and `spec.agent.image` on each root object to
   their rc.1 digests. The rc.1 controller refuses to start until the backup
   is restored because the rc.2 schema (SQLite 0038 or PostgreSQL 0029) is
   newer than it supports.
3. Scale the operator to zero, then scale each controller to zero and wait
   for its pods to terminate. Restore the pre-upgrade backup for each
   workflow store. Restoring discards incidents and audit entries written
   since the backup was taken.
4. Restart the rc.1 operator. Verify the rc.1 controller and agent pods
   become Ready.

The rc.1 CRD drops `spec.agent.deviceAccess` from the KubeNeuron object.
If you set it to `None`, reapply that setting when you upgrade again.
Otherwise rc.2 uses its default, `NvidiaRuntime`. Both CRDs accept an
explicit `spec.safety.impactPreview.enabled: false`.

The rc.1 GPURemediationPolicy CRD also drops `spec.replacement.successorZone`.
If a policy used `AnyPoolZone`, reapply it after upgrading again. Otherwise
rc.2 uses `SameZone` for that policy.

After the rollback, the Role `<name>-runtime-reader` added by rc.2 remains;
it grants read access to one ConfigMap only and is deleted with the KubeNeuron object.

Rolling back only the controller and agent images fails. The rc.2 operator
passes `--nvidia-runtime-devices` to the agent. The rc.1 agent exits on
startup because it does not recognize that flag. Until the rollback is completed in the order above,
no agent runs and faults the agent detects are not detected.

The upgrade from rc.1, the refusal by rc.1, the rollback in this order and a
second upgrade were rehearsed on kind for both stores. They were not rehearsed
on a cloud cluster.

### Other release pairs

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
