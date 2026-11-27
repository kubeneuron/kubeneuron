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

Then copy each CA certificate, without its key, into a Secret of its own,
and reference these Secrets from `spec.tls`:

```sh
kubectl -n "$NAMESPACE" get secret kubeneuron-server-ca -o jsonpath='{.data.ca\.crt}' | base64 -d |
  kubectl -n "$NAMESPACE" create secret generic ${NAME}-server-ca-bundle --from-file=ca.crt=/dev/stdin
kubectl -n "$NAMESPACE" get secret kubeneuron-client-ca -o jsonpath='{.data.ca\.crt}' | base64 -d |
  kubectl -n "$NAMESPACE" create secret generic ${NAME}-client-ca-bundle --from-file=ca.crt=/dev/stdin
```

```yaml
tls:
  issuer: External
  serverSecretRef:   {name: ${NAME}-controller-tls}
  serverCASecretRef: {name: ${NAME}-server-ca-bundle}
  clientSecretRef:   {name: ${NAME}-agent-tls}
  clientCASecretRef: {name: ${NAME}-client-ca-bundle}
```

Each reference names its own Secret: the operator refuses two references to
the same Secret. These CA Secrets hold only `ca.crt`. The CA keys stay in
cert-manager's Secrets kubeneuron-server-ca and kubeneuron-client-ca in this
namespace; no Pod refers to them.

**CA renewal:** cert-manager renews each CA certificate when it is 2/3
through its duration
([by default](https://cert-manager.io/docs/usage/certificate/#reissuance-triggered-by-expiry-renewal)),
here about 16 months after it was issued. `kubeneuron-pki.yaml` sets
`privateKey.rotationPolicy: Never` on both CAs, so the renewed certificate
keeps the CA key, and leaves signed with that key still verify against the
copies. Without it, cert-manager v1.18 and later
[make a new key](https://cert-manager.io/docs/releases/release-notes/release-notes-1.18/#the-default-value-of-certificatespecprivatekeyrotationpolicy-is-now-always)
on renewal. Nothing updates the copies: once cert-manager has renewed a CA
certificate, replace its copy before the copied certificate expires, two
years after it was issued; otherwise the agents and the controller stop
trusting each other. The operator warns with a `TLSMaterialExpiring` event on
the KubeNeuron resource from 45 days before a copied certificate expires,
and if you load the alert rules from
`deploy/kubernetes/dependencies/observability/rules.yaml`,
`KubeNeuronTLSCertExpiringSoon` fires from 30 days before. To replace the
copies, run the commands above with `--dry-run=client -o yaml | kubectl apply -f -`
added:

```sh
kubectl -n "$NAMESPACE" get secret kubeneuron-server-ca -o jsonpath='{.data.ca\.crt}' | base64 -d |
  kubectl -n "$NAMESPACE" create secret generic ${NAME}-server-ca-bundle --from-file=ca.crt=/dev/stdin \
    --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NAMESPACE" get secret kubeneuron-client-ca -o jsonpath='{.data.ca\.crt}' | base64 -d |
  kubectl -n "$NAMESPACE" create secret generic ${NAME}-client-ca-bundle --from-file=ca.crt=/dev/stdin \
    --dry-run=client -o yaml | kubectl apply -f -
```

**Renewal:** cert-manager rotates the leaf Secrets in place (35 days before
expiry — ahead of the 30-day `KubeNeuronTLSCertExpiringSoon` alert). The
controller and agents load certificates at startup: schedule
`kubectl -n $NAMESPACE rollout restart deploy/${NAME}-controller ds/${NAME}-agent`
after a renewal, or annotate the workloads for Reloader/Wave.

**CA rotation**, a new CA key, stays a deliberate, human-driven event:
follow the expand-trust → switch-leaf → contract-trust procedure exercised by
the kind harness (`hack/tls-rotate.sh`), never an unattended re-issue of a CA
that every fleet member must trust simultaneously.
