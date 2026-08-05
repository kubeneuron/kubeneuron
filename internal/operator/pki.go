package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/pki"
)

// managedPKILabel marks the Secrets this operator issued and may therefore
// replace.
//
// Its absence is what protects material the operator did not create. A Secret
// filled in by cert-manager, by an organisation's own CA, or by the installer
// before this feature existed is never overwritten — it is only watched, and
// its impending expiry is reported. Renewal has to be opt-in by provenance,
// because silently replacing somebody else's certificate would be worse than
// letting it expire with warning.
const managedPKILabel = "kubeneuron.io/managed-pki"

// PKIRole names one of the four pieces of material an installation needs.
type PKIRole string

const (
	// RoleServer is the controller's serving certificate, and RoleServerCA the
	// authority agents verify it with.
	RoleServer   PKIRole = "controller-tls"
	RoleServerCA PKIRole = "controller-server-ca"
	// RoleClient is the agent fleet's client certificate, and RoleClientCA the
	// authority the controller verifies it with.
	RoleClient   PKIRole = "agent-tls"
	RoleClientCA PKIRole = "agent-client-ca"
)

// PKIPlan is what the operator should do about an installation's TLS material.
type PKIPlan struct {
	// Secrets are the objects to create or update.
	Secrets []*corev1.Secret
	// Warnings describe material the operator does not manage and cannot renew.
	Warnings []string
	// Rotated is true when any managed certificate was reissued, which the
	// caller uses to roll the workloads that mounted the old one.
	Rotated bool
}

// PKIReconcileResult describes the material the runtime must load. Revision is
// a digest of only the Secret keys mounted by workloads; it is safe to put on a
// Pod template and forces processes that cache TLS at startup to roll.
type PKIReconcileResult struct {
	Revisions TLSRevisions
	Rotated   bool
}

// TLSRevisions carries one digest per managed workload, each hashing exactly
// the TLS Secret data THAT workload mounts. A single union digest would roll
// the controller when only the agents' trust changed (and vice versa),
// breaking the rotation protocol's scope guarantee: expanding server trust
// must roll only its consumers.
type TLSRevisions struct {
	// Controller covers the serving leaf, the client CA it verifies agents
	// against, and the optional public serving secret.
	Controller string
	// Agent covers the client leaf and the server CA the agents trust.
	Agent string
}

// CARotationRequiredError keeps CA replacement explicit. Replacing a trust
// root in place creates a mixed fleet that cannot mutually authenticate; use
// the documented expand/activate/retire rotation instead.
type CARotationRequiredError struct {
	Secret string
	Reason string
}

func (e *CARotationRequiredError) Error() string {
	return fmt.Sprintf("managed CA Secret %s requires manual rotation: %s", e.Secret, e.Reason)
}

// pkiInputs is the existing material, keyed by role. A missing entry means the
// Secret does not exist yet.
type pkiInputs map[PKIRole]*corev1.Secret

// ReconcilePKI issues and renews an installation's TLS material.
//
// It returns the revision the caller stamps on workloads mounting this material.
// A pod holding TLS in memory does not notice a Secret update underneath it.
func (r *KubeNeuronReconciler) ReconcilePKI(
	ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, now time.Time,
) (PKIReconcileResult, error) {
	names, complete := pkiSecretNames(installation)
	if !complete {
		return PKIReconcileResult{}, nil
	}
	issuing := installation.Spec.TLS.Issuer == kubeneuronv1alpha1.TLSIssuerOperator
	existing := pkiInputs{}
	for role, name := range names {
		secret := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Namespace: installation.Spec.Namespace, Name: name}, secret)
		switch {
		case err == nil:
			existing[role] = secret
		case apierrors.IsNotFound(err):
			// Absent: issue it below.
		default:
			return PKIReconcileResult{}, fmt.Errorf("reading TLS secret %s: %w", name, err)
		}
	}
	plan, err := planPKI(installation, existing, now)
	if err != nil {
		var rotation *CARotationRequiredError
		if errors.As(err, &rotation) {
			r.event(installation, corev1.EventTypeWarning, "TLSCARotationRequired", rotation.Error())
		}
		return PKIReconcileResult{}, err
	}
	if !issuing {
		// Someone else owns this material. Report what is aging, write nothing.
		for _, warning := range plan.Warnings {
			r.event(installation, corev1.EventTypeWarning, "TLSMaterialExpiring", warning)
		}
		revision, err := r.tlsRevision(ctx, installation, existing, nil)
		if err != nil {
			return PKIReconcileResult{}, err
		}
		return PKIReconcileResult{Revisions: revision}, nil
	}
	for _, warning := range plan.Warnings {
		// Loud, because nothing else will ever say it: material the operator
		// does not own has no renewal path at all.
		r.event(installation, corev1.EventTypeWarning, "TLSMaterialExpiring", warning)
	}
	for _, wanted := range plan.Secrets {
		if err := r.ensurePKISecret(ctx, installation, wanted); err != nil {
			return PKIReconcileResult{}, err
		}
	}
	if plan.Rotated {
		r.event(installation, corev1.EventTypeNormal, "TLSMaterialIssued",
			"TLS material issued or renewed; rolling the workloads that mount it")
	}
	revision, err := r.tlsRevision(ctx, installation, existing, plan.Secrets)
	if err != nil {
		return PKIReconcileResult{}, err
	}
	return PKIReconcileResult{Revisions: revision, Rotated: plan.Rotated}, nil
}

