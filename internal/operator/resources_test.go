package operator

import (
	"context"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

func TestControllerRBACTargetsManagedServiceAccount(t *testing.T) {
	installation := testKubeNeuron()
	role := controllerClusterRole(installation)
	if role.Name != "fleet-controller" {
		t.Fatalf("ClusterRole name = %q, want fleet-controller", role.Name)
	}
	if len(role.Rules) != 9 {
		t.Fatalf("ClusterRole has %d rules, want 9", len(role.Rules))
	}
	if !rulesCover(role.Rules, rbacv1.PolicyRule{
		APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"},
	}) {
		t.Fatal("controller ClusterRole cannot create TokenReviews")
	}
	if !rulesCover(role.Rules, rbacv1.PolicyRule{
		APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"},
	}) {
		t.Fatal("controller ClusterRole cannot create SubjectAccessReviews")
	}
	for _, wanted := range []struct {
		apiGroup string
		resource string
	}{
		{apiGroup: "", resource: "serviceaccounts"},
		{apiGroup: "apps", resource: "daemonsets"},
	} {
		rule := findRule(role.Rules, wanted.apiGroup, wanted.resource)
		if rule == nil || len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "fleet-agent" {
			t.Errorf("controller %s rule = %#v, want only resourceName fleet-agent", wanted.resource, rule)
		}
	}

	binding := controllerClusterRoleBinding(installation)
	if binding.RoleRef.Name != role.Name {
		t.Fatalf("RoleRef name = %q, want %q", binding.RoleRef.Name, role.Name)
	}
	if len(binding.Subjects) != 1 {
		t.Fatalf("binding has %d subjects, want 1", len(binding.Subjects))
	}
	subject := binding.Subjects[0]
	if subject.Name != "fleet-controller" || subject.Namespace != installation.Spec.Namespace {
		t.Fatalf("binding subject = %#v", subject)
	}
}

func TestAgentServiceAccountDisablesImplicitTokenMount(t *testing.T) {
	account := agentServiceAccount(testKubeNeuron())
	if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
		t.Fatalf("agent automountServiceAccountToken = %v, want false", account.AutomountServiceAccountToken)
	}
}

func TestOperatorRBACCanReconcilePVCAndDelegateControllerRole(t *testing.T) {
	data, err := os.ReadFile("../../config/rbac/operator_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var operatorRole rbacv1.ClusterRole
	if err := sigsyaml.Unmarshal(data, &operatorRole); err != nil {
		t.Fatal(err)
	}

	required := append([]rbacv1.PolicyRule(nil), controllerClusterRole(testKubeNeuron()).Rules...)
	required = append(required, rbacv1.PolicyRule{
		APIGroups: []string{""},
		Resources: []string{"persistentvolumeclaims"},
		Verbs:     []string{"create", "get", "list", "update", "watch"},
	})
	required = append(required, rbacv1.PolicyRule{
		APIGroups: []string{""},
		Resources: []string{"namespaces"},
		Verbs:     []string{"get"},
	})
	for _, wanted := range required {
		if !rulesCover(operatorRole.Rules, wanted) {
			t.Errorf("operator ClusterRole does not cover %#v", wanted)
		}
	}
	for _, rule := range operatorRole.Rules {
		if containsString(rule.Verbs, "escalate") || containsString(rule.Verbs, "bind") {
			t.Errorf("operator ClusterRole must not grant bind/escalate: %#v", rule)
		}
		if containsString(rule.Resources, "namespaces") {
			for _, forbidden := range []string{"create", "update", "patch", "delete"} {
				if containsString(rule.Verbs, forbidden) || containsString(rule.Verbs, "*") {
					t.Errorf("operator must not manage namespaces with %q: %#v", forbidden, rule)
				}
			}
		}
	}
}

func TestLeaderElectionRoleCanRecordOnlyNamespacedEvents(t *testing.T) {
	data, err := os.ReadFile("../../config/rbac/leader_election_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	if err := sigsyaml.Unmarshal(data, &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != "kube-neuron" {
		t.Fatalf("leader-election Role namespace = %q, want kube-neuron", role.Namespace)
	}
	for _, wanted := range []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create", "get", "update"}},
	} {
		if !rulesCover(role.Rules, wanted) {
			t.Errorf("leader-election Role does not cover %#v", wanted)
		}
	}
	for _, rule := range role.Rules {
		if containsString(rule.Resources, "leases") {
			for _, unused := range []string{"list", "patch", "watch"} {
				if containsString(rule.Verbs, unused) || containsString(rule.Verbs, "*") {
					t.Errorf("leader-election Role grants unused Lease verb %q: %#v", unused, rule)
				}
			}
		}
	}
}

func rulesCover(rules []rbacv1.PolicyRule, wanted rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		if containsAll(rule.APIGroups, wanted.APIGroups) &&
			containsAll(rule.Resources, wanted.Resources) &&
			containsAll(rule.Verbs, wanted.Verbs) {
			return true
		}
	}
	return false
}

func findRule(rules []rbacv1.PolicyRule, apiGroup, resource string) *rbacv1.PolicyRule {
	for i := range rules {
		if containsString(rules[i].APIGroups, apiGroup) && containsString(rules[i].Resources, resource) {
			return &rules[i]
		}
	}
	return nil
}

