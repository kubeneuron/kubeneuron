package operator

import (
	"fmt"
	"path/filepath"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	kubernetesplatform "github.com/kubeneuron/kubeneuron/internal/platform/kubernetes"
)

const (
	controllerPort               = 8080
	agentIngressPort             = 8443
	agentPort                    = 9402
	agentTokenAudience           = "kubeneuron-controller"
	agentTokenExpirationSeconds  = int64(3600)
	defaultSQLiteStorageSize     = "5Gi"
	defaultRevisionHistoryLimit  = 10
	defaultProgressDeadline      = 600
	defaultTerminationGrace      = 30
	defaultConfigMapMode         = 0o644
	defaultSecretMode            = 0o400
	storageRequestAnnotation     = "kubeneuron.io/storage-request"
	storageClassIntentAnnotation = "kubeneuron.io/storage-class-intent"
)

func resourceLabels(installation *kubeneuronv1alpha1.KubeNeuron, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "kube-neuron",
		"app.kubernetes.io/instance":   installation.Name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "kubeneuron-operator",
	}
}

func runtimeConfigMap(installation *kubeneuronv1alpha1.KubeNeuron, snapshot *Snapshot) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      installation.Name + RuntimeConfigSuffix,
			Namespace: installation.Spec.Namespace,
			Labels:    resourceLabels(installation, "controller-config"),
		},
		Data: map[string]string{
			"policies.yaml":        string(snapshot.PoliciesYAML),
			"windows.yaml":         string(snapshot.WindowsYAML),
			"signal-mappings.yaml": string(snapshot.MappingsYAML),
			"node-configs.yaml":    string(snapshot.NodeConfigsYAML),
			"config-digest":        snapshot.Digest,
		},
	}
}

func playbooksConfigMap(installation *kubeneuronv1alpha1.KubeNeuron, snapshot *Snapshot) *corev1.ConfigMap {
	data := make(map[string]string, len(snapshot.Playbooks)+1)
	for name, contents := range snapshot.Playbooks {
		data[name] = string(contents)
	}
	data["config-digest"] = snapshot.Digest
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      installation.Name + PlaybooksConfigSuffix,
			Namespace: installation.Spec.Namespace,
			Labels:    resourceLabels(installation, "playbook-config"),
		},
		Data: data,
	}
}

func controllerServiceAccount(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.ServiceAccount {
	var annotations map[string]string
	// IRSA is AWS-specific and stays scoped to the aws provider block: the
	// operator owns this ServiceAccount, so the role annotation must come from
	// the CR, or a reconcile would strip an annotation the user added by hand and
	// break node recycling. A future provider wires its own identity here without
	// touching the AWS branch.
	if cloud := installation.Spec.Cloud; cloud != nil && cloud.Provider == "aws" && cloud.AWS != nil && cloud.AWS.IAMRoleARN != "" {
		annotations = map[string]string{"eks.amazonaws.com/role-arn": cloud.AWS.IAMRoleARN}
	}
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        installation.Name + "-controller",
			Namespace:   installation.Spec.Namespace,
			Labels:      resourceLabels(installation, "controller"),
			Annotations: annotations,
		},
	}
}

// cloudRegion returns the region declared in the active provider's block. It is
// provider-scoped rather than a shared top-level field, so a future provider
// adds a case here instead of the region living in one cloud's namespace.
func cloudRegion(cloud *kubeneuronv1alpha1.CloudSpec) string {
	switch cloud.Provider {
	case "aws":
		if cloud.AWS != nil {
			return cloud.AWS.Region
		}
	}
	return ""
}

func agentServiceAccount(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      installation.Name + "-agent",
			Namespace: installation.Spec.Namespace,
			Labels:    resourceLabels(installation, "agent"),
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

func controllerClusterRole(installation *kubeneuronv1alpha1.KubeNeuron) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   installation.Name + "-controller",
			Labels: resourceLabels(installation, "controller"),
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, ResourceNames: []string{installation.Name + "-agent"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"pods/eviction"}, Verbs: []string{"create"}},
			{APIGroups: []string{"apps"}, Resources: []string{"daemonsets"}, ResourceNames: []string{installation.Name + "-agent", installation.Name + "-agent-detect"}, Verbs: []string{"get"}},
			{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
			// Operator-API authorization: callers authenticated by TokenReview
			// are admitted only if RBAC lets them get/update this KubeNeuron.
			{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
			// Leader election for the Postgres active/standby pair. Scoped to
			// the exact Lease name; harmless for single-replica SQLite.
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{installation.Name + "-controller"}, Verbs: []string{"get", "update"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		},
	}
}

