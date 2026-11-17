// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

func managedObservability(kn *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.KubeNeuron {
	kn.Spec.Observability.VictoriaMetrics = nil
	kn.Spec.Observability.Collector = &kubeneuronv1alpha1.CollectorSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}}
	kn.Spec.Observability.Store = &kubeneuronv1alpha1.MetricsStoreSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}}
	return kn
}

func envValue(env []corev1.EnvVar, name string) (corev1.EnvVar, int) {
	for i, e := range env {
		if e.Name == name {
			return e, i
		}
	}
	return corev1.EnvVar{}, -1
}

func TestTelemetryEnvFollowsCollectorMode(t *testing.T) {
	kn := testKubeNeuron()
	if env := telemetryEnv(kn); env != nil {
		t.Fatalf("no collector: env = %v, want none", env)
	}

	kn = managedObservability(testKubeNeuron())
	env := telemetryEnv(kn)
	if e, _ := envValue(env, "OTEL_EXPORTER_OTLP_ENDPOINT"); e.Value != "http://fleet-otel-collector.kube-neuron-system.svc:4318" {
		t.Errorf("endpoint = %q", e.Value)
	}
	if e, _ := envValue(env, "OTEL_EXPORTER_OTLP_PROTOCOL"); e.Value != "http/protobuf" {
		t.Errorf("protocol = %q", e.Value)
	}
	node, nodeAt := envValue(env, telemetryNodeEnv)
	attrs, attrsAt := envValue(env, "OTEL_RESOURCE_ATTRIBUTES")
	if node.ValueFrom == nil || node.ValueFrom.FieldRef == nil || node.ValueFrom.FieldRef.FieldPath != "spec.nodeName" || nodeAt > attrsAt {
		t.Errorf("node name must come from the downward API before it is used: %#v at %d, attributes at %d", node, nodeAt, attrsAt)
	}
	if want := "k8s.namespace.name=kube-neuron-system,kubeneuron.installation=fleet,k8s.node.name=$(" + telemetryNodeEnv + ")"; attrs.Value != want {
		t.Errorf("attributes = %q, want %q", attrs.Value, want)
	}
	if _, at := envValue(env, "OTEL_EXPORTER_OTLP_HEADERS"); at >= 0 {
		t.Error("a managed collector needs no headers")
	}

	kn.Spec.Observability.Collector = &kubeneuronv1alpha1.CollectorSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{
		Mode:                   kubeneuronv1alpha1.IntegrationModeExternal,
		Endpoint:               "https://otel.example:4318",
		AuthorizationSecretRef: &kubeneuronv1alpha1.SecretReference{Name: "otlp", Key: "authorization"},
	}}
	kn.Spec.Observability.Store = nil
	env = telemetryEnv(kn)
	if e, _ := envValue(env, "OTEL_EXPORTER_OTLP_ENDPOINT"); e.Value != "https://otel.example:4318" {
		t.Errorf("external endpoint = %q", e.Value)
	}
	auth, authAt := envValue(env, otlpAuthorizationEnv)
	headers, headersAt := envValue(env, "OTEL_EXPORTER_OTLP_HEADERS")
	if auth.ValueFrom == nil || auth.ValueFrom.SecretKeyRef == nil || auth.ValueFrom.SecretKeyRef.Name != "otlp" || auth.ValueFrom.SecretKeyRef.Key != "authorization" {
		t.Errorf("authorization must come from the Secret: %#v", auth)
	}
	if authAt < 0 || headersAt < authAt || headers.Value != "Authorization=$("+otlpAuthorizationEnv+")" {
		t.Errorf("headers %q at %d must follow the authorization variable at %d", headers.Value, headersAt, authAt)
	}
	for _, e := range env {
		if strings.Contains(e.Value, "Bearer") {
			t.Errorf("a credential leaked into a literal env value: %#v", e)
		}
	}
}