// tlsRevision hashes, PER WORKLOAD, exactly the Secret data that workload
// mounts. It includes external material as well: cert-manager updates also
// require a pod restart because Go loads these files once at process startup.
// The controller and agent digests are deliberately separate — one union
// digest would roll the controller Deployment when only the agents' server-CA
// trust expanded, which the rotation protocol (and its integration assertion)
// forbids: a phase rolls only its consumers.
func (r *KubeNeuronReconciler) tlsRevision(
	ctx context.Context,
	installation *kubeneuronv1alpha1.KubeNeuron,
	existing pkiInputs,
	wanted []*corev1.Secret,
) (TLSRevisions, error) {
	names, complete := pkiSecretNames(installation)
	if !complete {
		return TLSRevisions{}, nil
	}
	replacements := make(map[string]*corev1.Secret, len(wanted))
	for _, secret := range wanted {
		replacements[secret.Name] = secret
	}
	selectSecret := func(role PKIRole) *corev1.Secret {
		if replacement := replacements[names[role]]; replacement != nil {
			return replacement
		}
		return existing[role]
	}
	controllerHash := sha256.New()
	agentHash := sha256.New()
	// Which digest a role feeds follows the volume mounts in resources.go:
	// the controller serves with RoleServer and verifies agents against
	// RoleClientCA; the agent presents RoleClient and trusts RoleServerCA.
	hashFor := map[PKIRole]hash.Hash{
		RoleServer:   controllerHash,
		RoleClientCA: controllerHash,
		RoleClient:   agentHash,
		RoleServerCA: agentHash,
	}
	write := func(h hash.Hash, role, key string, data []byte) {
		_, _ = h.Write([]byte(role))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	caKeys := pkiCAKeys(installation)
	for _, role := range []PKIRole{RoleServer, RoleServerCA, RoleClient, RoleClientCA} {
		h := hashFor[role]
		secret := selectSecret(role)
		if secret == nil {
			write(h, string(role), "missing", nil)
			continue
		}
		if role == RoleServer || role == RoleClient {
			write(h, string(role), corev1.TLSCertKey, secret.Data[corev1.TLSCertKey])
			write(h, string(role), corev1.TLSPrivateKeyKey, secret.Data[corev1.TLSPrivateKeyKey])
			continue
		}
		write(h, string(role), caKeys[role], secret.Data[caKeys[role]])
	}
	// The public serving secret is mounted by the controller only.
	if ref := installation.Spec.TLS.PublicServerSecretRef; ref != nil && ref.Name != "" {
		secret := replacements[ref.Name]
		if secret == nil {
			secret = &corev1.Secret{}
			err := r.Get(ctx, client.ObjectKey{Namespace: installation.Spec.Namespace, Name: ref.Name}, secret)
			if apierrors.IsNotFound(err) {
				secret = nil
			} else if err != nil {
				return TLSRevisions{}, fmt.Errorf("reading public TLS secret %s: %w", ref.Name, err)
			}
		}
		if secret == nil {
			write(controllerHash, "public", "missing", nil)
		} else {
			write(controllerHash, "public", corev1.TLSCertKey, secret.Data[corev1.TLSCertKey])
			write(controllerHash, "public", corev1.TLSPrivateKeyKey, secret.Data[corev1.TLSPrivateKeyKey])
		}
	}
	return TLSRevisions{
		Controller: hex.EncodeToString(controllerHash.Sum(nil)),
		Agent:      hex.EncodeToString(agentHash.Sum(nil)),
	}, nil
}

func (r *KubeNeuronReconciler) ensurePKISecret(
	ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron, wanted *corev1.Secret,
) error {
	actual := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: wanted.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if err := ensureControlledBy(installation, actual, "Secret"); err != nil {
			return err
		}
		actual.Labels = copyStringMap(wanted.Labels)
		actual.Type = wanted.Type
		actual.Data = wanted.Data
		return setOwner(r.Scheme, installation, actual)
	})
	if err != nil {
		return fmt.Errorf("reconcile TLS secret %s: %w", wanted.Name, err)
	}
	return nil
}