func controllerClusterRoleBinding(installation *kubeneuronv1alpha1.KubeNeuron) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   installation.Name + "-controller",
			Labels: resourceLabels(installation, "controller"),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     installation.Name + "-controller",
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      installation.Name + "-controller",
			Namespace: installation.Spec.Namespace,
		}},
	}
}

func controllerService(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.Service {
	labels := resourceLabels(installation, "controller")
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      installation.Name + "-controller",
			Namespace: installation.Spec.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       controllerPort,
					TargetPort: intstr.FromString("http"),
				},
				{
					Name:       "agent-mtls",
					Protocol:   corev1.ProtocolTCP,
					Port:       agentIngressPort,
					TargetPort: intstr.FromString("agent-mtls"),
				},
			},
		},
	}
}

func sqliteStorageQuantity(store kubeneuronv1alpha1.WorkflowStoreSpec) (resource.Quantity, error) {
	size := defaultSQLiteStorageSize
	if store.SQLite != nil && store.SQLite.Size != "" {
		size = store.SQLite.Size
	}
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("spec.workflowStore.sqlite.size: %w", err)
	}
	if quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("spec.workflowStore.sqlite.size must be positive")
	}
	return quantity, nil
}

func controllerPVC(installation *kubeneuronv1alpha1.KubeNeuron) (*corev1.PersistentVolumeClaim, error) {
	quantity, err := sqliteStorageQuantity(installation.Spec.WorkflowStore)
	if err != nil {
		return nil, err
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        installation.Name + "-controller-state",
			Namespace:   installation.Spec.Namespace,
			Labels:      resourceLabels(installation, "controller-state"),
			Annotations: map[string]string{storageClassIntentAnnotation: sqliteStorageClassIntent(installation.Spec.WorkflowStore)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  ptr.To(corev1.PersistentVolumeFilesystem),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: quantity,
			}},
		},
	}
	if installation.Spec.WorkflowStore.SQLite != nil {
		claim.Spec.StorageClassName = installation.Spec.WorkflowStore.SQLite.StorageClassName
	}
	return claim, nil
}

func sqliteStorageClassIntent(store kubeneuronv1alpha1.WorkflowStoreSpec) string {
	if store.SQLite == nil || store.SQLite.StorageClassName == nil {
		return "default"
	}
	return "explicit:" + *store.SQLite.StorageClassName
}

// controllerAffinity keeps the controller off the GPU nodes it remediates.
//
// It is not a preference. On a hardware run a reboot playbook rebooted the very
// node the controller was running on: the reboot itself succeeded and the
// boot_id guard prevented a second one, but the playbook was cut off mid-step
// and left the node cordoned. A controller that can be taken down by its own
// remediation cannot finish it.
//
// Nodes are excluded by the presence of a GPU rather than by the installation's
// own selector, because the agent's node set can be widened later without
// anyone remembering to re-check where the controller sits. Scheduling is
// required, not preferred: a cluster whose only nodes have GPUs should make
// that choice explicitly rather than silently inherit the failure mode.
func controllerAffinity(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.Affinity {
	if installation.Spec.Controller.AllowGPUNodes {
		return nil
	}
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      gpuPresentLabel,
						Operator: corev1.NodeSelectorOpNotIn,
						Values:   []string{"true"},
					}},
				}},
			},
		},
	}
}

// gpuPresentLabel is set by NVIDIA's node-feature-discovery on every node that
// has a GPU.
const gpuPresentLabel = "nvidia.com/gpu.present"