func TestControllerPVCDefaultsAndCustomStorage(t *testing.T) {
	installation := testKubeNeuron()
	claim, err := controllerPVC(installation)
	if err != nil {
		t.Fatalf("controllerPVC() error = %v", err)
	}
	if claim.Name != "fleet-controller-state" || claim.Namespace != installation.Spec.Namespace {
		t.Fatalf("claim key = %s/%s", claim.Namespace, claim.Name)
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("access modes = %v, want ReadWriteOnce", claim.Spec.AccessModes)
	}
	if claim.Spec.VolumeMode == nil || *claim.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		t.Fatalf("volume mode = %v, want Filesystem", claim.Spec.VolumeMode)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Fatalf("default storage request = %s, want 5Gi", got.String())
	}
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("default storageClassName = %v, want nil", claim.Spec.StorageClassName)
	}
	if got := claim.Annotations[storageClassIntentAnnotation]; got != "default" {
		t.Fatalf("default storage-class intent = %q, want default", got)
	}

	noProvisioner := ""
	installation.Spec.WorkflowStore.SQLite = &kubeneuronv1alpha1.SQLiteStoreSpec{
		Size:             "10Gi",
		StorageClassName: &noProvisioner,
	}
	claim, err = controllerPVC(installation)
	if err != nil {
		t.Fatalf("controllerPVC() custom error = %v", err)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("custom storage request = %s, want 10Gi", got.String())
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "" {
		t.Fatalf("explicit empty storageClassName = %v, want pointer to empty string", claim.Spec.StorageClassName)
	}
	if got := claim.Annotations[storageClassIntentAnnotation]; got != "explicit:" {
		t.Fatalf("explicit empty storage-class intent = %q, want explicit:", got)
	}
}

func TestControllerPVCRejectsInvalidStorageSize(t *testing.T) {
	for _, size := range []string{"not-a-quantity", "0", "-1Gi"} {
		t.Run(size, func(t *testing.T) {
			installation := testKubeNeuron()
			installation.Spec.WorkflowStore.SQLite = &kubeneuronv1alpha1.SQLiteStoreSpec{Size: size}
			if _, err := controllerPVC(installation); err == nil {
				t.Fatalf("controllerPVC() size %q returned nil error", size)
			}
		})
	}
}