// planPKI decides what to issue, what to renew, and what to warn about.
//
// The split by provenance is the whole design. Material this operator issued is
// renewed automatically well before it expires; material somebody else supplied
// is never touched, only reported. Before this existed nothing renewed anything
// at all, and an installation simply stopped being able to authenticate about a
// year after it was created.
func planPKI(installation *kubeneuronv1alpha1.KubeNeuron, existing pkiInputs, now time.Time) (*PKIPlan, error) {
	plan := &PKIPlan{}
	names, ok := pkiSecretNames(installation)
	if !ok {
		// Without a complete TLS block the operator does not know which Secrets
		// the workloads will mount, and guessing would issue material nothing
		// reads. Validation reports the missing references separately.
		return plan, nil
	}

	// The authority has to be settled first: leaves are only worth issuing once
	// there is a signer whose certificate the fleet already trusts.
	keys := pkiCAKeys(installation)
	serverCA, serverCAIssued, err := authorityFor(installation, existing, names, keys, RoleServerCA, plan, now)
	if err != nil {
		return nil, err
	}
	clientCA, clientCAIssued, err := authorityFor(installation, existing, names, keys, RoleClientCA, plan, now)
	if err != nil {
		return nil, err
	}

	controllerDNS := fmt.Sprintf("%s-controller.%s.svc", installation.Name, installation.Spec.Namespace)
	identity, err := url.Parse(fmt.Sprintf("spiffe://kubeneuron.io/installation/%s/agent", installation.UID))
	if err != nil {
		return nil, fmt.Errorf("operator: building agent identity: %w", err)
	}

	if serverCA != nil {
		leaf, reissued, err := leafFor(existing, names, RoleServer, serverCA, serverCAIssued, plan, now, func() (pki.Material, error) {
			return serverCA.IssueServer(controllerDNS, []string{controllerDNS}, now)
		})
		if err != nil {
			return nil, err
		}
		if reissued {
			plan.Secrets = append(plan.Secrets, tlsSecret(installation, names[RoleServer], leaf))
			plan.Rotated = true
		}
	} else {
		warnOrphanedManagedLeaf(existing, names, RoleServer, plan)
	}
	if clientCA != nil {
		leaf, reissued, err := leafFor(existing, names, RoleClient, clientCA, clientCAIssued, plan, now, func() (pki.Material, error) {
			return clientCA.IssueClient(installation.Name+"-agent", identity, now)
		})
		if err != nil {
			return nil, err
		}
		if reissued {
			plan.Secrets = append(plan.Secrets, tlsSecret(installation, names[RoleClient], leaf))
			plan.Rotated = true
		}
	} else {
		warnOrphanedManagedLeaf(existing, names, RoleClient, plan)
	}
	return plan, nil
}