func controllerDeployment(installation *kubeneuronv1alpha1.KubeNeuron, snapshot *Snapshot, tlsRevision ...string) (*appsv1.Deployment, error) {
	if err := validateRuntimeSupport(installation); err != nil {
		return nil, err
	}
	annotations := map[string]string{
		"kubeneuron.io/config-digest": snapshot.Digest,
	}
	if len(tlsRevision) != 0 && tlsRevision[0] != "" {
		annotations["kubeneuron.io/tls-digest"] = tlsRevision[0]
	}
	if installation.Spec.WorkflowStore.Type == "SQLite" {
		storageRequest, err := sqliteStorageQuantity(installation.Spec.WorkflowStore)
		if err != nil {
			return nil, err
		}
		annotations[storageRequestAnnotation] = storageRequest.String()
	}
	labels := resourceLabels(installation, "controller")
	args := []string{
		"--listen=:8080",
		"--agent-listen=:8443",
		"--agent-tls-cert=/var/run/secrets/kube-neuron/server-tls/tls.crt",
		"--agent-tls-key=/var/run/secrets/kube-neuron/server-tls/tls.key",
		"--agent-client-ca=/var/run/secrets/kube-neuron/client-ca/ca.crt",
		"--agent-token-audience=" + agentTokenAudience,
		"--agent-token-namespace=" + installation.Spec.Namespace,
		"--agent-token-service-account=" + installation.Name + "-agent",
		// Both agent DaemonSets may authenticate: the primary, and the
		// detection-only companion that exists under Enabled arming. Listing
		// the detect name unconditionally is safe — the authenticator verifies
		// the live owner UID, so a name with no DaemonSet admits nothing.
		"--agent-daemonset=" + installation.Name + "-agent," + installation.Name + "-agent-detect",
		"--installation-name=" + installation.Name,
		"--installation-uid=" + string(installation.UID),
		"--config=/etc/kube-neuron/policies.yaml",
		"--windows=/etc/kube-neuron/windows.yaml",
		"--signal-mappings=/etc/kube-neuron/signal-mappings.yaml",
		"--node-configs=/etc/kube-neuron/node-configs.yaml",
		"--playbooks=/etc/kube-neuron/playbooks",
		"--platform=kubernetes",
	}
	if cloud := installation.Spec.Cloud; cloud != nil && cloud.Provider != "" {
		// The flags are provider-neutral (--cloud-provider/--cloud-region); the
		// region is read from whichever provider block is set.
		args = append(args, "--cloud-provider="+cloud.Provider)
		if region := cloudRegion(cloud); region != "" {
			args = append(args, "--cloud-region="+region)
		}
	}
	if effectiveExecutionMode(installation.Spec.Safety) == kubeneuronv1alpha1.ExecutionModePaused {
		// Paused installs run the full pipeline with the global gate closed;
		// an operator resumes through the authenticated API.
		args = append(args, "--start-paused")
	}
	container := corev1.Container{
		Name:            "controller",
		Image:           installation.Spec.Controller.Image,
		ImagePullPolicy: defaultImagePullPolicy(installation.Spec.Controller.Image),
		Args:            args,
		Resources:       installation.Spec.Controller.Resources,
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: controllerPort, Protocol: corev1.ProtocolTCP},
			{Name: "agent-mtls", ContainerPort: agentIngressPort, Protocol: corev1.ProtocolTCP},
		},
		ReadinessProbe:           defaultHTTPProbe("/healthz", "http"),
		LivenessProbe:            defaultHTTPProbe("/healthz", "http"),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "runtime-config", MountPath: "/etc/kube-neuron"},
			{Name: "playbooks", MountPath: "/etc/kube-neuron/playbooks"},
			{Name: "server-tls", MountPath: "/var/run/secrets/kube-neuron/server-tls", ReadOnly: true},
			{Name: "client-ca", MountPath: "/var/run/secrets/kube-neuron/client-ca", ReadOnly: true},
		},
	}
	if installation.Spec.TLS.PublicServerSecretRef != nil {
		// Probes must speak the listener's protocol once it serves TLS.
		container.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
		container.LivenessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
	}
	volumes := []corev1.Volume{
		{
			Name: "runtime-config",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: installation.Name + RuntimeConfigSuffix},
				DefaultMode:          ptr.To(int32(defaultConfigMapMode)),
			}},
		},
		{
			Name: "playbooks",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: installation.Name + PlaybooksConfigSuffix},
				DefaultMode:          ptr.To(int32(defaultConfigMapMode)),
			}},
		},
		{
			Name: "server-tls",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  installation.Spec.TLS.ServerSecretRef.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items: []corev1.KeyToPath{
					{Key: corev1.TLSCertKey, Path: corev1.TLSCertKey},
					{Key: corev1.TLSPrivateKeyKey, Path: corev1.TLSPrivateKeyKey},
				},
			}},
		},
		{
			Name: "client-ca",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  installation.Spec.TLS.ClientCASecretRef.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items:       []corev1.KeyToPath{{Key: caSecretKey(installation.Spec.TLS.ClientCASecretRef), Path: "ca.crt"}},
			}},
		},
	}

	if ref := installation.Spec.TLS.PublicServerSecretRef; ref != nil {
		args = append(args,
			"--public-tls-cert=/var/run/secrets/kube-neuron/public-tls/tls.crt",
			"--public-tls-key=/var/run/secrets/kube-neuron/public-tls/tls.key")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "public-tls", MountPath: "/var/run/secrets/kube-neuron/public-tls", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "public-tls",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items: []corev1.KeyToPath{
					{Key: corev1.TLSCertKey, Path: corev1.TLSCertKey},
					{Key: corev1.TLSPrivateKeyKey, Path: corev1.TLSPrivateKeyKey},
				},
			}},
		})
	}
	if ref := slackRef(installation); ref != nil {
		args = append(args, "--slack-webhook-file=/var/run/secrets/kube-neuron/notifications-slack/webhook-url")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "notifications-slack", MountPath: "/var/run/secrets/kube-neuron/notifications-slack", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "notifications-slack",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items:       []corev1.KeyToPath{{Key: slackSecretKey(ref), Path: "webhook-url"}},
			}},
		})
	}
	if ref := authUsersRef(installation); ref != nil {
		args = append(args, "--auth-users-dir=/var/run/secrets/kube-neuron/auth-users")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "auth-users", MountPath: "/var/run/secrets/kube-neuron/auth-users", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "auth-users",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
			}},
		})
	}
	if oidc := authOIDC(installation); oidc != nil {
		args = append(args,
			"--oidc-issuer="+oidc.IssuerURL,
			"--oidc-client-id="+oidc.ClientID,
			"--oidc-redirect-url="+oidc.RedirectURL,
			"--oidc-client-secret-file=/var/run/secrets/kube-neuron/oidc/client-secret")
		if len(oidc.AllowedEmailDomains) > 0 {
			args = append(args, "--oidc-allowed-email-domains="+strings.Join(oidc.AllowedEmailDomains, ","))
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "oidc-client", MountPath: "/var/run/secrets/kube-neuron/oidc", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "oidc-client",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  oidc.ClientSecretRef.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items:       []corev1.KeyToPath{{Key: oidcClientSecretKey(oidc.ClientSecretRef), Path: "client-secret"}},
			}},
		})
	}
	if ref := notifyWebhookRef(installation); ref != nil {
		// The "url" key is mandatory; "token" is optional, so the whole
		// Secret is mounted and the token flag is always passed — the
		// controller treats a missing token file as "no bearer credential".
		args = append(args,
			"--notify-webhook-url-file=/var/run/secrets/kube-neuron/notifications-webhook/url")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "notifications-webhook", MountPath: "/var/run/secrets/kube-neuron/notifications-webhook", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "notifications-webhook",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
			}},
		})
	}
	if ref := pagerdutyRef(installation); ref != nil {
		args = append(args, "--pagerduty-routing-key-file=/var/run/secrets/kube-neuron/notifications-pagerduty/routing-key")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "notifications-pagerduty", MountPath: "/var/run/secrets/kube-neuron/notifications-pagerduty", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "notifications-pagerduty",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items:       []corev1.KeyToPath{{Key: pagerdutySecretKey(ref), Path: "routing-key"}},
			}},
		})
	}
	if ref := operatorAPITokenRef(installation); ref != nil {
		// Managed installations always accept per-caller Kubernetes
		// credentials: TokenReview identity plus RBAC on this KubeNeuron
		// object. The static token stays as the break-glass path.
		args = append(args,
			"--api-token-file=/var/run/secrets/kube-neuron/operator-api-token/token",
			"--api-authn-kubernetes")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "operator-api-token", MountPath: "/var/run/secrets/kube-neuron/operator-api-token", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "operator-api-token",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items:       []corev1.KeyToPath{{Key: tokenSecretKey(ref), Path: "token"}},
			}},
		})
	}
	if ref := webhookTokenRef(installation); ref != nil {
		args = append(args, "--webhook-token-file=/var/run/secrets/kube-neuron/webhook-token/token")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "webhook-token", MountPath: "/var/run/secrets/kube-neuron/webhook-token", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "webhook-token",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items:       []corev1.KeyToPath{{Key: tokenSecretKey(ref), Path: "token"}},
			}},
		})
	}

	switch installation.Spec.WorkflowStore.Type {
	case "SQLite":
		args = append(args, "--db=/var/lib/kube-neuron/kubeneuron.db")
		container.Args = args
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "state", MountPath: "/var/lib/kube-neuron"})
		volumes = append(volumes, corev1.Volume{
			Name: "state",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: installation.Name + "-controller-state",
			}},
		})
	case "Postgres":
		// No PVC: PostgreSQL owns durability. The DSN never appears in argv;
		// the controller reads it from the mounted Secret file. A stateless
		// controller runs active/standby: two replicas, Lease election, and
		// readiness that follows leadership so Services reach one writer.
		args = append(args,
			"--store=postgres",
			"--postgres-dsn-file=/var/run/secrets/kube-neuron/postgres-dsn/dsn",
			"--leader-elect")
		container.Args = args
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			}},
			corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			}},
		)
		container.ReadinessProbe = defaultHTTPProbe("/readyz", "http")
		if installation.Spec.TLS.PublicServerSecretRef != nil {
			container.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "postgres-dsn", MountPath: "/var/run/secrets/kube-neuron/postgres-dsn", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "postgres-dsn",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  installation.Spec.WorkflowStore.SecretRef.Name,
				DefaultMode: ptr.To(int32(defaultSecretMode)),
				Items:       []corev1.KeyToPath{{Key: dsnSecretKey(installation.Spec.WorkflowStore.SecretRef), Path: "dsn"}},
			}},
		})
	default:
		return nil, fmt.Errorf("unsupported workflow store %q", installation.Spec.WorkflowStore.Type)
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        installation.Name + "-controller",
			Namespace:   installation.Spec.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.To(controllerReplicas(installation)),
			Strategy:                controllerStrategy(installation),
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			RevisionHistoryLimit:    ptr.To(int32(defaultRevisionHistoryLimit)),
			ProgressDeadlineSeconds: ptr.To(int32(defaultProgressDeadline)),
			Template: corev1.PodTemplateSpec{
				// The pod template deliberately omits the config-digest. The
				// controller reloads configuration in place from the mounted
				// ConfigMap (see applyRuntimeConfig), so stamping the digest here
				// would only trigger a Deployment rollout — which, under leader
				// election, deadlocks: only the leader is Ready, so a new pod can
				// never become Ready to retire the old one, and the stale-config
				// leader would serve forever. The digest stays on the Deployment
				// object for observability.
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: controllerPodAnnotations(annotations)},
				Spec: corev1.PodSpec{
					ServiceAccountName:            installation.Name + "-controller",
					DeprecatedServiceAccount:      installation.Name + "-controller",
					Affinity:                      controllerAffinity(installation),
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: ptr.To(int64(defaultTerminationGrace)),
					DNSPolicy:                     corev1.DNSClusterFirst,
					// The controller image runs as the distroless nonroot user;
					// fsGroup makes CSI-provisioned volumes (EBS and friends,
					// which mount root-owned) writable for the SQLite store.
					// kind's permissive local-path storage masked this.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(int64(65532)),
						RunAsGroup:   ptr.To(int64(65532)),
						FSGroup:      ptr.To(int64(65532)),
					},
					SchedulerName: corev1.DefaultSchedulerName,
					Containers:    []corev1.Container{container},
					Volumes:       volumes,
				},
			},
		},
	}, nil
}

