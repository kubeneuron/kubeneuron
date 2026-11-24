# Uninstall KubeNeuron

## Pause repairs and keep the installation

Use `kubeneuronctl pause` to stop new remediation actions while you investigate.
An action already running can finish. Use `kubeneuronctl resume` when ready.
Choose `DryRun` if you want to keep recording incidents and see what the policies
would do. See [Pausing automation](operations.md#pausing-automation).

## Remove an installation and keep its data

Use the release's `install.sh`, with `kubectl` pointed at the intended cluster.
The machine running the script needs `jq`, `curl` and `openssl`. The open-incident
check needs permission to read the operator API token Secret in the runtime
namespace and open a port-forward to the controller. These examples use the
default installation name and runtime namespace. Check yours first:

```sh
kubectl config current-context
kubectl get kubeneurons -o custom-columns='NAME:.metadata.name,RUNTIME-NAMESPACE:.spec.namespace'
```

1. Pause new repairs. Inspect replacements with `kubeneuronctl incidents`
   and `kubeneuronctl pools`. Wait for active replacements to finish. If a
   replacement needs an operator, check its incident and the actual machines,
   pool size and tags with the cloud provider. Once you have settled the
   operation, use the command in the incident, for example
   `kubeneuronctl resolve <id> --cloud-operation-settled --reason "<what you found>"`.
   Uninstalling does not reverse a resize or remove cloud replacement tags.
   The script checks a snapshot of incidents before deleting the root.
   If it cannot read the list, restore API access and try again. Use
   `--skip-open-incident-check` only after checking outstanding operations
   with the provider. See the [AWS](cloud-aws.md), [Azure](cloud-azure.md)
   and [GKE](cloud-gcp.md) replacement procedures.
2. If you plan to reinstall KubeNeuron or to restore a backup of its store later,
   note which nodes KubeNeuron has cordoned before you uninstall; afterwards check them and cordon them again where needed. Use
   `kubectl get nodes` and inspect each affected node with
   `kubectl get node <node> -o yaml`. If the new or restored store has no record
   of the incident, the controller can remove its cordon and make a faulty
   node schedulable again. A cordon already marked as held for an incident
   waiting for the operator is kept.
3. Run the uninstall while the operator is still running. It confirms that
   the installation's SQLite volume claim can be kept before the script
   removes the root resource. The managed metrics claim is also retained.
   Back up data you need before proceeding. See [Backup and restore](operations.md).

   ```sh
   bash install.sh --uninstall --name kubeneuron --namespace kube-neuron
   ```

4. After the script succeeds, check for other installations with
   `kubectl get kubeneurons`. Keep the operator while another installation uses it.

   **Release manifest.** If you want to keep data, do not run
   `kubectl delete -f <install manifest>`. It deletes the KubeNeuron CRDs and
   every KubeNeuron custom resource cluster-wide, and deletes the
   `kube-neuron` namespace with every PVC there. Retained claims in a different
   runtime namespace survive. To remove only the operator and its permissions,
   use the commands printed by the script for the release manifest. The script prints the namespace where it found the operator Deployment.

   **Helm.** Run `install.sh --uninstall` first (step 3), and only then remove the operator release:
   use `helm list -A` to find the operator's release and namespace,
   then run `helm uninstall <release> -n <operator-namespace>`.
   Before removing the operator, Helm runs a Job that checks for remaining
   KubeNeuron installations. If the check fails, use the Helm release namespace for `<namespace>` and read its output with
   `kubectl -n <namespace> logs job/kubeneuron-operator-uninstall-check`.
   If you intend to remove only the operator and leave the installations running
   without it, run `helm uninstall <release> -n <operator-namespace> --no-hooks`.
   This also bypasses the check if the Job cannot start or cannot list installations.
   If you already
   removed the operator with Helm while the installation was still present,
   the controller and agents are still running. To keep the stored data,
   reinstall the operator and repeat the uninstall in this order.
   If you do not need the stored data, repeat `install.sh --uninstall` with
   `--delete-data` instead; it removes the installation and its managed volume
   claims. The script asks for confirmation; in a non-interactive run add `--yes`.

## Remove the installation and clean up its data

Before confirming `--delete-data`, copy any node-local agent records you need, including NVIDIA diagnostic bundles.

**Deleting the managed claims can destroy SQLite incidents, their audit history
and stored metrics.** Check backups and settle cloud operations first. A PV's
reclaim policy determines what happens to the underlying volume after its claim
is deleted. The backup CronJob and its separate PVC remain, as do user-installed
cert-manager CA Secrets containing private keys. To request deletion, use the same procedure above with this command
at step 3:

```sh
bash install.sh --uninstall --name kubeneuron --namespace kube-neuron --delete-data
```

The script removes installer-managed credentials, playbooks and policies.
Kubernetes garbage collection removes runtime components owned by the root.
Review the following resources before declaring cleanup complete. Substitute
the names from your installation before running a deletion command.

| Resource | What to check and how to clean up |
| --- | --- |
| Storage and backups | Inspect `kubectl get pvc -A` and `kubectl get pv`. External PostgreSQL, external metrics stores and user backups remain under their owners' retention policies. For the optional backup example, remove its schedule with `kubectl -n <namespace> delete cronjob kubeneuron-backup`. Copy needed backups before deleting its separate PVC with `kubectl -n <namespace> delete pvc kubeneuron-backups`. |
| CRDs and user-created resources | CRDs and user-created policies, playbooks and other custom resources remain. Inventory types with `kubectl api-resources --api-group=kubeneuron.io`, then `kubectl get <resource-type>`. Delete individual resources with `kubectl delete <resource-type> <name>`. **Deleting a CRD deletes every object of that type across the cluster.** Only after checking all installations and exporting needed resources, use `kubectl delete crd <full-crd-name>` for each definition you intend to retire. |
| Node scheduling and annotations | Inspect `kubectl get node <node> -o yaml` for cordon owners, held and restore annotations, the degraded taint and `karpenter.sh/do-not-disrupt`. Once the node is healthy and safe to schedule, use the commands printed for that node: `kubectl uncordon <node>` and, if present, `kubectl taint nodes <node> kubeneuron.io/degraded-`. Review prior values and other owners before using `kubectl annotate node <node> <annotation-key>-` to remove an obsolete annotation. When removing the last installation, the script removes `karpenter.sh/do-not-disrupt=true` if the cordon restore record shows that the annotation was absent before KubeNeuron cordoned the node. It preserves pre-existing settings. If ownership is unclear, check who needs the annotation before running the printed removal command. |
| NVIDIA GPU Operator | On the last installation, uninstall restores to `true` only components recorded in `kubeneuron.io/quiesced-accelerator-stack` whose `nvidia.com/gpu.deploy.<component>` label is still `false`. It removes the stack annotation in the same update. Leave other disabled components alone. If an update fails, review and run the printed label command for that node. If another installation remains, the script leaves its labels in place. |
| Agent files | Inspect `/var/lib/kube-neuron` on every host where an agent ran, including control-plane nodes. Ordinary uninstall retains these files. With `--delete-data`, removing the last installation also attempts to empty this directory on the nodes listed by this installation’s agent Pods before removal. The script reports how many nodes it emptied and lists unconfirmed nodes with a manual command. When another installation remains, it retains the shared directory on every node. It contains the event queue (`spool.jsonl`) and local action journal (`actions.jsonl`), and can contain read positions, device state and NVIDIA diagnostic archives in `bundles/`. These archives come from `nvidia-bug-report.sh` and can include kernel logs, process names and device serial numbers. If delivery is delayed, the queue can contain the matching kernel message read from `/dev/kmsg`. Copy any records you need. Restrict access to any copy of the action journal: it can contain per-action lease tokens. Once no agent or other installation uses the directory and its contents can be erased, run `sudo rm -rf -- /var/lib/kube-neuron` on that host. Nodes already removed from the cluster require host access. |
| Certificates and Secrets | For user-installed cert-manager resources, inspect `kubectl -n <namespace> get certificates,issuers,secrets`. Remove only dedicated Certificate and Issuer resources first, then delete their unused leaf or CA Secrets with `kubectl -n <namespace> delete secret <name>`. Check other consumers before deleting a CA private key. |
| Monitoring dependencies | Follow the [dependency removal order](../deploy/kubernetes/dependencies/README.md#removal-order-and-retained-data) while the VictoriaMetrics operator is still running. Remove routes to the old controller and copied webhook Secrets. Inspect remaining claims and workloads before deleting a dedicated namespace with `kubectl delete namespace kubeneuron-monitoring`. |
| Leases and namespaces | Inspect `kubectl -n <namespace> get leases` for the operator or controller Lease. When its component is gone and no other installation uses it, run `kubectl -n <namespace> delete lease <name>`. Delete a dedicated namespace only after checking its remaining workloads, Secrets and claims. |
| Cloud resources | Check instance identity, pool size and pending provider operations against the incident before removing its records. For AKS, check the pool's replacement tags, including `kubeneuron-replacing`. Use the provider procedure linked above to settle outstanding changes before retiring any tag. |