func TestControllerAndAgentCarryTelemetryEnv(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	playbook, policy := observeConfiguration(kn.Name)
	snapshot, err := CompileSnapshot(kn, []kubeneuronv1alpha1.GPURemediationPolicy{*policy}, []kubeneuronv1alpha1.GPUPlaybook{*playbook}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := controllerDeployment(kn, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, at := envValue(deployment.Spec.Template.Spec.Containers[0].Env, "OTEL_RESOURCE_ATTRIBUTES"); at < 0 {
		t.Error("controller has no OTEL_RESOURCE_ATTRIBUTES")
	}
	agents := agentDaemonSet(kn, snapshot)
	if _, at := envValue(agents.Spec.Template.Spec.Containers[0].Env, "OTEL_RESOURCE_ATTRIBUTES"); at < 0 {
		t.Error("agent has no OTEL_RESOURCE_ATTRIBUTES")
	}
}

func decodeCollectorConfig(t *testing.T, kn *kubeneuronv1alpha1.KubeNeuron) map[string]any {
	t.Helper()
	data, err := collectorConfig(kn)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := sigsyaml.Unmarshal([]byte(data), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestCollectorConfigExportsToTheStore(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	config := decodeCollectorConfig(t, kn)
	exporter := config["exporters"].(map[string]any)["otlphttp/store"].(map[string]any)
	if exporter["endpoint"] != "http://fleet-metrics-store.kube-neuron-system.svc:8428/opentelemetry" {
		t.Errorf("managed store endpoint = %v", exporter["endpoint"])
	}
	if _, ok := exporter["headers"]; ok {
		t.Error("a managed store has no authentication")
	}
	processors := config["service"].(map[string]any)["pipelines"].(map[string]any)["metrics"].(map[string]any)["processors"].([]any)
	if len(processors) == 0 || processors[0] != "memory_limiter" {
		t.Errorf("memory_limiter must run first: %v", processors)
	}

	kn.Spec.Observability.Store = &kubeneuronv1alpha1.MetricsStoreSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{
		Mode:                   kubeneuronv1alpha1.IntegrationModeExternal,
		Endpoint:               "https://prometheus.example/api/v1/otlp",
		AuthorizationSecretRef: &kubeneuronv1alpha1.SecretReference{Name: "store", Key: "authorization"},
	}}
	config = decodeCollectorConfig(t, kn)
	exporter = config["exporters"].(map[string]any)["otlphttp/store"].(map[string]any)
	if exporter["endpoint"] != "https://prometheus.example/api/v1/otlp" {
		t.Errorf("external store endpoint = %v", exporter["endpoint"])
	}
	if h := exporter["headers"].(map[string]any)["Authorization"]; h != "${env:"+storeAuthorizationEnv+"}" {
		t.Errorf("authorization header = %v, want an env reference", h)
	}
	deployment := collectorDeployment(kn, DefaultCollectorImage, "digest")
	if e, _ := envValue(deployment.Spec.Template.Spec.Containers[0].Env, storeAuthorizationEnv); e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "store" {
		t.Errorf("collector reads the store authorization from the Secret: %#v", e)
	}

	// A deprecated victoriaMetrics block alone is an external store.
	kn.Spec.Observability.Store = nil
	kn.Spec.Observability.VictoriaMetrics = &kubeneuronv1alpha1.DependencySpec{Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://vm.example:8428/opentelemetry"}
	config = decodeCollectorConfig(t, kn)
	if e := config["exporters"].(map[string]any)["otlphttp/store"].(map[string]any)["endpoint"]; e != "http://vm.example:8428/opentelemetry" {
		t.Errorf("victoriaMetrics as store endpoint = %v", e)
	}

	kn.Spec.Observability.VictoriaMetrics = nil
	config = decodeCollectorConfig(t, kn)
	if _, ok := config["exporters"].(map[string]any)["nop"]; !ok {
		t.Errorf("no store: exporters = %v, want nop", config["exporters"])
	}
}

func TestCollectorNetworkPolicy(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	policy, err := collectorNetworkPolicy(kn, "kubeneuron-operator-ns")
	if err != nil {
		t.Fatal(err)
	}
	from := policy.Spec.Ingress[0].From
	var components []string
	operator := false
	for _, peer := range from {
		if c := peer.PodSelector.MatchLabels["app.kubernetes.io/component"]; c == "operator" {
			operator = peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "kubeneuron-operator-ns"
		} else {
			if peer.PodSelector.MatchLabels["app.kubernetes.io/instance"] != "fleet" {
				t.Errorf("peer %#v is not scoped to the installation", peer)
			}
			components = append(components, c)
		}
	}
	if !operator || strings.Join(components, ",") != "controller,agent" {
		t.Errorf("ingress peers = %#v", from)
	}
	if p := policy.Spec.Ingress[0].Ports; len(p) != 1 || p[0].Port.IntValue() != collectorOTLPPort {
		t.Errorf("ingress ports = %#v", p)
	}
	if got := policy.Spec.Egress[1].Ports[0].Port.IntValue(); got != storePort {
		t.Errorf("egress to the managed store on %d, want %d", got, storePort)
	}

	kn.Spec.Observability.Store = &kubeneuronv1alpha1.MetricsStoreSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{
		Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "https://store.example/opentelemetry",
	}}
	policy, err = collectorNetworkPolicy(kn, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.Spec.Egress[1].Ports[0].Port.IntValue(); got != 443 || policy.Spec.Egress[1].To != nil {
		t.Errorf("external store egress = %#v", policy.Spec.Egress[1])
	}
}

func TestStoreDeploymentStorage(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	deployment := storeDeployment(kn, DefaultMetricsStoreImage)
	if v := deployment.Spec.Template.Spec.Volumes[0]; v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "fleet-metrics-store" {
		t.Errorf("store volume = %#v, want the claim", v)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("strategy = %s, want Recreate", deployment.Spec.Strategy.Type)
	}
	if args := strings.Join(deployment.Spec.Template.Spec.Containers[0].Args, " "); !strings.Contains(args, "-retentionPeriod=30d") {
		t.Errorf("args = %s", args)
	}
	kn.Spec.Observability.Store.Managed = &kubeneuronv1alpha1.ManagedStoreSpec{Storage: kubeneuronv1alpha1.StoreStorageSpec{Ephemeral: true}}
	deployment = storeDeployment(kn, DefaultMetricsStoreImage)
	if v := deployment.Spec.Template.Spec.Volumes[0]; v.EmptyDir == nil {
		t.Errorf("ephemeral store volume = %#v, want emptyDir", v)
	}
}

func TestValidateObservability(t *testing.T) {
	cases := map[string]func(*kubeneuronv1alpha1.KubeNeuron){
		"victoriaMetrics with store": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.VictoriaMetrics = &kubeneuronv1alpha1.DependencySpec{Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://vm.example"}
		},
		"external collector with managed store": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Collector.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Collector.Endpoint = "http://otel.example:4318"
		},
		"credentials in the endpoint": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Store.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Store.Endpoint = "https://user:pass@store.example"
		},
		"secret in another namespace": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Store.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Store.Endpoint = "https://store.example"
			kn.Spec.Observability.Store.AuthorizationSecretRef = &kubeneuronv1alpha1.SecretReference{Name: "s", Namespace: "other", Key: "k"}
		},
		"managed settings on an external store": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Store.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Store.Endpoint = "https://store.example"
			kn.Spec.Observability.Store.Managed = &kubeneuronv1alpha1.ManagedStoreSpec{}
		},
		"managed grafana": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}
		},
	}
	if err := validateObservability(managedObservability(testKubeNeuron())); err != nil {
		t.Fatalf("managed collector and store: %v", err)
	}
	external := managedObservability(testKubeNeuron())
	external.Spec.Observability.Collector.Mode = kubeneuronv1alpha1.IntegrationModeExternal
	external.Spec.Observability.Collector.Endpoint = "http://otel.example:4318"
	external.Spec.Observability.Store.Mode = kubeneuronv1alpha1.IntegrationModeExternal
	external.Spec.Observability.Store.Endpoint = "http://vm.example:8428/opentelemetry"
	if err := validateObservability(external); err != nil {
		t.Fatalf("external collector and store: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			kn := managedObservability(testKubeNeuron())
			mutate(kn)
			if err := validateObservability(kn); err == nil {
				t.Error("validateObservability() = nil, want an error")
			}
		})
	}
}

