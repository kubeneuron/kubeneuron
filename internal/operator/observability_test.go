// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/inventory"
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
	exporter := config["exporters"].(map[string]any)["otlp_http/store"].(map[string]any)
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
	exporter = config["exporters"].(map[string]any)["otlp_http/store"].(map[string]any)
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

	// A deprecated victoriaMetrics block is a query address: never written to.
	kn.Spec.Observability.Store = nil
	kn.Spec.Observability.VictoriaMetrics = &kubeneuronv1alpha1.DependencySpec{Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://vm.example:8428"}
	config = decodeCollectorConfig(t, kn)
	if exporters := config["exporters"].(map[string]any); exporters["otlp_http/store"] != nil || exporters["nop"] == nil {
		t.Errorf("victoriaMetrics alone: exporters = %v, want nop only", exporters)
	}

	kn.Spec.Observability.VictoriaMetrics = nil
	config = decodeCollectorConfig(t, kn)
	if _, ok := config["exporters"].(map[string]any)["nop"]; !ok {
		t.Errorf("no store: exporters = %v, want nop", config["exporters"])
	}
}

func TestCollectorNetworkPolicy(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	policy, err := collectorNetworkPolicy(kn, "kubeneuron-operator-ns", nil)
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
	policy, err = collectorNetworkPolicy(kn, "", nil)
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
		"dcgmExporter on an external collector": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Collector.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Collector.Endpoint = "http://otel.example:4318"
			kn.Spec.Observability.Store.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Store.Endpoint = "http://vm.example:8428/opentelemetry"
			kn.Spec.Observability.Collector.DCGMExporter = &kubeneuronv1alpha1.DCGMExporterScrapeSpec{}
		},
		"grafana datasource with a managed store": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged,
				Datasource: &kubeneuronv1alpha1.GrafanaDatasourceSpec{URL: "http://vm.example:8428"}}
		},
		"managed grafana with an external store and no datasource": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Store.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Store.Endpoint = "http://vm.example:8428/opentelemetry"
			kn.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}
		},
		"managed grafana without a store": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Store = nil
			kn.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}
		},
		"credentials in the grafana datasource": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Store.Mode = kubeneuronv1alpha1.IntegrationModeExternal
			kn.Spec.Observability.Store.Endpoint = "http://vm.example:8428/opentelemetry"
			kn.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged,
				Datasource: &kubeneuronv1alpha1.GrafanaDatasourceSpec{URL: "http://u:p@vm.example:8428"}}
		},
		"datasource on an external grafana": func(kn *kubeneuronv1alpha1.KubeNeuron) {
			kn.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://g.example",
				Datasource: &kubeneuronv1alpha1.GrafanaDatasourceSpec{URL: "http://vm.example:8428"}}
		},
	}
	withGrafana := managedObservability(testKubeNeuron())
	withGrafana.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}
	if err := validateObservability(withGrafana); err != nil {
		t.Fatalf("managed grafana on the managed store: %v", err)
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
	for key, value := range map[string]string{"otelCollectorK8s": DefaultCollectorImage, "victoriaMetricsSingle": DefaultMetricsStoreImage, "victoriaMetricsAlert": DefaultMetricsRulesImage, "grafana": DefaultGrafanaImage} {
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

func reconcileObservabilityFixture(t *testing.T, kn *kubeneuronv1alpha1.KubeNeuron, extra ...client.Object) (client.Client, *KubeNeuronReconciler) {
	t.Helper()
	scheme := newOperatorScheme(t)
	kn.UID = types.UID("fleet-uid")
	kn.Generation = 1
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kn.Spec.Namespace}}
	playbook, policy := observeConfiguration(kn.Name)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kubeneuronv1alpha1.KubeNeuron{}, &kubeneuronv1alpha1.GPUPlaybook{}, &kubeneuronv1alpha1.GPURemediationPolicy{}, &appsv1.Deployment{}, &appsv1.DaemonSet{}).
		WithObjects(append([]client.Object{kn, namespace, playbook, policy}, extra...)...).
		// The API server answers a metadata.name field selector itself; the
		// fake client needs it as an index.
		WithIndex(&corev1.Service{}, "metadata.name", func(o client.Object) []string { return []string{o.GetName()} }).
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
		{"fleet-metrics-rules", &appsv1.Deployment{}},
		{"fleet-metrics-rules", &corev1.ConfigMap{}},
		{"fleet-metrics-rules", &corev1.ServiceAccount{}},
		{"fleet-metrics-rules", &networkingv1.NetworkPolicy{}},
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
	if c := meta.FindStatusCondition(kn.Status.Conditions, rulesReadyCondition); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("rules condition before it is available = %#v", c)
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
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.ConfigMap{}, &corev1.ServiceAccount{}, &networkingv1.NetworkPolicy{}} {
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-metrics-rules", Namespace: ns}, object); !apierrors.IsNotFound(err) {
			t.Errorf("%T fleet-metrics-rules after switch: err = %v, want NotFound", object, err)
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
	if c := meta.FindStatusCondition(kn.Status.Conditions, rulesReadyCondition); c != nil {
		t.Errorf("an external store still has a rules condition: %#v", c)
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

func storeClaim(t *testing.T, c client.Client) *corev1.PersistentVolumeClaim {
	t.Helper()
	var claim corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Name: "fleet-metrics-store", Namespace: "kube-neuron-system"}, &claim); err != nil {
		t.Fatalf("store claim: %v", err)
	}
	return &claim
}

