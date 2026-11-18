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

This function uses the Python standard library and can be called from a PyTorch training loop. It rereads the projected token on each call so Kubernetes can rotate it.

```python
import http.client, json, os, ssl
from pathlib import Path

def report(**fact):
    volume = Path("/var/run/training-report")
    tls = ssl.create_default_context(cafile=str(volume / "ca.crt"))
    conn = http.client.HTTPSConnection(os.environ["TRAINING_REPORT_HOST"], 8444, context=tls, timeout=5)
    try:
        headers = {"Authorization": "Bearer " + (volume / "token").read_text().strip(), "Content-Type": "application/json"}
        conn.request("POST", "/v1/training/reports", json.dumps(fact), headers)
        return conn.getresponse().status
    finally:
        conn.close()
```

Call `report(kind="checkpoint", checkpoint_id="epoch-12", step=1200)` after the checkpoint write succeeds. After the replacement worker loads it, call `report(kind="resume", checkpoint_id="epoch-12")`. Once training advances, call `report(kind="progress", step=1201)`. Use a nonnegative step and an opaque checkpoint identifier of up to 128 ASCII letters, digits, periods, underscores or hyphens. Use the same identifier for a checkpoint and its resume. Keep storage paths, URLs, personal data and credentials out of identifiers.

HTTP 204 means the controller handled the request. A worker without a matching incident also receives 204. For HTTP 429, back off before retrying. Duplicate reports preserve the original receipt time. Each worker has a budget of 60 requests per minute. Reporting is optional; a reporting failure can be handled separately from training or checkpoint I/O.