// agentHostToolingWiring renders the opt-in NVIDIA host-tooling mounts: the
// host bin directory joins PATH (exec.LookPath finds nvidia-smi/dcgmi), the
// driver library directories become LD_LIBRARY_PATH (inherited by the child
// processes the agent execs), and an optional scripts directory lands at the
// agent's --scripts-dir. Everything is mounted read-only at stable /host/...
// container paths so a hostile node image cannot shadow agent binaries.
// hostToolingBinMount is where the node's tooling directory appears inside the
// agent container. It is a fixed path so a hostile node image cannot shadow
// agent binaries by choosing a clever binDir.
const hostToolingBinMount = "/host/nvidia/bin"

func agentHostToolingWiring(tooling *kubeneuronv1alpha1.HostToolingSpec) (args []string, env []corev1.EnvVar, mounts []corev1.VolumeMount, volumes []corev1.Volume) {
	if tooling == nil {
		return nil, nil, nil, nil
	}
	binDir := tooling.BinDir
	if binDir == "" {
		binDir = "/usr/bin"
	}
	libDirs := tooling.LibDirs
	if len(libDirs) == 0 {
		libDirs = []string{"/usr/lib64"}
	}
	mounts = append(mounts, corev1.VolumeMount{Name: "nvidia-bin", MountPath: hostToolingBinMount, ReadOnly: true})
	volumes = append(volumes, corev1.Volume{Name: "nvidia-bin", VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: binDir, Type: hostPathType(corev1.HostPathDirectory)},
	}})
	ldPaths := make([]string, 0, len(libDirs))
	for i, dir := range libDirs {
		name := fmt.Sprintf("nvidia-lib-%d", i)
		containerPath := fmt.Sprintf("/host/nvidia/lib/%d", i)
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: containerPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: dir, Type: hostPathType(corev1.HostPathDirectory)},
		}})
		ldPaths = append(ldPaths, containerPath)
	}
	// The scratch agent image has no ELF interpreter, and exec of a
	// dynamically linked nvidia-smi resolves PT_INTERP
	// (/lib64/ld-linux-x86-64.so.2) before LD_LIBRARY_PATH is ever
	// consulted. The first library directory must therefore also appear at
	// /lib64 — on AL2023 (and other merged-usr distributions) /usr/lib64
	// contains the loader, so the default satisfies this.
	mounts = append(mounts, corev1.VolumeMount{Name: "nvidia-lib-0", MountPath: "/lib64", ReadOnly: true})
	env = append(env,
		corev1.EnvVar{Name: "PATH", Value: hostToolingBinMount + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		corev1.EnvVar{Name: "LD_LIBRARY_PATH", Value: strings.Join(ldPaths, ":")},
	)
	// Host tooling declared means a Fake-driver fallback is a configuration
	// error, not a tolerable degradation.
	args = append(args, "--require-real-driver")
	if endpoint := strings.TrimSpace(tooling.DCGMEndpoint); endpoint != "" {
		args = append(args, "--nvidia-dcgm-endpoint="+endpoint)
	}
	// Only an explicit request switches attestation to the node's own client.
	// The agent's default is the client in its image, so the attested version
	// is the same on every node regardless of how each one was provisioned.
	// binDir is the only host directory mounted, so the client must live in it;
	// the compiler rejects anything else rather than passing a path that would
	// not exist in the container.
	if path := strings.TrimSpace(tooling.DCGMIPath); path != "" {
		args = append(args, "--nvidia-dcgmi="+hostToolingBinMount+"/"+filepath.Base(path))
	}
	if tooling.ScriptsDir != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "scripts", MountPath: "/etc/kube-neuron/scripts", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "scripts", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: tooling.ScriptsDir, Type: hostPathType(corev1.HostPathDirectory)},
		}})
		args = append(args, "--scripts-dir=/etc/kube-neuron/scripts")
	}
	return args, env, mounts, volumes
}