func TestStoreClaimHasNoOwnerAndOutlivesTheStore(t *testing.T) {
	ctx := context.Background()
	fakeClient, reconciler := reconcileObservabilityFixture(t, managedObservability(testKubeNeuron()))
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	claim := storeClaim(t, fakeClient)
	if len(claim.OwnerReferences) != 0 {
		t.Fatalf("store claim owners = %v, want none: garbage collection would delete the metrics with the installation", claim.OwnerReferences)
	}

	// A claim from an earlier build carries our owner reference: dropped.
	var kn kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, request.NamespacedName, &kn); err != nil {
		t.Fatal(err)
	}
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "kubeneuron.io/v1alpha1", Kind: "KubeNeuron", Name: kn.Name, UID: kn.UID, Controller: ptr.To(true)}}
	if err := fakeClient.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if owners := storeClaim(t, fakeClient).OwnerReferences; len(owners) != 0 {
		t.Fatalf("owner reference not removed: %v", owners)
	}

	// Disabled: the store goes, the claim stays and still has no owner.
	kn.Spec.Observability.Store = nil
	kn.Spec.Observability.Collector = nil
	if err := fakeClient.Update(ctx, &kn); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if owners := storeClaim(t, fakeClient).OwnerReferences; len(owners) != 0 {
		t.Fatalf("claim after disable has owners %v", owners)
	}
}

func TestStoreRefusesAForeignClaim(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn)
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-metrics-store", Namespace: kn.Spec.Namespace, Labels: map[string]string{"app": "other"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if err := fakeClient.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}); err == nil {
		t.Fatal("Reconcile() = nil, want a refusal to adopt a claim that is not this installation's")
	}
	if got := storeClaim(t, fakeClient); got.Labels["app"] != "other" || len(got.OwnerReferences) != 0 {
		t.Errorf("the foreign claim was changed: %#v", got.ObjectMeta)
	}
}

func TestDeprecatedVictoriaMetricsIsNotADestination(t *testing.T) {
	ctx := context.Background()
	kn := testKubeNeuron() // victoriaMetrics set, no store
	kn.Spec.Observability.Collector = &kubeneuronv1alpha1.CollectorSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}}
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var config corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector", Namespace: kn.Spec.Namespace}, &config); err != nil {
		t.Fatal(err)
	}
	if data := config.Data["config.yaml"]; strings.Contains(data, "vm.example") || !strings.Contains(data, "nop") {
		t.Errorf("collector config sends to the deprecated query address:\n%s", data)
	}
	var got kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, storeReadyCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "DeprecatedVictoriaMetrics" ||
		c.Message != "victoriaMetrics is deprecated and is not used as the metrics destination; set spec.observability.store" {
		t.Errorf("store condition = %#v", c)
	}
}

func dcgmFixtureObjects(namespace string, nodePool string) []client.Object {
	selector := map[string]string{"app": "nvidia-dcgm-exporter"}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm-exporter", Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports:    []corev1.ServicePort{{Name: "gpu-metrics", Port: 9400, TargetPort: intstr.FromString("metrics")}},
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-a", Labels: map[string]string{
		"kubeneuron.io/node-pool": "fallback", "eks.amazonaws.com/nodegroup": nodePool,
	}}}
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "something-else", Namespace: namespace},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "x"}, Ports: []corev1.ServicePort{{Name: "gpu-metrics", Port: 1}}}}
	terminating := dcgmPod(namespace, "dcgm-4", "10.0.0.9", true)
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{"test/keep"}
	return []client.Object{service, node, other,
		dcgmPod(namespace, "dcgm-1", "10.0.0.7", true),
		dcgmPod(namespace, "dcgm-2", "", false),
		dcgmPod(namespace, "dcgm-3", "10.0.0.8", false),
		terminating,
	}
}

