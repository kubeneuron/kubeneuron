# Data and privacy

KubeNeuron sends no usage data, metrics or reports to its authors and does not check for updates. Data leaves your cluster only through the connections listed below: destinations you configure, the Kubernetes and cloud APIs the software needs to run, and image pulls from the publishers' registries.

## Agent privileges

The installed agent runs privileged and shares the host PID namespace, including in dry-run mode. It reads host kernel fault messages and uses host namespaces and tools for configured diagnostics and node actions. Dry-run controls remediation execution. It does not change the agent pod's permissions.

## What is stored

For each incident KubeNeuron records the accelerator worker pods observed on the node (namespace, name, UID) and the training jobs that own them: kind, namespace, name, UID and status. It also records the observation time and the completion index when verified for an Indexed Job. It reads them with `get` on Jobs, JobSets, RayClusters and RayJobs that own the affected pods. It does not store or log job specifications, environment variables, commands or annotations. Job names stay in the incident record and audit trail; they are not added to metric labels and are left out of notifications by default. They are deleted with the incident.

Incidents, approvals, actions, operational state and the audit trail live in SQLite on a volume in your cluster by default. If you configure PostgreSQL, they live at the database address you supply. Approval and audit records identify the person who approved, rejected or triggered an action. Password sign-in accounts use a Kubernetes Secret. For automatic mode, the policy and audit trail record the identifier declared in `enabledBy`. KubeNeuron excludes that identifier from metric labels, logs and telemetry exports.

The managed metrics store keeps metrics in your cluster for 30 days by default. You can change its retention period. GPU series identify nodes and GPU UUIDs. The managed Collector drops standard workload labels for pod, namespace, container and pod UID before storage unless you enable `keepWorkloadLabels`. Additional labels you enable in dcgm-exporter are stored as sent.

Managed Grafana keeps its administrator account in your cluster. The installation generates its password in a Secret that is removed with the installation.

## Connections

- The installer runs on your workstation. It downloads the install manifest from GitHub Releases and, with `--version latest`, asks the GitHub API for the newest release. Installed components do not check for updates.
- The agent sends observations to the configured controller. When a DCGM client is available it can contact the DCGM host engine. Host tools and scripts you supply may make their own connections.
- Cluster components access the Kubernetes API. The controller connects to the configured database, OIDC provider and notification destinations. Slack, PagerDuty and webhook recipients receive incident details. The web panel loads no third-party scripts, fonts or analytics. OIDC sign-in redirects the browser to your identity provider.
- With the AWS provider enabled, cloud operations use the EC2, EKS and Auto Scaling APIs. The AWS SDK may contact STS, instance metadata or container credential endpoints for authentication.
- With managed monitoring, the OpenTelemetry Collector, VictoriaMetrics and vmalert exchange metrics inside the cluster, and vmalert sends metric alerts to the controller; the alerts carry node and GPU identifiers, not workload names. External telemetry destinations require your configuration. The audit log stays in the configured controller database.
- Managed Grafana queries the configured metrics store. By default, usage reporting, update checks, Gravatar and external snapshot sharing are disabled. The installation references the unmodified Grafana OSS image (AGPL-3.0) from its publisher.
- Your cluster pulls unmodified images from their publishers. The managed monitoring stack references OpenTelemetry Collector, open VictoriaMetrics and vmalert (Apache-2.0). It can scrape an existing dcgm-exporter. The NVIDIA driver and dcgm-exporter are used under their vendors' terms when present. The optional external dependency manifests have their own installation and licence information.

## Deletion and access

Uninstall retains the controller's state by default. See the [installation guide](install.md) for the explicit state deletion option. Manage an external PostgreSQL database or external metrics store through its own retention and deletion controls.

The operator never deletes the managed metrics volume. `deploy/install.sh --uninstall --delete-data` deletes it together with the controller's state; a plain uninstall keeps it, and it can still contain metrics and node names. To erase the volume, run `kubectl delete pvc <name>-metrics-store -n <namespace>`, substituting the KubeNeuron resource name and its configured namespace. Underlying disk disposal follows the volume's reclaim policy.

Your team controls access and retention. Handle user identities in approval and audit records according to the requirements that apply in your environment.