func TestDefaultObservabilityImagesMatchTheLock(t *testing.T) {
	data, err := os.ReadFile("../../deploy/kubernetes/dependencies/versions.lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type image struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
	}
	var lock struct {
		Images map[string]image `json:"images"`
	}
	if err := sigsyaml.Unmarshal(data, &lock); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"otelCollectorK8s": DefaultCollectorImage, "victoriaMetricsSingle": DefaultMetricsStoreImage} {
		pin := lock.Images[key]
		if pin.Digest == "" {
			t.Errorf("lock %s has no digest", key)
		}
		if want := pin.Repository + ":" + pin.Tag + "@" + pin.Digest; value != want {
			t.Errorf("default image %s, lock %s says %s", value, key, want)
		}
	}
}

// observeConfiguration is the smallest configuration that compiles: one
// observe-only policy for the installation.
func observeConfiguration(installation string) (*kubeneuronv1alpha1.GPUPlaybook, *kubeneuronv1alpha1.GPURemediationPolicy) {
	playbook := &kubeneuronv1alpha1.GPUPlaybook{
		ObjectMeta: metav1.ObjectMeta{Name: "observe"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: installation,
			Target:        kubeneuronv1alpha1.PlaybookTargetGPU,
			Steps:         []kubeneuronv1alpha1.PlaybookStep{{Name: "observe", Action: kubeneuronv1alpha1.ActionObserve}},
		},
	}
	policy := &kubeneuronv1alpha1.GPURemediationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "observe"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: installation,
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "test"},
			PlaybookRef:   playbook.Name,
		},
	}
	return playbook, policy
}