// dcgmPod is an exporter pod on node gpu-a, Running, Ready or not.
func dcgmPod(namespace, name, ip string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	phase := corev1.PodRunning
	if ip == "" {
		phase = corev1.PodPending
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": "nvidia-dcgm-exporter"}},
		Spec: corev1.PodSpec{NodeName: "gpu-a", Containers: []corev1.Container{{
			Name: "exporter", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9400}},
		}}},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func TestDCGMExporterDiscoveryAndScrape(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn, dcgmFixtureObjects("gpu-operator", "gpu-ng")...)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 || result.RequeueAfter > dcgmRediscoveryPeriod {
		t.Errorf("requeue = %v, want at most %v while scraping", result.RequeueAfter, dcgmRediscoveryPeriod)
	}
	var targets corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector-targets", Namespace: kn.Spec.Namespace}, &targets); err != nil {
		t.Fatal(err)
	}
	if want := `[{"targets":["10.0.0.7:9400"],"labels":{"node":"gpu-a","node_pool":"gpu-ng"}}]`; targets.Data[dcgmTargetsKey] != want {
		t.Errorf("targets = %s, want %s (the Ready pod only, EKS node group winning)", targets.Data[dcgmTargetsKey], want)
	}

	config := decodeCollectorConfig(t, kn)
	receiver := config["receivers"].(map[string]any)["prometheus/dcgm-exporter"].(map[string]any)
	scrape := receiver["config"].(map[string]any)["scrape_configs"].([]any)[0].(map[string]any)
	relabel := scrape["metric_relabel_configs"].([]any)
	last := relabel[len(relabel)-1].(map[string]any)
	if last["action"] != "labeldrop" || last["regex"] != dcgmWorkloadLabelRegex || !strings.Contains(dcgmWorkloadLabelRegex, "|pod_uid|") {
		t.Errorf("workload labels are not dropped by default: %v", relabel)
	}
	if scrape["job_name"] != "dcgm-exporter" || scrape["scrape_interval"] != "30s" || scrape["scrape_timeout"] != "10s" || scrape["sample_limit"] != float64(dcgmSampleLimit) {
		t.Errorf("scrape config = %v", scrape)
	}
	pipelines := config["service"].(map[string]any)["pipelines"].(map[string]any)
	if receivers := pipelines["metrics"].(map[string]any)["receivers"].([]any); len(receivers) != 1 || receivers[0] != "otlp" {
		t.Errorf("components pipeline receivers = %v", receivers)
	}
	scraped := pipelines["metrics/dcgm-exporter"].(map[string]any)
	if receivers := scraped["receivers"].([]any); len(receivers) != 1 || receivers[0] != "prometheus/dcgm-exporter" {
		t.Errorf("dcgm pipeline receivers = %v", receivers)
	}
	if processors := fmt.Sprint(scraped["processors"]); processors != "[memory_limiter resource/dcgm-exporter batch]" {
		t.Errorf("dcgm pipeline processors = %s", processors)
	}
	stamp := config["processors"].(map[string]any)["resource/dcgm-exporter"].(map[string]any)["attributes"].([]any)[0].(map[string]any)
	if stamp["key"] != "kubeneuron.installation" || stamp["value"] != "fleet" || stamp["action"] != "upsert" {
		t.Errorf("installation attribute = %v", stamp)
	}

	var collector appsv1.Deployment
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector", Namespace: kn.Spec.Namespace}, &collector); err != nil {
		t.Fatal(err)
	}
	if v := findVolume(collector.Spec.Template.Spec.Volumes, "targets"); v == nil || v.ConfigMap.Name != "fleet-otel-collector-targets" {
		t.Errorf("targets volume = %#v", v)
	}
	if mounts := collector.Spec.Template.Spec.Containers[0].VolumeMounts; mounts[len(mounts)-1].SubPath != "" {
		t.Error("targets mounted by subPath would never refresh")
	}

	var policy networkingv1.NetworkPolicy
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector", Namespace: kn.Spec.Namespace}, &policy); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "gpu-operator" &&
				peer.PodSelector.MatchLabels["app"] == "nvidia-dcgm-exporter" && len(rule.Ports) == 1 && rule.Ports[0].Port.Type == intstr.Int && rule.Ports[0].Port.IntValue() == 9400 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no egress to the exporter pods on their metrics port by number: %#v", policy.Spec.Egress)
	}

	var got kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, gpuMetricsCondition); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Scraping" ||
		c.Message != "scraping 1 Ready dcgm-exporter pods behind gpu-operator/nvidia-dcgm-exporter" {
		t.Errorf("GPU metrics condition = %#v", c)
	}

	// Kept on request.
	got.Spec.Observability.Collector.DCGMExporter = &kubeneuronv1alpha1.DCGMExporterScrapeSpec{KeepWorkloadLabels: true}
	config = decodeCollectorConfig(t, &got)
	scrape = config["receivers"].(map[string]any)["prometheus/dcgm-exporter"].(map[string]any)["config"].(map[string]any)["scrape_configs"].([]any)[0].(map[string]any)
	for _, r := range scrape["metric_relabel_configs"].([]any) {
		if r.(map[string]any)["action"] == "labeldrop" {
			t.Error("keepWorkloadLabels still drops the workload labels")
		}
	}

	// Scraping off: no receiver, no targets, no condition.
	got.Spec.Observability.Collector.DCGMExporter = &kubeneuronv1alpha1.DCGMExporterScrapeSpec{Scrape: ptr.To(false)}
	if err := fakeClient.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector-targets", Namespace: kn.Spec.Namespace}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("targets ConfigMap with scraping off: %v, want NotFound", err)
	}
	if _, ok := decodeCollectorConfig(t, &got)["receivers"].(map[string]any)["prometheus/dcgm-exporter"]; ok {
		t.Error("scraping off still has the receiver")
	}
	if err := fakeClient.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, gpuMetricsCondition); c != nil {
		t.Errorf("scraping off still has a condition: %#v", c)
	}
}