func TestSQLiteControllerDeploymentUsesKubeNeuronStatePath(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.TLS.ClientCASecretRef.Key = "clients.pem"
	deployment, err := controllerDeployment(installation, &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatalf("controllerDeployment() error = %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if !containsString(container.Args, "--db=/var/lib/kube-neuron/kubeneuron.db") {
		t.Fatalf("controller args do not contain KubeNeuron database path: %v", container.Args)
	}
	for _, arg := range []string{
		"--agent-listen=:8443",
		"--agent-tls-cert=/var/run/secrets/kube-neuron/server-tls/tls.crt",
		"--agent-tls-key=/var/run/secrets/kube-neuron/server-tls/tls.key",
		"--agent-client-ca=/var/run/secrets/kube-neuron/client-ca/ca.crt",
		"--agent-token-audience=kubeneuron-controller",
		"--agent-token-namespace=kube-neuron-system",
		"--agent-token-service-account=fleet-agent",
		"--agent-daemonset=fleet-agent",
		"--installation-name=fleet",
		"--installation-uid=installation-uid",
	} {
		if !containsString(container.Args, arg) {
			t.Errorf("controller args are missing %q: %v", arg, container.Args)
		}
	}
	if len(container.Ports) != 2 || container.Ports[0].Name != "http" || container.Ports[1].Name != "agent-mtls" ||
		container.Ports[1].ContainerPort != agentIngressPort {
		t.Fatalf("controller ports = %#v", container.Ports)
	}
	serverTLS := findVolume(deployment.Spec.Template.Spec.Volumes, "server-tls")
	if serverTLS == nil || serverTLS.Secret == nil || serverTLS.Secret.SecretName != "fleet-controller-tls" {
		t.Fatalf("server TLS volume = %#v", serverTLS)
	}
	assertSecretProjection(t, serverTLS.Secret, defaultSecretMode, map[string]string{
		corev1.TLSCertKey:       corev1.TLSCertKey,
		corev1.TLSPrivateKeyKey: corev1.TLSPrivateKeyKey,
	})
	clientCA := findVolume(deployment.Spec.Template.Spec.Volumes, "client-ca")
	if clientCA == nil || clientCA.Secret == nil || clientCA.Secret.SecretName != "fleet-agent-client-ca" {
		t.Fatalf("client CA volume = %#v", clientCA)
	}
	assertSecretProjection(t, clientCA.Secret, defaultSecretMode, map[string]string{"clients.pem": "ca.crt"})
	for _, name := range []string{"server-tls", "client-ca"} {
		mount := findVolumeMount(container.VolumeMounts, name)
		if mount == nil || !mount.ReadOnly {
			t.Errorf("controller %s mount = %#v, want read-only", name, mount)
		}
	}
	if got := deployment.Spec.Template.Annotations["kubeneuron.io/config-digest"]; got != "abc123" {
		t.Fatalf("config digest annotation = %q, want abc123", got)
	}
	if got := deployment.Spec.Template.Annotations[storageRequestAnnotation]; got != "5Gi" {
		t.Fatalf("storage request annotation = %q, want 5Gi", got)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("deployment strategy = %q, want Recreate for single-writer SQLite", deployment.Spec.Strategy.Type)
	}
	stateMount := findVolumeMount(container.VolumeMounts, "state")
	if stateMount == nil || stateMount.MountPath != "/var/lib/kube-neuron" || stateMount.ReadOnly {
		t.Fatalf("controller state mount = %#v", stateMount)
	}
	stateVolume := findVolume(deployment.Spec.Template.Spec.Volumes, "state")
	if stateVolume == nil || stateVolume.PersistentVolumeClaim == nil ||
		stateVolume.PersistentVolumeClaim.ClaimName != "fleet-controller-state" {
		t.Fatalf("controller state volume = %#v", stateVolume)
	}
}

func findVolumeMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func assertSecretProjection(t *testing.T, source *corev1.SecretVolumeSource, mode int32, want map[string]string) {
	t.Helper()
	if source.DefaultMode == nil || *source.DefaultMode != mode {
		t.Fatalf("Secret %q defaultMode = %v, want %#o", source.SecretName, source.DefaultMode, mode)
	}
	if len(source.Items) != len(want) {
		t.Fatalf("Secret %q items = %#v, want %#v", source.SecretName, source.Items, want)
	}
	for _, item := range source.Items {
		if path, ok := want[item.Key]; !ok || path != item.Path {
			t.Errorf("Secret %q item = %#v, want %#v", source.SecretName, item, want)
		}
	}
}

func TestControllerDeploymentRejectsUnsupportedRuntimeConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*kubeneuronv1alpha1.KubeNeuron)
		want   string
	}{
		{
			name: "enabled execution without notifier",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModeEnabled
			},
			want: "requires spec.safety.destructiveExecution",
		},
		{
			name: "Postgres store without a DSN secret",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.WorkflowStore.Type = "Postgres"
				installation.Spec.WorkflowStore.SQLite = nil
			},
			want: "secretRef is required for Postgres",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installation := testKubeNeuron()
			tt.mutate(installation)
			_, err := controllerDeployment(installation, &Snapshot{Digest: "abc123"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("controllerDeployment() error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestDefaultImagePullPolicyMatchesKubernetesDefaulting(t *testing.T) {
	tests := []struct {
		image string
		want  corev1.PullPolicy
	}{
		{image: "registry.example/repository", want: corev1.PullAlways},
		{image: "registry.example/repository:latest", want: corev1.PullAlways},
		{image: "localhost:5000/repository", want: corev1.PullAlways},
		{image: "registry.example/repository:v1", want: corev1.PullIfNotPresent},
		{image: "registry.example/repository@sha256:abcdef", want: corev1.PullIfNotPresent},
		{image: "registry.example/repository:latest@sha256:abcdef", want: corev1.PullAlways},
		{image: "registry.example/repository:v1@sha256:abcdef", want: corev1.PullIfNotPresent},
	}
	for _, tt := range tests {
		if got := defaultImagePullPolicy(tt.image); got != tt.want {
			t.Errorf("defaultImagePullPolicy(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestAgentDaemonSetReportsRegistrationReadiness(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.TLS.ServerCASecretRef.Key = "servers.pem"
	agents := agentDaemonSet(installation, &Snapshot{Digest: "abc123"})
	if len(agents.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("agent containers = %d, want 1", len(agents.Spec.Template.Spec.Containers))
	}
	container := agents.Spec.Template.Spec.Containers[0]
	wantArgs := []string{
		"--controller=https://fleet-controller.kube-neuron-system.svc:8443",
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
	}
	if strings.Join(container.Args, "\n") != strings.Join(wantArgs, "\n") {
		t.Fatalf("agent args = %q, want %q", container.Args, wantArgs)
	}
	if len(container.Ports) != 1 {
		t.Fatalf("agent ports = %#v, want one health port", container.Ports)
	}
	port := container.Ports[0]
	if port.Name != "health" || port.ContainerPort != agentPort || port.Protocol != corev1.ProtocolTCP {
		t.Fatalf("agent health port = %#v, want named TCP port health:%d", port, agentPort)
	}
	assertHTTPProbe(t, container.LivenessProbe, "/livez", "health", 3)
	assertHTTPProbe(t, container.ReadinessProbe, "/readyz", "health", 1)
	if agents.Spec.Template.Spec.AutomountServiceAccountToken != nil {
		t.Fatalf("Pod automountServiceAccountToken = %v, want inherited false from the managed ServiceAccount", *agents.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	for _, name := range []string{"client-tls", "server-ca", "identity"} {
		mount := findVolumeMount(container.VolumeMounts, name)
		if mount == nil || !mount.ReadOnly {
			t.Errorf("agent %s mount = %#v, want read-only", name, mount)
		}
	}
	identity := findVolume(agents.Spec.Template.Spec.Volumes, "identity")
	if identity == nil || identity.Projected == nil || len(identity.Projected.Sources) != 1 ||
		identity.Projected.Sources[0].ServiceAccountToken == nil {
		t.Fatalf("identity projected volume = %#v", identity)
	}
	projection := identity.Projected.Sources[0].ServiceAccountToken
	if projection.Audience != agentTokenAudience || projection.Path != "token" ||
		projection.ExpirationSeconds == nil || *projection.ExpirationSeconds != agentTokenExpirationSeconds {
		t.Fatalf("identity token projection = %#v", projection)
	}
	if identity.Projected.DefaultMode == nil || *identity.Projected.DefaultMode != defaultSecretMode {
		t.Fatalf("identity defaultMode = %v, want %#o", identity.Projected.DefaultMode, defaultSecretMode)
	}
	clientTLS := findVolume(agents.Spec.Template.Spec.Volumes, "client-tls")
	if clientTLS == nil || clientTLS.Secret == nil || clientTLS.Secret.SecretName != "fleet-agent-tls" {
		t.Fatalf("client TLS volume = %#v", clientTLS)
	}
	assertSecretProjection(t, clientTLS.Secret, defaultSecretMode, map[string]string{
		corev1.TLSCertKey:       corev1.TLSCertKey,
		corev1.TLSPrivateKeyKey: corev1.TLSPrivateKeyKey,
	})
	serverCA := findVolume(agents.Spec.Template.Spec.Volumes, "server-ca")
	if serverCA == nil || serverCA.Secret == nil || serverCA.Secret.SecretName != "fleet-controller-server-ca" {
		t.Fatalf("server CA volume = %#v", serverCA)
	}
	assertSecretProjection(t, serverCA.Secret, defaultSecretMode, map[string]string{"servers.pem": "ca.crt"})
}

func TestControllerServiceSeparatesPublicAndAgentPorts(t *testing.T) {
	service := controllerService(testKubeNeuron())
	if len(service.Spec.Ports) != 2 {
		t.Fatalf("service ports = %#v, want two", service.Spec.Ports)
	}
	if service.Spec.Ports[0].Name != "http" || service.Spec.Ports[0].Port != controllerPort ||
		service.Spec.Ports[0].Protocol != corev1.ProtocolTCP || service.Spec.Ports[0].TargetPort.StrVal != "http" ||
		service.Spec.Ports[1].Name != "agent-mtls" || service.Spec.Ports[1].Port != agentIngressPort ||
		service.Spec.Ports[1].Protocol != corev1.ProtocolTCP || service.Spec.Ports[1].TargetPort.StrVal != "agent-mtls" {
		t.Fatalf("service ports = %#v", service.Spec.Ports)
	}
}

func assertHTTPProbe(t *testing.T, probe *corev1.Probe, path, port string, failureThreshold int32) {
	t.Helper()
	if probe == nil || probe.HTTPGet == nil {
		t.Fatalf("probe = %#v, want HTTP GET %s on %s", probe, path, port)
	}
	if probe.HTTPGet.Path != path || probe.HTTPGet.Port.StrVal != port ||
		probe.HTTPGet.Port.Type != intstr.String || probe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Fatalf("probe HTTP GET = %#v, want HTTP %s on named port %s", probe.HTTPGet, path, port)
	}
	if probe.InitialDelaySeconds != 0 || probe.TimeoutSeconds != 1 || probe.PeriodSeconds != 10 ||
		probe.SuccessThreshold != 1 || probe.FailureThreshold != failureThreshold || probe.TerminationGracePeriodSeconds != nil {
		t.Fatalf("probe API defaults = %#v", probe)
	}
}

func TestRuntimeReadyTreatsMissingWorkloadsAsNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KubeNeuronReconciler{Client: client, Scheme: scheme}

	ready, message, err := reconciler.runtimeReady(context.Background(), testKubeNeuron())
	if err != nil {
		t.Fatalf("runtimeReady() error = %v", err)
	}
	if ready || !strings.Contains(message, "not found") {
		t.Fatalf("runtimeReady() = (%v, %q), want not ready/not found", ready, message)
	}
}

func TestRuntimeConfigMapPublishesAllConsumedInputs(t *testing.T) {
	configMap := runtimeConfigMap(testKubeNeuron(), &Snapshot{
		PoliciesYAML:    []byte("policies: []\n"),
		WindowsYAML:     []byte("windows: []\n"),
		MappingsYAML:    []byte("overrides: []\n"),
		NodeConfigsYAML: []byte("nodes: []\n"),
		Digest:          "abc123",
	})
	want := []string{"policies.yaml", "windows.yaml", "signal-mappings.yaml", "node-configs.yaml", "config-digest"}
	if len(configMap.Data) != len(want) {
		t.Fatalf("runtime ConfigMap keys = %#v, want %v", configMap.Data, want)
	}
	for _, key := range want {
		if _, ok := configMap.Data[key]; !ok {
			t.Fatalf("runtime ConfigMap is missing %q: %#v", key, configMap.Data)
		}
	}
}

func TestControllerPDBKeepsSingleReplicaAvailable(t *testing.T) {
	pdb := controllerPDB(testKubeNeuron())
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Fatalf("MinAvailable = %v, want 1", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.MaxUnavailable != nil {
		t.Fatalf("MaxUnavailable = %v, want nil", pdb.Spec.MaxUnavailable)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsAll(values, wanted []string) bool {
	for _, item := range wanted {
		if !containsString(values, item) && !containsString(values, "*") {
			return false
		}
	}
	return true
}

func TestControllerDeploymentWiresNotificationsAndPause(t *testing.T) {
	// Slack is useful for dry-run notifications as well; it must not unlock
	// unsupported Enabled execution.
	installation := testKubeNeuron()
	installation.Spec.Notifications = &kubeneuronv1alpha1.NotificationsSpec{
		Slack:            &kubeneuronv1alpha1.SecretReference{Name: "fleet-slack"},
		OperatorAPIToken: &kubeneuronv1alpha1.SecretReference{Name: "fleet-api"},
		WebhookToken:     &kubeneuronv1alpha1.SecretReference{Name: "fleet-webhook"},
	}
	deployment, err := controllerDeployment(installation, &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatalf("controllerDeployment() error = %v", err)
	}
	args := strings.Join(deployment.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--slack-webhook-file=/var/run/secrets/kube-neuron/notifications-slack/webhook-url") {
		t.Fatalf("args missing slack webhook flag: %s", args)
	}
	if strings.Contains(args, "--start-paused") {
		t.Fatalf("dry-run notification deployment must not start paused: %s", args)
	}
	foundVolume := false
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		if v.Name == "notifications-slack" && v.Secret != nil && v.Secret.SecretName == "fleet-slack" {
			foundVolume = true
			if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "webhook-url" {
				t.Fatalf("slack secret items = %#v", v.Secret.Items)
			}
		}
	}
	if !foundVolume {
		t.Fatal("notifications-slack volume missing")
	}
	for name, secret := range map[string]string{"operator-api-token": "fleet-api", "webhook-token": "fleet-webhook"} {
		volume := findVolume(deployment.Spec.Template.Spec.Volumes, name)
		if volume == nil || volume.Secret == nil || volume.Secret.SecretName != secret || len(volume.Secret.Items) != 1 || volume.Secret.Items[0].Path != "token" {
			t.Fatalf("%s volume = %#v", name, volume)
		}
		if mount := findVolumeMount(deployment.Spec.Template.Spec.Containers[0].VolumeMounts, name); mount == nil || !mount.ReadOnly {
			t.Fatalf("%s mount = %#v, want read-only", name, mount)
		}
	}
	if !strings.Contains(args, "--api-token-file=/var/run/secrets/kube-neuron/operator-api-token/token") || !strings.Contains(args, "--webhook-token-file=/var/run/secrets/kube-neuron/webhook-token/token") {
		t.Fatalf("args missing API/webhook token flags: %s", args)
	}

	// Paused: the controller starts with the gate closed.
	paused := testKubeNeuron()
	paused.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModePaused
	deployment, err = controllerDeployment(paused, &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatalf("paused controllerDeployment() error = %v", err)
	}
	args = strings.Join(deployment.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--start-paused") {
		t.Fatalf("paused mode must pass --start-paused: %s", args)
	}

	// Custom Secret key maps onto the fixed mounted filename.
	custom := testKubeNeuron()
	custom.Spec.Notifications = &kubeneuronv1alpha1.NotificationsSpec{
		Slack:        &kubeneuronv1alpha1.SecretReference{Name: "fleet-slack", Key: "url"},
		WebhookToken: &kubeneuronv1alpha1.SecretReference{Name: "fleet-webhook"},
	}
	deployment, err = controllerDeployment(custom, &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatalf("custom-key controllerDeployment() error = %v", err)
	}
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		if v.Name == "notifications-slack" {
			if v.Secret.Items[0].Key != "url" || v.Secret.Items[0].Path != "webhook-url" {
				t.Fatalf("custom key mapping = %#v", v.Secret.Items)
			}
		}
	}
}

// Optional public TLS: the reference must wire the cert/key flags, mount the
// Secret, and switch the probes to HTTPS; omitting it keeps plain HTTP.
func TestControllerDeploymentPublicTLS(t *testing.T) {
	plain := testKubeNeuron()
	deployment, err := controllerDeployment(plain, &Snapshot{Digest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	for _, arg := range container.Args {
		if strings.Contains(arg, "public-tls") {
			t.Fatalf("plain-HTTP install must not pass public TLS args: %v", container.Args)
		}
	}
	if container.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Fatalf("plain install probe scheme = %v, want HTTP", container.ReadinessProbe.HTTPGet.Scheme)
	}

	withTLS := testKubeNeuron()
	withTLS.Spec.TLS.PublicServerSecretRef = &kubeneuronv1alpha1.SecretReference{Name: "fleet-public-tls"}
	deployment, err = controllerDeployment(withTLS, &Snapshot{Digest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	container = deployment.Spec.Template.Spec.Containers[0]
	for _, arg := range []string{
		"--public-tls-cert=/var/run/secrets/kube-neuron/public-tls/tls.crt",
		"--public-tls-key=/var/run/secrets/kube-neuron/public-tls/tls.key",
	} {
		if !containsString(container.Args, arg) {
			t.Errorf("controller args missing %q: %v", arg, container.Args)
		}
	}
	volume := findVolume(deployment.Spec.Template.Spec.Volumes, "public-tls")
	if volume == nil || volume.Secret == nil || volume.Secret.SecretName != "fleet-public-tls" {
		t.Fatalf("public TLS volume = %#v", volume)
	}
	mount := findVolumeMount(container.VolumeMounts, "public-tls")
	if mount == nil || !mount.ReadOnly {
		t.Fatalf("public TLS mount = %#v, want read-only", mount)
	}
	if container.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS ||
		container.LivenessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Fatal("probes must use HTTPS when the public listener serves TLS")
	}
}

// The controller runs as the distroless nonroot user; without fsGroup the
// SQLite store cannot write CSI-provisioned volumes (EBS mounts root-owned).
// Found on the first real EKS deployment — kind's permissive local-path
// storage masked it.
func TestControllerDeploymentNonRootWithFSGroup(t *testing.T) {
	deployment, err := controllerDeployment(testKubeNeuron(), &Snapshot{Digest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	sc := deployment.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.FSGroup == nil || *sc.FSGroup != 65532 ||
		sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
		t.Fatalf("controller pod SecurityContext = %+v, want nonroot 65532 with fsGroup", sc)
	}
}

// A Postgres installation is stateless: DSN comes from a mounted Secret,
// there is no state PVC, and the DSN never appears in argv.
func TestControllerDeploymentPostgresStore(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.WorkflowStore = kubeneuronv1alpha1.WorkflowStoreSpec{
		Type:      "Postgres",
		SecretRef: &kubeneuronv1alpha1.SecretReference{Name: "fleet-postgres-dsn"},
	}
	deployment, err := controllerDeployment(installation, &Snapshot{Digest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	for _, arg := range []string{
		"--store=postgres",
		"--postgres-dsn-file=/var/run/secrets/kube-neuron/postgres-dsn/dsn",
	} {
		if !containsString(container.Args, arg) {
			t.Errorf("controller args missing %q: %v", arg, container.Args)
		}
	}
	for _, arg := range container.Args {
		if strings.Contains(arg, "postgres://") {
			t.Fatalf("DSN leaked into argv: %v", container.Args)
		}
	}
	if findVolume(deployment.Spec.Template.Spec.Volumes, "state") != nil {
		t.Fatal("Postgres deployment must not mount a state PVC")
	}
	dsn := findVolume(deployment.Spec.Template.Spec.Volumes, "postgres-dsn")
	if dsn == nil || dsn.Secret == nil || dsn.Secret.SecretName != "fleet-postgres-dsn" {
		t.Fatalf("postgres DSN volume = %#v", dsn)
	}
	if _, ok := deployment.Spec.Template.Annotations[storageRequestAnnotation]; ok {
		t.Fatal("Postgres deployment must not carry the SQLite storage annotation")
	}
}

func TestAgentDaemonSetHostToolingMounts(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Agent.HostTooling = &kubeneuronv1alpha1.HostToolingSpec{
		ScriptsDir: "/etc/kube-neuron-host/scripts",
	}
	agents := agentDaemonSet(installation, &Snapshot{Digest: "abc123"})
	container := agents.Spec.Template.Spec.Containers[0]

	joined := strings.Join(container.Args, "\n")
	for _, want := range []string{"--require-real-driver", "--scripts-dir=/etc/kube-neuron/scripts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("agent args missing %q: %q", want, container.Args)
		}
	}

	envByName := map[string]string{}
	for _, env := range container.Env {
		envByName[env.Name] = env.Value
	}
	if !strings.HasPrefix(envByName["PATH"], "/host/nvidia/bin:") {
		t.Errorf("PATH = %q, want /host/nvidia/bin first", envByName["PATH"])
	}
	if envByName["LD_LIBRARY_PATH"] != "/host/nvidia/lib/0" {
		t.Errorf("LD_LIBRARY_PATH = %q, want default single lib mount", envByName["LD_LIBRARY_PATH"])
	}

	for name, hostPath := range map[string]string{
		"nvidia-bin":   "/usr/bin",
		"nvidia-lib-0": "/usr/lib64",
		"scripts":      "/etc/kube-neuron-host/scripts",
	} {
		mount := findVolumeMount(container.VolumeMounts, name)
		if mount == nil || !mount.ReadOnly {
			t.Fatalf("host-tooling mount %s = %#v, want read-only", name, mount)
		}
		volume := findVolume(agents.Spec.Template.Spec.Volumes, name)
		if volume == nil || volume.HostPath == nil || volume.HostPath.Path != hostPath {
			t.Fatalf("host-tooling volume %s = %#v, want hostPath %s", name, volume, hostPath)
		}
		if volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathDirectory {
			t.Fatalf("host-tooling volume %s type = %v, want Directory (fail loud on a wrong AMI)", name, volume.HostPath.Type)
		}
	}

	loaderMounts := 0
	for _, mount := range container.VolumeMounts {
		if mount.Name == "nvidia-lib-0" && mount.MountPath == "/lib64" && mount.ReadOnly {
			loaderMounts++
		}
	}
	if loaderMounts != 1 {
		t.Fatalf("ELF-loader mount of nvidia-lib-0 at /lib64 = %d, want 1 (exec of a dynamic nvidia-smi needs PT_INTERP)", loaderMounts)
	}

	// Without hostTooling the DaemonSet is byte-identical to before: no
	// extra args, env, mounts, or volumes on CPU-only installs.
	plain := agentDaemonSet(testKubeNeuron(), &Snapshot{Digest: "abc123"})
	plainContainer := plain.Spec.Template.Spec.Containers[0]
	if strings.Contains(strings.Join(plainContainer.Args, "\n"), "require-real-driver") {
		t.Error("plain install unexpectedly requires the real driver")
	}
	for _, env := range plainContainer.Env {
		if env.Name == "PATH" || env.Name == "LD_LIBRARY_PATH" {
			t.Errorf("plain install sets %s", env.Name)
		}
	}
	for _, name := range []string{"nvidia-bin", "nvidia-lib-0", "scripts"} {
		if findVolume(plain.Spec.Template.Spec.Volumes, name) != nil {
			t.Errorf("plain install carries host-tooling volume %s", name)
		}
	}
}

func TestControllerDeploymentNotificationChannelWiring(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Notifications.Webhook = &kubeneuronv1alpha1.SecretReference{Name: "notify-hook"}
	installation.Spec.Notifications.PagerDuty = &kubeneuronv1alpha1.SecretReference{Name: "pd-key"}
	deployment, err := controllerDeployment(installation, &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	joined := strings.Join(container.Args, "\n")
	for _, want := range []string{
		"--notify-webhook-url-file=/var/run/secrets/kube-neuron/notifications-webhook/url",
		"--pagerduty-routing-key-file=/var/run/secrets/kube-neuron/notifications-pagerduty/routing-key",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("controller args missing %q", want)
		}
	}
	for volume, secret := range map[string]string{
		"notifications-webhook":   "notify-hook",
		"notifications-pagerduty": "pd-key",
	} {
		mount := findVolumeMount(container.VolumeMounts, volume)
		if mount == nil || !mount.ReadOnly {
			t.Fatalf("mount %s = %#v, want read-only", volume, mount)
		}
		vol := findVolume(deployment.Spec.Template.Spec.Volumes, volume)
		if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secret {
			t.Fatalf("volume %s = %#v, want Secret %s", volume, vol, secret)
		}
	}

	// Without the references there are no mounts and no flags.
	plain, err := controllerDeployment(testKubeNeuron(), &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	plainJoined := strings.Join(plain.Spec.Template.Spec.Containers[0].Args, "\n")
	if strings.Contains(plainJoined, "notify-webhook") || strings.Contains(plainJoined, "pagerduty") {
		t.Fatalf("plain install carries notification-channel flags: %s", plainJoined)
	}
}

func TestControllerDeploymentAuthWiring(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Auth = &kubeneuronv1alpha1.AuthSpec{
		Users: &kubeneuronv1alpha1.SecretReference{Name: "panel-users"},
		OIDC: &kubeneuronv1alpha1.OIDCSpec{
			IssuerURL: "https://sso.example.com/realms/prod",
			ClientID:  "kubeneuron-panel",
			ClientSecretRef: kubeneuronv1alpha1.SecretReference{
				Name: "oidc-client",
			},
			RedirectURL:         "https://panel.example.com/api/v1/auth/oidc/callback",
			AllowedEmailDomains: []string{"example.com"},
		},
	}
	deployment, err := controllerDeployment(installation, &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	joined := strings.Join(container.Args, "\n")
	for _, want := range []string{
		"--auth-users-dir=/var/run/secrets/kube-neuron/auth-users",
		"--oidc-issuer=https://sso.example.com/realms/prod",
		"--oidc-client-id=kubeneuron-panel",
		"--oidc-redirect-url=https://panel.example.com/api/v1/auth/oidc/callback",
		"--oidc-client-secret-file=/var/run/secrets/kube-neuron/oidc/client-secret",
		"--oidc-allowed-email-domains=example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("controller args missing %q", want)
		}
	}
	for volume, secret := range map[string]string{
		"auth-users":  "panel-users",
		"oidc-client": "oidc-client",
	} {
		mount := findVolumeMount(container.VolumeMounts, volume)
		if mount == nil || !mount.ReadOnly {
			t.Fatalf("mount %s = %#v, want read-only", volume, mount)
		}
		vol := findVolume(deployment.Spec.Template.Spec.Volumes, volume)
		if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secret {
			t.Fatalf("volume %s = %#v, want Secret %s", volume, vol, secret)
		}
	}

	plain, err := controllerDeployment(testKubeNeuron(), &Snapshot{Digest: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	plainJoined := strings.Join(plain.Spec.Template.Spec.Containers[0].Args, "\n")
	if strings.Contains(plainJoined, "auth-users") || strings.Contains(plainJoined, "oidc") {
		t.Fatalf("plain install carries auth flags: %s", plainJoined)
	}
}

func TestDestructiveExecutionArmsOnlyDeclaredNodes(t *testing.T) {
	// Without the block, nothing is armed and the agent lands wherever the
	// agent selector says.
	plain := agentDaemonSet(testKubeNeuron(), &Snapshot{Digest: "abc123"})
	if strings.Contains(strings.Join(plain.Spec.Template.Spec.Containers[0].Args, "\n"), "enable-destructive-actions") {
		t.Fatal("dry-run install armed destructive actions")
	}

	installation := testKubeNeuron()
	installation.Spec.Agent.NodeSelector = map[string]string{"gpu": "true"}
	installation.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModeEnabled
	installation.Spec.Safety.DestructiveExecution = &kubeneuronv1alpha1.DestructiveExecutionSpec{
		NodeSelector:    map[string]string{"kubeneuron.io/destructive": "true"},
		Acknowledgement: "I understand these nodes may be reset, rebooted, or destroyed",
	}
	armed := agentDaemonSet(installation, &Snapshot{Digest: "abc123"})
	if !strings.Contains(strings.Join(armed.Spec.Template.Spec.Containers[0].Args, "\n"), "--enable-destructive-actions") {
		t.Fatal("Enabled install did not arm the agent")
	}
	// Arming narrows placement: both the agent selector and the declared
	// destructive selector must match, so an undeclared node never receives
	// an armed agent.
	want := map[string]string{"gpu": "true", "kubeneuron.io/destructive": "true"}
	if len(armed.Spec.Template.Spec.NodeSelector) != len(want) {
		t.Fatalf("node selector = %v, want %v", armed.Spec.Template.Spec.NodeSelector, want)
	}
	for key, value := range want {
		if armed.Spec.Template.Spec.NodeSelector[key] != value {
			t.Fatalf("node selector = %v, want %v", armed.Spec.Template.Spec.NodeSelector, want)
		}
	}
}

func TestEnabledRequiresDeclaredDestructiveNodes(t *testing.T) {
	base := func() *kubeneuronv1alpha1.KubeNeuron {
		k := testKubeNeuron()
		k.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModeEnabled
		return k
	}
	ack := "I understand these nodes may be reset, rebooted, or destroyed"

	for _, tt := range []struct {
		name    string
		mutate  func(*kubeneuronv1alpha1.KubeNeuron)
		wantErr string
	}{
		{"no block at all", func(k *kubeneuronv1alpha1.KubeNeuron) {}, "requires spec.safety.destructiveExecution"},
		{"empty selector", func(k *kubeneuronv1alpha1.KubeNeuron) {
			k.Spec.Safety.DestructiveExecution = &kubeneuronv1alpha1.DestructiveExecutionSpec{Acknowledgement: ack}
		}, "must name the permitted nodes"},
		{"wrong acknowledgement", func(k *kubeneuronv1alpha1.KubeNeuron) {
			k.Spec.Safety.DestructiveExecution = &kubeneuronv1alpha1.DestructiveExecutionSpec{
				NodeSelector: map[string]string{"lab": "true"}, Acknowledgement: "yes",
			}
		}, "must read exactly"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			k := base()
			tt.mutate(k)
			err := validateRuntimeSupport(k)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}

	// Fully declared: accepted, and the compiled snapshot leaves dry-run.
	k := base()
	k.Spec.Safety.DestructiveExecution = &kubeneuronv1alpha1.DestructiveExecutionSpec{
		NodeSelector: map[string]string{"kubeneuron.io/destructive": "true"}, Acknowledgement: ack,
	}
	if err := validateRuntimeSupport(k); err != nil {
		t.Fatalf("fully declared install rejected: %v", err)
	}
	policies, playbooks := testPolicyFixture()
	snapshot, err := CompileSnapshot(k, policies, playbooks, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshot.PoliciesYAML), "dry_run: false") {
		t.Fatalf("compiled safety did not leave dry-run:\n%s", snapshot.PoliciesYAML)
	}
}

func TestHostToolingPassesTheDCGMEndpoint(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Agent.HostTooling = &kubeneuronv1alpha1.HostToolingSpec{
		DCGMEndpoint: "nvidia-dcgm.gpu-operator.svc:5555",
	}
	armed := agentDaemonSet(installation, &Snapshot{Digest: "abc123"})
	if !strings.Contains(strings.Join(armed.Spec.Template.Spec.Containers[0].Args, "\n"),
		"--nvidia-dcgm-endpoint=nvidia-dcgm.gpu-operator.svc:5555") {
		t.Fatalf("agent args missing the DCGM endpoint: %v", armed.Spec.Template.Spec.Containers[0].Args)
	}

	// Host tooling without an endpoint leaves the probe local: the flag is
	// absent rather than empty, so dcgmi keeps its own default.
	plain := testKubeNeuron()
	plain.Spec.Agent.HostTooling = &kubeneuronv1alpha1.HostToolingSpec{}
	local := agentDaemonSet(plain, &Snapshot{Digest: "abc123"})
	if strings.Contains(strings.Join(local.Spec.Template.Spec.Containers[0].Args, "\n"), "nvidia-dcgm-endpoint") {
		t.Fatal("agent carries a DCGM endpoint flag without one configured")
	}
}