// agentDaemonSet builds the ONE agent DaemonSet, covering the whole agent
// fleet on the base spec.agent.nodeSelector in every execution mode. Arming
// is no longer scheduling geometry: the agent boots unarmed and adopts the
// controller-served arming answer delivered with each v2 registration
// response (computed from spec.safety.destructiveExecution.nodeSelector
// against the node's live labels), so no --enable-destructive-actions arg is
// rendered, no blast-radius narrowing is applied, and no detection-only
// companion DaemonSet exists anymore. The retired "-agent-detect" companion
// from the two-DaemonSet era is removed by the reconciler on upgrade.
// agentTolerations is the operator's declared agent tolerations plus one this
// product owes itself.
//
// spec.safety.taintDegradedNodes can place kubeneuron.io/degraded with effect
// NoSchedule, and nothing in this repository declared a toleration for it —
// the DaemonSet controller auto-tolerates the seven built-in node-condition
// taints and no custom key. NoSchedule does not evict, so the steady state was
// fine; the trap is any event that needs the agent pod RE-SCHEDULED on a
// degraded node — a config-digest change rolling the DaemonSet, a manual pod
// delete, an API-initiated eviction. The new pod could not be placed, the
// taint is only removed when the incident halts, and the incident cannot halt
// without the agent.
//
// No effect is specified, so it covers whichever effect the installation
// chose, including one added later.
func agentTolerations(installation *kubeneuronv1alpha1.KubeNeuron) []corev1.Toleration {
	out := append([]corev1.Toleration(nil), installation.Spec.Agent.Tolerations...)
	for _, t := range out {
		if t.Key == kubernetesplatform.DegradedTaintKey {
			return out // the operator declared their own; do not duplicate it
		}
	}
	return append(out, corev1.Toleration{
		Key:      kubernetesplatform.DegradedTaintKey,
		Operator: corev1.TolerationOpExists,
	})
}