func TestNoDCGMExporterIsAConditionNotAnError(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v, want none without dcgm-exporter", err)
	}
	var got kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, gpuMetricsCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NotFound" || !strings.HasPrefix(c.Message, "no dcgm-exporter found") {
		t.Errorf("condition = %#v", c)
	}
	var targets corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector-targets", Namespace: kn.Spec.Namespace}, &targets); err != nil || targets.Data[dcgmTargetsKey] != "[]" {
		t.Errorf("targets = %v (%v), want an empty list for the collector to read", targets.Data, err)
	}
}

func TestDCGMExporterNamespaceIsHonoured(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	kn.Spec.Observability.Collector.DCGMExporter = &kubeneuronv1alpha1.DCGMExporterScrapeSpec{Namespace: "elsewhere"}
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn, dcgmFixtureObjects("gpu-operator", "ng")...)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}); err != nil {
		t.Fatal(err)
	}
	var got kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet"}, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, gpuMetricsCondition); c == nil || c.Reason != "NotFound" || !strings.Contains(c.Message, "in namespace elsewhere") {
		t.Errorf("condition = %#v, want NotFound in the configured namespace", c)
	}
}

func TestDCGMExporterWithoutReadyPods(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	objects := dcgmFixtureObjects("gpu-operator", "ng")
	kept := objects[:0]
	for _, o := range objects {
		if o.GetName() != "dcgm-1" {
			kept = append(kept, o)
		}
	}
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn, kept...)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}); err != nil {
		t.Fatal(err)
	}
	var got kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet"}, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, gpuMetricsCondition); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NoReadyPods" {
		t.Errorf("condition = %#v, want NoReadyPods while the Service exists", c)
	}
}

func TestDCGMExporterOnTheHostNetwork(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	objects := dcgmFixtureObjects("gpu-operator", "ng")
	for _, o := range objects {
		if pod, ok := o.(*corev1.Pod); ok && pod.Name == "dcgm-1" {
			pod.Spec.HostNetwork = true
			pod.Status.PodIP = "192.168.1.20"
		}
	}
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn, objects...)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}); err != nil {
		t.Fatal(err)
	}
	var targets corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector-targets", Namespace: kn.Spec.Namespace}, &targets); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(targets.Data[dcgmTargetsKey], `"192.168.1.20:9400"`) {
		t.Errorf("targets = %s, want the node address", targets.Data[dcgmTargetsKey])
	}
	var policy networkingv1.NetworkPolicy
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector", Namespace: kn.Spec.Namespace}, &policy); err != nil {
		t.Fatal(err)
	}
	var byIP, bySelector bool
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "192.168.1.20/32" && rule.Ports[0].Port.IntValue() == 9400 {
				byIP = true
			}
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "nvidia-dcgm-exporter" {
				bySelector = true
			}
		}
	}
	if !byIP || bySelector {
		t.Errorf("egress = %#v, want the node address on 9400 and no pod selector, which matches no host-network pod", policy.Spec.Egress)
	}
}

func TestDCGMExporterInSeveralNamespaces(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	objects := dcgmFixtureObjects("z-gpu", "ng")
	for _, o := range dcgmFixtureObjects("a-gpu", "ng") {
		if _, ok := o.(*corev1.Node); ok {
			continue
		}
		if pod, ok := o.(*corev1.Pod); ok && pod.Name == "dcgm-3" {
			pod.Status.Conditions[0].Status = corev1.ConditionTrue
		}
		objects = append(objects, o)
	}
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn, objects...)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}); err != nil {
		t.Fatal(err)
	}
	var got kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet"}, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, gpuMetricsCondition); c == nil || c.Status != metav1.ConditionTrue ||
		c.Message != "scraping 3 Ready dcgm-exporter pods behind a-gpu/nvidia-dcgm-exporter, z-gpu/nvidia-dcgm-exporter" {
		t.Errorf("condition = %#v", c)
	}
	var targets corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector-targets", Namespace: kn.Spec.Namespace}, &targets); err != nil {
		t.Fatal(err)
	}
	first := targets.Data[dcgmTargetsKey]
	if strings.Index(first, "10.0.0.7") > strings.Index(first, "10.0.0.8") {
		t.Errorf("targets out of order: %s", first)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}); err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector-targets", Namespace: kn.Spec.Namespace}, &targets); err != nil {
		t.Fatal(err)
	}
	if targets.Data[dcgmTargetsKey] != first {
		t.Errorf("targets changed with nothing changed: %s, then %s", first, targets.Data[dcgmTargetsKey])
	}
}