func reconcileObservabilityFixture(t *testing.T, kn *kubeneuronv1alpha1.KubeNeuron) (client.Client, *KubeNeuronReconciler) {
	t.Helper()
	scheme := newOperatorScheme(t)
	kn.UID = types.UID("fleet-uid")
	kn.Generation = 1
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kn.Spec.Namespace}}
	playbook, policy := observeConfiguration(kn.Name)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kubeneuronv1alpha1.KubeNeuron{}, &kubeneuronv1alpha1.GPUPlaybook{}, &kubeneuronv1alpha1.GPURemediationPolicy{}, &appsv1.Deployment{}, &appsv1.DaemonSet{}).
		WithObjects(kn, namespace, playbook, policy).
		Build()
	return fakeClient, &KubeNeuronReconciler{Client: fakeClient, Scheme: scheme, OperatorNamespace: "kubeneuron-operator"}
}

func TestReconcileManagedObservabilityAndSwitchAway(t *testing.T) {
	ctx := context.Background()
	fakeClient, reconciler := reconcileObservabilityFixture(t, managedObservability(testKubeNeuron()))
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ns := "kube-neuron-system"
	for _, check := range []struct {
		name   string
		object client.Object
	}{
		{"fleet-otel-collector", &appsv1.Deployment{}},
		{"fleet-otel-collector", &corev1.Service{}},
		{"fleet-otel-collector", &corev1.ConfigMap{}},
		{"fleet-otel-collector", &corev1.ServiceAccount{}},
		{"fleet-otel-collector", &networkingv1.NetworkPolicy{}},
		{"fleet-metrics-store", &appsv1.Deployment{}},
		{"fleet-metrics-store", &corev1.Service{}},
		{"fleet-metrics-store", &corev1.ServiceAccount{}},
		{"fleet-metrics-store", &corev1.PersistentVolumeClaim{}},
		{"fleet-metrics-store", &networkingv1.NetworkPolicy{}},
	} {
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: check.name, Namespace: ns}, check.object); err != nil {
			t.Errorf("%T %s: %v", check.object, check.name, err)
		}
	}
	var collector appsv1.Deployment
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector", Namespace: ns}, &collector); err != nil {
		t.Fatal(err)
	}
	if image := collector.Spec.Template.Spec.Containers[0].Image; image != DefaultCollectorImage {
		t.Errorf("collector image = %s", image)
	}
	if limits := collector.Spec.Template.Spec.Containers[0].Resources.Limits; limits.Memory().IsZero() {
		t.Error("the collector has no memory limit for memory_limiter to work from")
	}

	var kn kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, request.NamespacedName, &kn); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(kn.Status.Conditions, storeReadyCondition); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "StoragePending" {
		t.Errorf("store condition with an unbound claim = %#v", c)
	}
	if c := meta.FindStatusCondition(kn.Status.Conditions, collectorReadyCondition); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("collector condition before it is available = %#v", c)
	}

	// Managed -> External store: the workload goes, the data stays.
	kn.Spec.Observability.Store = &kubeneuronv1alpha1.MetricsStoreSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{
		Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://vm.example:8428/opentelemetry",
	}}
	kn.Generation = 2
	if err := fakeClient.Update(ctx, &kn); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() after switch error = %v", err)
	}
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{}, &networkingv1.NetworkPolicy{}} {
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-metrics-store", Namespace: ns}, object); !apierrors.IsNotFound(err) {
			t.Errorf("%T fleet-metrics-store after switch: err = %v, want NotFound", object, err)
		}
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-metrics-store", Namespace: ns}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("the store's claim must survive a switch away from Managed: %v", err)
	}
	if err := fakeClient.Get(ctx, request.NamespacedName, &kn); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(kn.Status.Conditions, storeReadyCondition); c == nil || c.Reason != "External" {
		t.Errorf("external store condition = %#v", c)
	}

	// Disabled collector: its objects go and its condition is removed.
	kn.Spec.Observability.Collector = nil
	kn.Generation = 3
	if err := fakeClient.Update(ctx, &kn); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() after disable error = %v", err)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector", Namespace: ns}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("collector Deployment after disable: err = %v, want NotFound", err)
	}
	if err := fakeClient.Get(ctx, request.NamespacedName, &kn); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(kn.Status.Conditions, collectorReadyCondition); c != nil {
		t.Errorf("disabled collector still has a condition: %#v", c)
	}
}

func TestReconcileLeavesAnUnownedCollectorAlone(t *testing.T) {
	ctx := context.Background()
	kn := testKubeNeuron()
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn)
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "fleet-otel-collector", Namespace: kn.Spec.Namespace}}
	if err := fakeClient.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}); err == nil {
		t.Fatal("Reconcile() = nil, want a refusal to delete an object this installation does not own")
	}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(foreign), &appsv1.Deployment{}); err != nil {
		t.Errorf("the unowned Deployment was removed: %v", err)
	}
}