func agentDaemonSet(installation *kubeneuronv1alpha1.KubeNeuron, snapshot *Snapshot, tlsRevision ...string) *appsv1.DaemonSet {
	labels := resourceLabels(installation, "agent")
	privileged := true
	toolingArgs, toolingEnv, toolingMounts, toolingVolumes := agentHostToolingWiring(installation.Spec.Agent.HostTooling)
	agentNodes := copyStringMap(installation.Spec.Agent.NodeSelector)
	annotations := map[string]string{"kubeneuron.io/config-digest": snapshot.Digest}
	if len(tlsRevision) != 0 && tlsRevision[0] != "" {
		annotations["kubeneuron.io/tls-digest"] = tlsRevision[0]
	}
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        installation.Name + "-agent",
			Namespace:   installation.Spec.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector:             &metav1.LabelSelector{MatchLabels: labels},
			RevisionHistoryLimit: ptr.To(int32(defaultRevisionHistoryLimit)),
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: ptr.To(intstr.FromInt(1)),
					MaxSurge:       ptr.To(intstr.FromInt(0)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: copyStringMap(annotations)},
				Spec: corev1.PodSpec{
					ServiceAccountName:            installation.Name + "-agent",
					DeprecatedServiceAccount:      installation.Name + "-agent",
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: ptr.To(int64(defaultTerminationGrace)),
					DNSPolicy:                     corev1.DNSClusterFirst,
					SecurityContext:               &corev1.PodSecurityContext{},
					SchedulerName:                 corev1.DefaultSchedulerName,
					HostPID:                       true,
					NodeSelector:                  agentNodes,
					Tolerations:                   agentTolerations(installation),
					Containers: []corev1.Container{{
						Name:            "agent",
						Image:           installation.Spec.Agent.Image,
						ImagePullPolicy: defaultImagePullPolicy(installation.Spec.Agent.Image),
						Args: append([]string{
							"--controller=https://" + installation.Name + "-controller." + installation.Spec.Namespace + ".svc:8443",
							"--token-file=/var/run/secrets/kube-neuron/identity/token",
							"--tls-ca=/var/run/secrets/kube-neuron/server-ca/ca.crt",
							"--tls-cert=/var/run/secrets/kube-neuron/client-tls/tls.crt",
							"--tls-key=/var/run/secrets/kube-neuron/client-tls/tls.key",
							"--node=$(NODE_NAME)",
							"--spool=/var/lib/kube-neuron/spool.jsonl",
							"--health-listen=:9402",
							"--registration-interval=30s",
							"--registration-stale-after=90s",
							"--nvidia-observation",
							"--nvidia-controller-profile",
						}, toolingArgs...),
						Env: append([]corev1.EnvVar{{
							Name: "NODE_NAME",
							ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "spec.nodeName",
							}},
						}}, toolingEnv...),
						SecurityContext:          &corev1.SecurityContext{Privileged: &privileged},
						Ports:                    []corev1.ContainerPort{{Name: "health", ContainerPort: agentPort, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe:           defaultReadinessHTTPProbe("/readyz", "health"),
						LivenessProbe:            defaultHTTPProbe("/livez", "health"),
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						VolumeMounts: append([]corev1.VolumeMount{
							{Name: "kmsg", MountPath: "/dev/kmsg", ReadOnly: true},
							{Name: "state", MountPath: "/var/lib/kube-neuron"},
							{Name: "client-tls", MountPath: "/var/run/secrets/kube-neuron/client-tls", ReadOnly: true},
							{Name: "server-ca", MountPath: "/var/run/secrets/kube-neuron/server-ca", ReadOnly: true},
							{Name: "identity", MountPath: "/var/run/secrets/kube-neuron/identity", ReadOnly: true},
						}, toolingMounts...),
					}},
					Volumes: append([]corev1.Volume{
						{Name: "kmsg", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kmsg", Type: hostPathType(corev1.HostPathUnset)}}},
						{Name: "state", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kube-neuron", Type: hostPathType(corev1.HostPathDirectoryOrCreate)}}},
						{
							Name: "client-tls",
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName:  installation.Spec.TLS.ClientSecretRef.Name,
								DefaultMode: ptr.To(int32(defaultSecretMode)),
								Items: []corev1.KeyToPath{
									{Key: corev1.TLSCertKey, Path: corev1.TLSCertKey},
									{Key: corev1.TLSPrivateKeyKey, Path: corev1.TLSPrivateKeyKey},
								},
							}},
						},
						{
							Name: "server-ca",
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName:  installation.Spec.TLS.ServerCASecretRef.Name,
								DefaultMode: ptr.To(int32(defaultSecretMode)),
								Items:       []corev1.KeyToPath{{Key: caSecretKey(installation.Spec.TLS.ServerCASecretRef), Path: "ca.crt"}},
							}},
						},
						{
							Name: "identity",
							VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
								DefaultMode: ptr.To(int32(defaultSecretMode)),
								Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
									Audience:          agentTokenAudience,
									ExpirationSeconds: ptr.To(agentTokenExpirationSeconds),
									Path:              "token",
								}}},
							}},
						},
					}, toolingVolumes...),
				},
			},
		},
	}
}