func TestDCGMTargetChangesDoNotRestartTheCollector(t *testing.T) {
	ctx := context.Background()
	kn := managedObservability(testKubeNeuron())
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn, dcgmFixtureObjects("gpu-operator", "ng")...)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}
	digest := func() string {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		var collector appsv1.Deployment
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector", Namespace: kn.Spec.Namespace}, &collector); err != nil {
			t.Fatal(err)
		}
		return collector.Spec.Template.Annotations["kubeneuron.io/config-digest"]
	}
	before := digest()
	if err := fakeClient.Create(ctx, dcgmPod("gpu-operator", "dcgm-5", "10.0.0.10", true)); err != nil {
		t.Fatal(err)
	}
	after := digest()
	var targets corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-otel-collector-targets", Namespace: kn.Spec.Namespace}, &targets); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(targets.Data[dcgmTargetsKey], "10.0.0.10") {
		t.Fatalf("new pod not a target: %s", targets.Data[dcgmTargetsKey])
	}
	if before == "" || before != after {
		t.Errorf("config digest %q -> %q: a new target must not restart the collector", before, after)
	}
}

func TestSettledRequeue(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	if got := settledRequeue(kn); got != min(dcgmRediscoveryPeriod, pkiCheckInterval) {
		t.Errorf("scraping requeue = %v", got)
	}
	kn.Spec.Observability.Collector.DCGMExporter = &kubeneuronv1alpha1.DCGMExporterScrapeSpec{Scrape: ptr.To(false)}
	if got := settledRequeue(kn); got != 3*inventory.Interval {
		t.Errorf("requeue without scraping = %v, want %v", got, 3*inventory.Interval)
	}
}