// warnOrphanedManagedLeaf reports a KubeNeuron-managed leaf that sits under a CA
// this operator does not manage. authorityFor returns a nil authority for an
// unmanaged (migrated, or externally supplied) CA, so planPKI skips the leaf
// beneath it entirely — meaning a leftover managed leaf would be neither renewed
// nor mentioned and would silently expire at its 90-day mark, taking fleet mTLS
// down with it. The operator cannot renew it (it does not hold that CA's signing
// key), so the honest outcome is a loud, recurring warning rather than silence.
func warnOrphanedManagedLeaf(existing pkiInputs, names map[PKIRole]string, role PKIRole, plan *PKIPlan) {
	secret, present := existing[role]
	if !present || !isManagedPKI(secret) {
		return
	}
	certPEM := secret.Data[corev1.TLSCertKey]
	if len(certPEM) == 0 {
		return
	}
	if notAfter, err := pki.NotAfter(certPEM); err == nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"Secret %s is a KubeNeuron-managed certificate but its signing CA is not managed by KubeNeuron; nothing will renew it and it expires %s",
			names[role], notAfter.UTC().Format(time.RFC3339)))
		return
	}
	plan.Warnings = append(plan.Warnings, fmt.Sprintf(
		"Secret %s is a KubeNeuron-managed certificate but its signing CA is not managed by KubeNeuron; nothing will renew it and its material is unreadable",
		names[role]))
}

// authorityFor loads or creates the signer for a role. A nil authority means
// somebody else owns this material and the operator must keep its hands off.
func authorityFor(
	installation *kubeneuronv1alpha1.KubeNeuron,
	existing pkiInputs,
	names map[PKIRole]string,
	keys map[PKIRole]string,
	role PKIRole,
	plan *PKIPlan,
	now time.Time,
) (*pki.Authority, bool, error) {
	// The installation chooses which key inside the Secret holds the bundle, and
	// the workloads mount exactly that key. Writing a different one would
	// produce material nothing reads.
	certKey := keys[role]
	secret, present := existing[role]
	if present && !isManagedPKI(secret) {
		if certPEM := secret.Data[certKey]; len(certPEM) != 0 {
			// Warn with the absolute-lead-capped predicate, not the bare 1/3-life
			// fraction: an externally supplied 10-year CA has less than a third of
			// its life left for its final ~3.3 years, and NeedsRenewal would emit a
			// scary "nothing will renew it" warning on every reconcile for years
			// before it actually matters. RenewalDue only fires near real expiry.
			if notAfter, err := pki.NotAfter(certPEM); err == nil && pki.RenewalDue(certPEM, now) {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"Secret %s was not issued by KubeNeuron and expires %s; nothing will renew it",
					names[role], notAfter.UTC().Format(time.RFC3339)))
			}
		}
		return nil, false, nil
	}
	if present {
		authority, err := pki.LoadAuthority(pki.Material{
			CertPEM: secret.Data[certKey], KeyPEM: secret.Data[caPrivateKeyKey],
		})
		if err != nil {
			return nil, false, &CARotationRequiredError{Secret: names[role], Reason: "authority material is unreadable"}
		}
		// Use the absolute-lead-capped predicate, not the bare 1/3-life fraction:
		// a healthy 10-year CA has less than a third of its life left for its final
		// ~3.3 years, and demanding rotation that early turns every reconcile into a
		// hard failure that freezes the config plane. RenewalDue only fires within
		// maxRenewalLead of real expiry.
		if pki.RenewalDue(authority.CertPEM, now) {
			return nil, false, &CARotationRequiredError{Secret: names[role], Reason: "authority is expiring or expired"}
		}
		return authority, false, nil
	}
	authority, err := pki.NewAuthority(names[role], now)
	if err != nil {
		return nil, false, err
	}
	plan.Secrets = append(plan.Secrets, caSecret(installation, names[role], certKey, authority.Material))
	plan.Rotated = true
	return authority, true, nil
}