// controllerPDB guards the single controller replica. minAvailable=1 on a
// one-replica Deployment deliberately blocks voluntary node drains: evicting
// the controller mid-remediation would strand in-flight incidents, and the
// RWO SQLite claim pins it to one node anyway. Administrators drain that
// node by scaling/relocating the installation first (or deleting the Pod
// directly); this is a conscious trade against automated node maintenance.
func controllerPDB(installation *kubeneuronv1alpha1.KubeNeuron) *policyv1.PodDisruptionBudget {
	labels := resourceLabels(installation, "controller")
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      installation.Name + "-controller",
			Namespace: installation.Spec.Namespace,
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt(1)),
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}

// controllerReplicas: SQLite pins one writer to one RWO volume; Postgres
// runs an elected active/standby pair.
func controllerReplicas(installation *kubeneuronv1alpha1.KubeNeuron) int32 {
	if installation.Spec.WorkflowStore.Type == "Postgres" {
		return 2
	}
	return 1
}

// controllerStrategy is Recreate for both stores.
//
// Config changes never reach here — they reload in place — so the only thing
// that rolls the controller now is a genuine spec change, chiefly an image
// upgrade. Recreate is required for that under leader election: a RollingUpdate
// waits for the new pods to become Ready before retiring the old, but only the
// leader is ever Ready, so it would deadlock and the old image would serve
// forever. Recreate deletes the old pods first, which frees the Lease so a new
// pod can win it and become Ready. The cost is a brief control-plane gap during
// an upgrade — acceptable because incidents are durable and agents retry, and
// far better than an upgrade that cannot complete at all.
func controllerStrategy(installation *kubeneuronv1alpha1.KubeNeuron) appsv1.DeploymentStrategy {
	return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
}