type ruleFile struct {
	Groups []struct {
		Name  string           `json:"name"`
		Rules []map[string]any `json:"rules"`
	} `json:"groups"`
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func parseRuleFile(t *testing.T, data string) ruleFile {
	t.Helper()
	var file ruleFile
	if err := sigsyaml.Unmarshal([]byte(data), &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestManagedRulesAreRecordingOnly(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	records := 0
	for _, group := range parseRuleFile(t, rulesConfigMap(kn).Data[rulesKey]).Groups {
		for _, rule := range group.Rules {
			if _, ok := rule["alert"]; ok {
				t.Errorf("group %s of %s has an alerting rule %v", group.Name, rulesKey, rule["alert"])
			}
			if _, ok := rule["record"]; ok {
				records++
			}
		}
	}
	if records == 0 {
		t.Fatal("no recording rules")
	}

	deployment := rulesDeployment(kn, DefaultMetricsRulesImage, "d", false)
	args := strings.Join(deployment.Spec.Template.Spec.Containers[0].Args, " ")
	store := "http://fleet-metrics-store.kube-neuron-system.svc:8428"
	if !strings.Contains(args, "-datasource.url="+store) || !strings.Contains(args, "-remoteWrite.url="+store) {
		t.Errorf("vmalert args = %s, want our store", args)
	}
	if spec := deployment.Spec.Template.Spec; spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("vmalert mounts a service account token")
	}
	admitted := false
	for _, peer := range storeNetworkPolicy(kn).Spec.Ingress[0].From {
		if peer.PodSelector.MatchLabels["app.kubernetes.io/component"] == rulesComponent {
			admitted = true
		}
	}
	if !admitted {
		t.Error("the store does not admit the rules evaluator")
	}
}

// The managed alerting rules are exactly the faults that reach KubeNeuron on
// NVIDIA only as metric alerts, with the dependency profile's names, `for`
// and severity, in plain PromQL, aggregated so no workload label survives.
func TestManagedAlertingRules(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	want := map[string]bool{
		"GpuRemappedRowsHigh": true, "GpuPcieReplayHigh": true, "GpuThermalThrottle": true,
		"GpuTempCritical": true, "GpuPowerBrake": true, "GpuExporterDown": true, "KubeNeuronAgentDown": true,
	}
	profile := map[string]map[string]any{}
	for _, group := range parseRuleFile(t, readRepoFile(t, "configs/vmalert/gpu-rules.yaml")).Groups {
		for _, rule := range group.Rules {
			if name, ok := rule["alert"].(string); ok {
				profile[name] = rule
			}
		}
	}
	known := map[string]bool{}
	for _, name := range detect.KnownAlertNames() {
		known[name] = true
	}
	got := map[string]bool{}
	for _, group := range parseRuleFile(t, rulesConfigMap(kn).Data[alertingKey]).Groups {
		for _, rule := range group.Rules {
			if _, ok := rule["record"]; ok {
				t.Errorf("group %s of %s has a recording rule", group.Name, alertingKey)
			}
			name, _ := rule["alert"].(string)
			got[name] = true
			if !want[name] {
				t.Errorf("unexpected alert %q", name)
			}
			if !known[name] {
				t.Errorf("alert %q is not in the controller's alertClassMap", name)
			}
			expr, _ := rule["expr"].(string)
			if strings.Contains(expr, "bitmap_") {
				t.Errorf("alert %q uses a MetricsQL-only function: %s", name, expr)
			}
			if !strings.Contains(expr, "max by (") {
				t.Errorf("alert %q does not aggregate, so workload labels would reach the alert: %s", name, expr)
			}
			for _, label := range []string{"pod", "namespace", "container", "pod_uid"} {
				if regexp.MustCompile(`by \([^)]*\b` + label + `\b`).MatchString(expr) {
					t.Errorf("alert %q keeps the workload label %s", name, label)
				}
			}
			source := profile[name]
			if source == nil {
				t.Errorf("alert %q is not in gpu-rules.yaml", name)
				continue
			}
			if fmt.Sprint(rule["for"]) != fmt.Sprint(source["for"]) ||
				fmt.Sprint(rule["labels"]) != fmt.Sprint(source["labels"]) {
				t.Errorf("alert %q: for %v labels %v, the dependency profile has for %v labels %v",
					name, rule["for"], rule["labels"], source["for"], source["labels"])
			}
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("alert %q is missing", name)
		}
	}

	deployment := rulesDeployment(kn, DefaultMetricsRulesImage, "d", false)
	args := strings.Join(deployment.Spec.Template.Spec.Containers[0].Args, " ")
	for _, arg := range []string{
		"-rule=/etc/vmalert/alerting-rules.yaml",
		"-notifier.url=http://fleet-controller.kube-neuron-system.svc:8080/api/v1/webhooks/vmalert",
		"-notifier.bearerTokenFile=/etc/vmalert-notifier/token/token",
	} {
		if !strings.Contains(args, arg) {
			t.Errorf("vmalert args = %s, want %s", args, arg)
		}
	}
	if strings.Contains(args, "tlsCAFile") || strings.Contains(args, "bearerToken=") {
		t.Errorf("vmalert args = %s", args)
	}
	tokenMounted := false
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Secret != nil && volume.Secret.SecretName == webhookTokenRef(kn).Name {
			tokenMounted = true
		}
	}
	if !tokenMounted {
		t.Error("the webhook token Secret is not mounted")
	}

	policy := rulesNetworkPolicy(kn)
	if len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 3 ||
		policy.Spec.Egress[1].To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != storeComponent ||
		policy.Spec.Egress[2].To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "controller" ||
		policy.Spec.Egress[2].Ports[0].Port.IntValue() != controllerPort {
		t.Errorf("rules NetworkPolicy = %#v, want no ingress and egress to DNS, the store and the controller", policy.Spec)
	}

	// Behind the controller's own TLS the notifier speaks https and, when
	// the Secret has one, verifies with its ca.crt.
	kn.Spec.TLS.PublicServerSecretRef = &kubeneuronv1alpha1.SecretReference{Name: "public-tls"}
	args = strings.Join(rulesDeployment(kn, DefaultMetricsRulesImage, "d", true).Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "-notifier.url=https://fleet-controller.") || !strings.Contains(args, "-notifier.tlsCAFile=/etc/vmalert-notifier/ca/ca.crt") {
		t.Errorf("vmalert args behind TLS = %s", args)
	}
}

func TestAlertDetectionCondition(t *testing.T) {
	kn := managedObservability(testKubeNeuron())
	if c := alertDetection(kn, &metav1.Condition{Status: metav1.ConditionTrue}); c.Status != metav1.ConditionTrue {
		t.Errorf("managed store, rules ready: %+v", c)
	}
	if c := alertDetection(kn, notReady("NotReady", "x")); c.Status != metav1.ConditionFalse {
		t.Errorf("managed store, rules not ready: %+v", c)
	}
	kn.Spec.Observability.Store = &kubeneuronv1alpha1.MetricsStoreSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://vm:8428/opentelemetry"}}
	if c := alertDetection(kn, nil); c.Status != metav1.ConditionFalse || c.Reason != "ExternalStore" ||
		!strings.Contains(c.Message, "point them at the controller webhook") {
		t.Errorf("external store: %+v", c)
	}
	kn.Spec.Observability.Store = nil
	if c := alertDetection(kn, nil); c != nil {
		t.Errorf("disabled store: %+v", c)
	}
}

func managedGrafana(kn *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.KubeNeuron {
	kn = managedObservability(kn)
	kn.Spec.Observability.Grafana = &kubeneuronv1alpha1.GrafanaSpec{Mode: kubeneuronv1alpha1.IntegrationModeManaged}
	return kn
}

func TestManagedGrafana(t *testing.T) {
	ctx := context.Background()
	kn := managedGrafana(testKubeNeuron())
	fakeClient, reconciler := reconcileObservabilityFixture(t, kn)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	ns := kn.Spec.Namespace
	for _, check := range []struct {
		name   string
		object client.Object
	}{
		{"fleet-grafana", &appsv1.Deployment{}},
		{"fleet-grafana", &corev1.Service{}},
		{"fleet-grafana", &corev1.ConfigMap{}},
		{"fleet-grafana", &corev1.ServiceAccount{}},
		{"fleet-grafana", &networkingv1.NetworkPolicy{}},
		{"fleet-grafana-dashboards", &corev1.ConfigMap{}},
		{"fleet-grafana-admin", &corev1.Secret{}},
	} {
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: check.name, Namespace: ns}, check.object); err != nil {
			t.Errorf("%T %s: %v", check.object, check.name, err)
		}
	}

	// The admin password: random, owned by the installation, never rewritten.
	var admin corev1.Secret
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-grafana-admin", Namespace: ns}, &admin); err != nil {
		t.Fatal(err)
	}
	password := string(admin.Data[grafanaAdminPasswordKey])
	if len(password) < 32 || string(admin.Data[grafanaAdminUserKey]) != "admin" {
		t.Errorf("admin Secret = user %q, password of %d characters", admin.Data[grafanaAdminUserKey], len(password))
	}
	if !metav1.IsControlledBy(&admin, kn) {
		t.Error("the admin Secret does not belong to the installation, so it would outlive it")
	}
	admin.Data[grafanaAdminPasswordKey] = []byte("chosen-by-the-user")
	if err := fakeClient.Update(ctx, &admin); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-grafana-admin", Namespace: ns}, &admin); err != nil {
		t.Fatal(err)
	}
	if string(admin.Data[grafanaAdminPasswordKey]) != "chosen-by-the-user" {
		t.Error("reconcile overwrote the admin password the user set")
	}

	var deployment appsv1.Deployment
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-grafana", Namespace: ns}, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	container := pod.Containers[0]
	if container.Image != DefaultGrafanaImage || !strings.Contains(container.Image, "@sha256:") {
		t.Errorf("image = %s", container.Image)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("grafana mounts a service account token")
	}
	want := map[string]string{
		"GF_AUTH_ANONYMOUS_ENABLED": "false", "GF_USERS_ALLOW_SIGN_UP": "false",
		"GF_ANALYTICS_REPORTING_ENABLED": "false", "GF_ANALYTICS_CHECK_FOR_UPDATES": "false",
		"GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES": "false", "GF_ANALYTICS_FEEDBACK_LINKS_ENABLED": "false",
		"GF_PLUGINS_PREINSTALL_DISABLED": "true", "GF_PLUGINS_PLUGIN_ADMIN_ENABLED": "false",
		"GF_PLUGINS_PUBLIC_KEY_RETRIEVAL_DISABLED": "true", "GF_NEWS_NEWS_FEED_ENABLED": "false",
		"GF_SECURITY_DISABLE_GRAVATAR": "true", "GF_SNAPSHOTS_EXTERNAL_ENABLED": "false",
		"GF_PUBLIC_DASHBOARDS_ENABLED": "false",
	}
	for name, value := range want {
		if e, i := envValue(container.Env, name); i < 0 || e.Value != value {
			t.Errorf("%s = %q, want %q", name, e.Value, value)
		}
	}
	if e, _ := envValue(container.Env, "GF_SECURITY_ADMIN_PASSWORD"); e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "fleet-grafana-admin" {
		t.Errorf("admin password env = %#v, want it from the Secret", e)
	}
	for _, v := range pod.Volumes {
		if v.Name == "data" && v.EmptyDir == nil {
			t.Error("grafana data is not an emptyDir")
		}
	}

	// Provisioning: the managed store as datasource, files win over the UI.
	var provisioning corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-grafana", Namespace: ns}, &provisioning); err != nil {
		t.Fatal(err)
	}
	if ds := provisioning.Data["datasources.yaml"]; !strings.Contains(ds, "url: http://fleet-metrics-store.kube-neuron-system.svc:8428") ||
		!strings.Contains(ds, "uid: "+grafanaDatasourceUID) || strings.Contains(ds, "secureJsonData") {
		t.Errorf("datasources.yaml = %s", ds)
	}
	if !strings.Contains(provisioning.Data["dashboards.yaml"], "allowUiUpdates: false") {
		t.Errorf("dashboards.yaml = %s", provisioning.Data["dashboards.yaml"])
	}
	var boards corev1.ConfigMap
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-grafana-dashboards", Namespace: ns}, &boards); err != nil {
		t.Fatal(err)
	}
	if len(boards.Data) != 6 {
		t.Errorf("%d dashboards, want 6", len(boards.Data))
	}
	for name, board := range boards.Data {
		if strings.Contains(board, "${DS_PROMETHEUS}") || !strings.Contains(board, grafanaDatasourceUID) {
			t.Errorf("%s is not bound to the datasource", name)
		}
	}

	var policy networkingv1.NetworkPolicy
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "fleet-grafana", Namespace: ns}, &policy); err != nil {
		t.Fatal(err)
	}
	ingress := policy.Spec.Ingress
	if len(ingress) != 1 || len(ingress[0].From) != 1 || ingress[0].From[0].NamespaceSelector != nil || ingress[0].From[0].PodSelector == nil ||
		len(policy.Spec.Egress) != 2 || policy.Spec.Egress[1].To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != storeComponent {
		t.Errorf("grafana NetworkPolicy = %#v, want ingress from this namespace only and egress to DNS and the store", policy.Spec)
	}
	admitted := false
	for _, peer := range storeNetworkPolicy(kn).Spec.Ingress[0].From {
		if peer.PodSelector.MatchLabels["app.kubernetes.io/component"] == grafanaComponent {
			admitted = true
		}
	}
	if !admitted {
		t.Error("the store does not admit grafana")
	}

	var got kubeneuronv1alpha1.KubeNeuron
	if err := fakeClient.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, grafanaReadyCondition); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("grafana condition before it is available = %#v", c)
	}

	// Disabled: the workload goes; the admin Secret stays for garbage
	// collection with the installation.
	got.Spec.Observability.Grafana = nil
	if err := fakeClient.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name   string
		object client.Object
	}{
		{"fleet-grafana", &appsv1.Deployment{}},
		{"fleet-grafana-dashboards", &corev1.ConfigMap{}},
	} {
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: check.name, Namespace: ns}, check.object); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s after disable: %v, want NotFound", check.object, check.name, err)
		}
	}
	if err := fakeClient.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, grafanaReadyCondition); c != nil {
		t.Errorf("disabled grafana still has a condition: %#v", c)
	}
}

