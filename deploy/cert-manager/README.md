# TLS via cert-manager

Automates issuance and renewal of the four-Secret TLS model from
[docs/install.md](../../docs/install.md) §3. Requires cert-manager v1.13+.

```sh
export NAME=<kubeneuron name> NAMESPACE=<spec.namespace>
export ROOT_UID=$(kubectl get kubeneuron "$NAME" -o jsonpath='{.metadata.uid}')
envsubst < kubeneuron-pki.yaml | kubectl apply -f -
kubectl -n "$NAMESPACE" wait --for=condition=Ready \
  certificate/${NAME}-controller-tls certificate/${NAME}-agent-tls --timeout=120s
```

Then reference the produced Secrets from `spec.tls`:

```yaml
tls:
  serverSecretRef:   {name: ${NAME}-controller-tls}
  serverCASecretRef: {name: ${NAME}-controller-tls, key: ca.crt}
  clientSecretRef:   {name: ${NAME}-agent-tls}
  clientCASecretRef: {name: ${NAME}-agent-tls, key: ca.crt}
```

The CA references point at the *leaf* Secrets' `ca.crt` — the operator
mounts only that key, so no CA private key ever reaches a workload.

**Renewal:** cert-manager rotates the leaf Secrets in place (35 days before
expiry — ahead of the 30-day `KubeNeuronTLSCertExpiringSoon` alert). The
controller and agents load certificates at startup: schedule
`kubectl -n $NAMESPACE rollout restart deploy/${NAME}-controller ds/${NAME}-agent`
after a renewal, or annotate the workloads for Reloader/Wave.

**CA rotation** stays a deliberate, human-driven event: follow the
expand-trust → switch-leaf → contract-trust procedure exercised by the kind
harness (`hack/tls-rotate.sh`), never an unattended re-issue of a CA that
every fleet member must trust simultaneously.