// controllerPodAnnotations returns the Deployment annotations without the
// config-digest, so an ordinary configuration change does not roll the pods.
func controllerPodAnnotations(annotations map[string]string) map[string]string {
	out := copyStringMap(annotations)
	delete(out, "kubeneuron.io/config-digest")
	return out
}

func hostPathType(value corev1.HostPathType) *corev1.HostPathType { return &value }

func defaultHTTPProbe(path, port string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   path,
			Port:   intstr.FromString(port),
			Scheme: corev1.URISchemeHTTP,
		}},
		TimeoutSeconds:   1,
		PeriodSeconds:    10,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
}

func defaultReadinessHTTPProbe(path, port string) *corev1.Probe {
	probe := defaultHTTPProbe(path, port)
	// Propagate the agent's strict stale threshold after the next probe rather
	// than keeping the Pod Ready through three consecutive stale results.
	probe.FailureThreshold = 1
	return probe
}

func defaultImagePullPolicy(image string) corev1.PullPolicy {
	name := image
	hasDigest := false
	if at := strings.LastIndex(name, "@"); at >= 0 {
		name = name[:at]
		hasDigest = true
	}
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon <= lastSlash {
		if hasDigest {
			return corev1.PullIfNotPresent
		}
		return corev1.PullAlways
	}
	if name[lastColon+1:] == "latest" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func setOwner(scheme *runtime.Scheme, installation *kubeneuronv1alpha1.KubeNeuron, object client.Object) error {
	return controllerutil.SetControllerReference(installation, object, scheme)
}