// leafFor decides whether a leaf must be reissued.
func leafFor(
	existing pkiInputs,
	names map[PKIRole]string,
	role PKIRole,
	authority *pki.Authority,
	authorityChanged bool,
	plan *PKIPlan,
	now time.Time,
	issue func() (pki.Material, error),
) (pki.Material, bool, error) {
	secret, present := existing[role]
	if present && !isManagedPKI(secret) {
		if certPEM := secret.Data[corev1.TLSCertKey]; len(certPEM) != 0 {
			if notAfter, err := pki.NotAfter(certPEM); err == nil && pki.RenewalDue(certPEM, now) {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"Secret %s was not issued by KubeNeuron and expires %s; nothing will renew it",
					names[role], notAfter.UTC().Format(time.RFC3339)))
			}
		}
		return pki.Material{}, false, nil
	}
	if present && !authorityChanged && !pki.NeedsRenewal(secret.Data[corev1.TLSCertKey], now) {
		// The leaf is managed, current, and issued under the same reconcile's
		// authority — but "same reconcile" is not the same as "chains to the CA
		// the fleet trusts now". A crash between writing a new CA Secret and its
		// leaf, or a CA Secret recreated by a partial restore, can leave a
		// still-valid leaf signed by the OLD authority that authorityChanged never
		// catches. Verify provenance against the currently-loaded CA and reissue on
		// mismatch, rather than trust a leaf the controller no longer accepts.
		if authority == nil || authority.VerifyLeaf(secret.Data[corev1.TLSCertKey]) == nil {
			return pki.Material{}, false, nil
		}
	}
	material, err := issue()
	if err != nil {
		return pki.Material{}, false, err
	}
	return material, true, nil
}

func isManagedPKI(secret *corev1.Secret) bool {
	return secret != nil && secret.Labels[managedPKILabel] == "true"
}

// pkiSecretNames mirrors the names the installation's TLS block references, so
// the operator issues into exactly the Secrets the workloads mount.
func pkiSecretNames(installation *kubeneuronv1alpha1.KubeNeuron) (map[PKIRole]string, bool) {
	tls := installation.Spec.TLS
	refs := map[PKIRole]*kubeneuronv1alpha1.SecretReference{
		RoleServer:   tls.ServerSecretRef,
		RoleServerCA: tls.ServerCASecretRef,
		RoleClient:   tls.ClientSecretRef,
		RoleClientCA: tls.ClientCASecretRef,
	}
	names := make(map[PKIRole]string, len(refs))
	for role, ref := range refs {
		if ref == nil || ref.Name == "" {
			return nil, false
		}
		names[role] = ref.Name
	}
	return names, true
}

// tlsSecretNames returns every Secret mounted as TLS material, including the
// optional public listener key pair. It is used for watch routing only and
// therefore tolerates an incomplete TLS block while admission reports it.
func tlsSecretNames(installation *kubeneuronv1alpha1.KubeNeuron) map[string]struct{} {
	refs := []*kubeneuronv1alpha1.SecretReference{
		installation.Spec.TLS.ServerSecretRef,
		installation.Spec.TLS.ServerCASecretRef,
		installation.Spec.TLS.ClientSecretRef,
		installation.Spec.TLS.ClientCASecretRef,
		installation.Spec.TLS.PublicServerSecretRef,
	}
	names := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref != nil && ref.Name != "" {
			names[ref.Name] = struct{}{}
		}
	}
	return names
}

// caPrivateKeyKey holds the authority's signing key beside its certificate. It
// is never mounted into a workload; only the bundle key is.
const caPrivateKeyKey = "ca.key"

// pkiCAKeys reports which key inside each CA Secret holds the bundle.
func pkiCAKeys(installation *kubeneuronv1alpha1.KubeNeuron) map[PKIRole]string {
	return map[PKIRole]string{
		RoleServerCA: caSecretKey(installation.Spec.TLS.ServerCASecretRef),
		RoleClientCA: caSecretKey(installation.Spec.TLS.ClientCASecretRef),
	}
}

func caSecret(installation *kubeneuronv1alpha1.KubeNeuron, name, certKey string, material pki.Material) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: installation.Spec.Namespace,
			Labels:    map[string]string{managedPKILabel: "true"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			certKey: material.CertPEM,
			// The signing key lives beside the authority it belongs to. It never
			// reaches an agent: only the bundle key is mounted into workloads.
			caPrivateKeyKey: material.KeyPEM,
		},
	}
}

func tlsSecret(installation *kubeneuronv1alpha1.KubeNeuron, name string, material pki.Material) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: installation.Spec.Namespace,
			Labels:    map[string]string{managedPKILabel: "true"},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       material.CertPEM,
			corev1.TLSPrivateKeyKey: material.KeyPEM,
		},
	}
}