func TestManagedGrafanaOnAnExternalStore(t *testing.T) {
	kn := managedGrafana(testKubeNeuron())
	kn.Spec.Observability.Store = &kubeneuronv1alpha1.MetricsStoreSpec{OTLPEndpointSpec: kubeneuronv1alpha1.OTLPEndpointSpec{
		Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://vm.example:8428/opentelemetry",
	}}
	kn.Spec.Observability.Grafana.Datasource = &kubeneuronv1alpha1.GrafanaDatasourceSpec{
		URL:                    "https://query.example:9443",
		AuthorizationSecretRef: &kubeneuronv1alpha1.SecretReference{Name: "query-auth", Key: "header"},
	}
	if err := validateObservability(kn); err != nil {
		t.Fatal(err)
	}
	provisioning, err := grafanaProvisioning(kn)
	if err != nil {
		t.Fatal(err)
	}
	ds := provisioning.Data["datasources.yaml"]
	if !strings.Contains(ds, "url: https://query.example:9443") || !strings.Contains(ds, "httpHeaderName1: Authorization") ||
		!strings.Contains(ds, "httpHeaderValue1: ${"+grafanaAuthorizationEnv+"}") {
		t.Errorf("datasources.yaml = %s", ds)
	}
	container := grafanaDeployment(kn, DefaultGrafanaImage, "d").Spec.Template.Spec.Containers[0]
	if e, _ := envValue(container.Env, grafanaAuthorizationEnv); e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "query-auth" || e.ValueFrom.SecretKeyRef.Key != "header" {
		t.Errorf("authorization env = %#v", e)
	}
	policy, err := grafanaNetworkPolicy(kn)
	if err != nil {
		t.Fatal(err)
	}
	if last := policy.Spec.Egress[len(policy.Spec.Egress)-1]; len(last.To) != 0 || last.Ports[0].Port.IntValue() != 9443 {
		t.Errorf("egress = %#v, want the datasource port", policy.Spec.Egress)
	}
}
