# Report training progress and checkpoints

A training worker can report when it finishes a checkpoint, resumes from one, or completes a training step. KubeNeuron associates these application statements with matching incident job maps using the pod's Kubernetes identity. A checkpoint's age measures time since the controller received the report. Reading or loading a checkpoint alone does not confirm subsequent training progress.

## Set up the worker pod

The cluster administrator enables `spec.trainingReports: true` on the KubeNeuron installation and gives the job owner its installation UID and controller Service DNS name. In the worker namespace the administrator creates a ConfigMap named `training-report-ca` containing the controller's **public CA bundle** under `ca.crt`. During CA rotation, distribute both roots before switching the serving certificate and remove the old root after workers trust the new one. Where NetworkPolicies restrict traffic, allow worker pods to reach the controller on TCP 8444.

Add this volume to the job's pod template, replacing `INSTALLATION_UID` with the installation's actual `metadata.uid`:

```yaml
volumes:
  - name: training-report
    projected:
      sources:
        - serviceAccountToken:
            audience: kubeneuron.io/training-report/INSTALLATION_UID
            expirationSeconds: 3600
            path: token
        - configMap:
            name: training-report-ca
            items:
              - key: ca.crt
                path: ca.crt
```

Mount it read-only at `/var/run/training-report` in the worker container. Set `TRAINING_REPORT_HOST` to the controller Service DNS name, for example `fleet-controller.kube-neuron-system.svc` for an installation named `fleet` in that runtime namespace. The worker needs a verified job owner, an accelerator allocation and a monitored node. The controller checks its pod-bound token using Kubernetes TokenReview. The worker service account needs no permission to edit the job or incident.

## Send a report from Python

This function uses the Python standard library and can be called from a training loop. It rereads the projected token on each call so Kubernetes can rotate it.

```python
import http.client, json, os, ssl, time
from pathlib import Path

def report(**fact):
    volume = Path("/var/run/training-report")
    for attempt in range(5):
        tls = ssl.create_default_context(cafile=str(volume / "ca.crt"))
        conn = http.client.HTTPSConnection(os.environ["TRAINING_REPORT_HOST"], 8444, context=tls, timeout=5)
        try:
            headers = {"Authorization": "Bearer " + (volume / "token").read_text().strip(), "Content-Type": "application/json"}
            conn.request("POST", "/v1/training/reports", json.dumps(fact), headers)
            status = conn.getresponse().status
            if status != 503 or attempt == 4:
                return status
        finally:
            conn.close()
        time.sleep(2 ** attempt)
```

Call `report(kind="checkpoint", checkpoint_id="epoch-12", step=1200)` after the checkpoint write succeeds. After the replacement worker loads it, call `report(kind="resume", checkpoint_id="epoch-12")`. Once training advances, call `report(kind="progress", step=1201)`. Use a nonnegative step and an opaque checkpoint identifier of up to 128 ASCII letters, digits, periods, underscores or hyphens. Use the same identifier for a checkpoint and its resume. Keep storage paths, URLs, personal data and credentials out of identifiers.

HTTP 204 means the controller handled the request. A worker without a matching incident also receives 204. The controller caches report routes for five seconds, so a report received just after an incident opens may return 204 without being attached to that incident. With PostgreSQL, the Service routes training reports to the Ready controller replica. Only the elected leader becomes Ready. A request that reaches a standby during a leadership change receives HTTP 503. Retry the same report on a fresh connection with exponential backoff. The retries can block the caller for up to about 40 seconds. Call the reporting function outside the training step if that delay matters. Send routine progress at most once every 30 seconds per worker. For HTTP 429, back off before retrying. Duplicate reports preserve the original receipt time. Each worker has a budget of 60 requests per minute. Reporting is optional; a reporting failure can be handled separately from training or checkpoint I/O.

In incident detail, each affected job has a `recovery.progress_status`. It is `unknown` when infrastructure readiness, the job identity or report evidence is unavailable. After verified infrastructure recovery, a job with report evidence has status `not_confirmed` until an accepted progress report advances its worker's step. The status then becomes `confirmed`, with the first confirmation time in `progress_confirmed_at`.

## Use checkpoint age before drain

A report is the worker's own statement. KubeNeuron does not check that the checkpoint exists, is complete or can be loaded.

Set `spec.automatic.maxCheckpointAgeMinutes` on the remediation policy to require a recent checkpoint report from each affected job before automatic drain. Zero disables this check. For example, with a value of 10, a receipt exactly ten minutes old meets the requirement. An older receipt sends the drain for human approval. The approval preview shows the age at the time the preview was computed.

When the job owner or checkpoint age is unknown, the default is human approval. Set `spec.automatic.allowUnknownCheckpoint: true` to let automatic drain proceed in that case. A known stale checkpoint still requires approval. A storage read error also requires approval.

The receiver associates reports with matching incident job maps and accepts them for up to 24 hours after resolution. A checkpoint reported before any matching incident exists cannot establish freshness for a later drain.

## Read the recovery report

In `kubeneuronctl report --json`, the live report's `incident_details[].jobs[]` lists identified jobs by namespace and owner. For each job, `infrastructure_ready_seconds` measures elapsed time from incident opening to verified infrastructure readiness. `progress_confirmed_seconds` uses the same starting point and ends at confirmed worker progress after readiness. Each field appears only when its evidence is available. After a cloud replacement, verified infrastructure readiness is the moment the node that took the old node's place was confirmed. A replacement counts as recovered when the new node is confirmed at the depth available for its accelerator; the resolution of each incident says which. It does not mean the job resumed: that is what `progress_confirmed_seconds` shows.

`checkpoint_age_at_drain_seconds` measures the time from the last checkpoint receipt to the audited start of the latest live drain in the current attempt. Later receipts leave that historical age unchanged. The report omits the field when either timestamp is unknown.
