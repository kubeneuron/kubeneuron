// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	sigsyaml "sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// The managed collector and store run third-party images, referenced by
// digest and pulled from their publishers: KubeNeuron does not redistribute
// them. Both are Apache-2.0. Move tag and digest together; the operator's
// --otel-collector-image and --metrics-store-image flags override them for
// a mirror.
const (
	DefaultCollectorImage    = "otel/opentelemetry-collector-k8s:0.165.0@sha256:ce55d0a38923c83120e4ba613fbdf446786652d2ce9f29d1cd7879e5d869829f"
	DefaultMetricsStoreImage = "victoriametrics/victoria-metrics:v1.147.0@sha256:40ea45a6d14b6ad9f2f1fff597309d456ff9885d77d8d1da5fd559b251db9987"
)

const (
	collectorComponent = "otel-collector"
	storeComponent     = "metrics-store"
	collectorOTLPPort  = 4318
	collectorHealth    = 13133
	storePort          = 8428
	// storeAuthorizationEnv carries the external store's Authorization
	// header into the collector; otlpAuthorizationEnv carries the external
	// collector's into the components.
	storeAuthorizationEnv = "KUBENEURON_STORE_AUTHORIZATION"
	otlpAuthorizationEnv  = "KUBENEURON_OTLP_AUTHORIZATION"
	telemetryNodeEnv      = "KUBENEURON_TELEMETRY_NODE"
)

func collectorName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	return installation.Name + "-" + collectorComponent
}

func storeName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	return installation.Name + "-" + storeComponent
}

func serviceURL(installation *kubeneuronv1alpha1.KubeNeuron, name string, port int) string {
	return "http://" + name + "." + installation.Spec.Namespace + ".svc:" + strconv.Itoa(port)
}

// telemetryEnv is the OTLP export configuration of a KubeNeuron component:
// where to send and which resource attributes identify the sender. The node
// name keeps the series of agents on different nodes apart. Nothing about
// the user's workloads (pods, jobs, namespaces) is an attribute.
func telemetryEnv(installation *kubeneuronv1alpha1.KubeNeuron) []corev1.EnvVar {
	var endpoint string
	var auth *kubeneuronv1alpha1.SecretReference
	switch collectorMode(installation) {
	case kubeneuronv1alpha1.IntegrationModeManaged:
		endpoint = serviceURL(installation, collectorName(installation), collectorOTLPPort)
	case kubeneuronv1alpha1.IntegrationModeExternal:
		endpoint = installation.Spec.Observability.Collector.Endpoint
		auth = installation.Spec.Observability.Collector.AuthorizationSecretRef
	default:
		return nil
	}
	env := []corev1.EnvVar{
		{Name: telemetryNodeEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			APIVersion: "v1", FieldPath: "spec.nodeName",
		}}},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: endpoint},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "http/protobuf"},
		{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: "k8s.namespace.name=" + installation.Spec.Namespace +
			",kubeneuron.installation=" + installation.Name + ",k8s.node.name=$(" + telemetryNodeEnv + ")"},
	}
	if auth != nil {
		// Declared before its use: Kubernetes expands $(VAR) only from
		// variables listed earlier.
		env = append(env,
			corev1.EnvVar{Name: otlpAuthorizationEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: auth.Name}, Key: auth.Key,
			}}},
			corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: "Authorization=$(" + otlpAuthorizationEnv + ")"},
		)
	}
	return env
}

// collectorConfig renders the managed collector's configuration: OTLP over
// HTTP in, memory_limiter and batch, OTLP over HTTP out to the store.
func collectorConfig(installation *kubeneuronv1alpha1.KubeNeuron) (string, error) {
	exporters := map[string]any{}
	pipelineExporters := []string{}
	switch storeMode(installation) {
	case kubeneuronv1alpha1.IntegrationModeManaged:
		exporters["otlp_http/store"] = map[string]any{
			"endpoint": serviceURL(installation, storeName(installation), storePort) + "/opentelemetry",
		}
		pipelineExporters = append(pipelineExporters, "otlp_http/store")
	case kubeneuronv1alpha1.IntegrationModeExternal:
		store := effectiveStore(installation)
		exporter := map[string]any{"endpoint": store.Endpoint}
		if store.AuthorizationSecretRef != nil {
			exporter["headers"] = map[string]any{"Authorization": "${env:" + storeAuthorizationEnv + "}"}
		}
		exporters["otlp_http/store"] = exporter
		pipelineExporters = append(pipelineExporters, "otlp_http/store")
	default:
		// No store: received metrics are dropped rather than refused, so
		// the components do not log export failures for a choice the user
		// made.
		exporters["nop"] = map[string]any{}
		pipelineExporters = append(pipelineExporters, "nop")
	}
	receivers := map[string]any{
		"otlp": map[string]any{"protocols": map[string]any{
			"http": map[string]any{"endpoint": "0.0.0.0:" + strconv.Itoa(collectorOTLPPort)},
		}},
	}
	processors := map[string]any{
		// Percentages of the container memory limit, which is always set.
		"memory_limiter": map[string]any{"check_interval": "1s", "limit_percentage": 80, "spike_limit_percentage": 25},
		"batch":          map[string]any{},
	}
	pipelines := map[string]any{
		"metrics": map[string]any{
			"receivers":  []string{"otlp"},
			"processors": []string{"memory_limiter", "batch"},
			"exporters":  pipelineExporters,
		},
	}
	if dcgmScrapeEnabled(installation) {
		// A pipeline of its own, so only the scraped series get the
		// installation attribute; the components set theirs themselves.
		receivers["prometheus/dcgm-exporter"] = dcgmScrapeReceiver(installation)
		processors["resource/dcgm-exporter"] = dcgmResourceProcessor(installation)
		pipelines["metrics/dcgm-exporter"] = map[string]any{
			"receivers":  []string{"prometheus/dcgm-exporter"},
			"processors": []string{"memory_limiter", "resource/dcgm-exporter", "batch"},
			"exporters":  pipelineExporters,
		}
	}
	config := map[string]any{
		"extensions": map[string]any{
			"health_check": map[string]any{"endpoint": "0.0.0.0:" + strconv.Itoa(collectorHealth)},
		},
		"receivers":  receivers,
		"processors": processors,
		"exporters":  exporters,
		"service": map[string]any{
			"extensions": []string{"health_check"},
			"pipelines":  pipelines,
			// The collector's own telemetry stays inside the pod.
			"telemetry": map[string]any{"metrics": map[string]any{"level": "none"}},
		},
	}
	data, err := sigsyaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("render collector configuration: %w", err)
	}
	return string(data), nil
}

func collectorConfigMap(installation *kubeneuronv1alpha1.KubeNeuron) (*corev1.ConfigMap, error) {
	config, err := collectorConfig(installation)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      collectorName(installation),
			Namespace: installation.Spec.Namespace,
			Labels:    resourceLabels(installation, collectorComponent),
		},
		Data: map[string]string{"config.yaml": config},
	}, nil
}

func observabilityServiceAccount(installation *kubeneuronv1alpha1.KubeNeuron, name, component string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: installation.Spec.Namespace,
			Labels:    resourceLabels(installation, component),
		},
		// Neither the collector nor the store talks to the Kubernetes API.
		AutomountServiceAccountToken: ptr.To(false),
	}
}

func defaultCollectorResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
}

func defaultStoreResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
}

func restrictedContainerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		RunAsNonRoot:             ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func restrictedPodSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To(int64(65532)),
		RunAsGroup:   ptr.To(int64(65532)),
		FSGroup:      ptr.To(int64(65532)),
		// Relabel a volume only when its root is not ours yet: the store's
		// claim holds 30 days of files, and a full chown on every start
		// would delay it.
		FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
		SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func collectorDeployment(installation *kubeneuronv1alpha1.KubeNeuron, image, configDigest string) *appsv1.Deployment {
	labels := resourceLabels(installation, collectorComponent)
	resources := defaultCollectorResources()
	if c := collectorSpec(installation); c != nil && c.Resources != nil {
		resources = *c.Resources.DeepCopy()
	}
	container := corev1.Container{
		Name:            "collector",
		Image:           image,
		ImagePullPolicy: defaultImagePullPolicy(image),
		Args:            []string{"--config=/etc/otelcol/config.yaml"},
		Resources:       resources,
		Ports: []corev1.ContainerPort{
			{Name: "otlp-http", ContainerPort: collectorOTLPPort, Protocol: corev1.ProtocolTCP},
			{Name: "health", ContainerPort: collectorHealth, Protocol: corev1.ProtocolTCP},
		},
		ReadinessProbe:           defaultReadinessHTTPProbe("/", "health"),
		LivenessProbe:            defaultHTTPProbe("/", "health"),
		SecurityContext:          restrictedContainerSecurity(),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts:             []corev1.VolumeMount{{Name: "config", MountPath: "/etc/otelcol", ReadOnly: true}},
	}
	volumes := []corev1.Volume{{
		Name: "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: collectorName(installation)},
			DefaultMode:          ptr.To(int32(defaultConfigMapMode)),
		}},
	}}
	if dcgmScrapeEnabled(installation) {
		// Mounted whole, not by subPath, so the kubelet refreshes the file
		// when the operator rewrites the targets.
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "targets", MountPath: dcgmTargetsMountPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "targets",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: dcgmTargetsName(installation)},
				DefaultMode:          ptr.To(int32(defaultConfigMapMode)),
			}},
		})
	}
	if store := effectiveStore(installation); storeMode(installation) == kubeneuronv1alpha1.IntegrationModeExternal && store.AuthorizationSecretRef != nil {
		ref := store.AuthorizationSecretRef
		container.Env = append(container.Env, corev1.EnvVar{Name: storeAuthorizationEnv, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name}, Key: ref.Key},
		}})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: collectorName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.To(int32(1)),
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			RevisionHistoryLimit:    ptr.To(int32(defaultRevisionHistoryLimit)),
			ProgressDeadlineSeconds: ptr.To(int32(defaultProgressDeadline)),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromString("25%")),
					MaxSurge:       ptr.To(intstr.FromString("25%")),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"kubeneuron.io/config-digest": configDigest}},
				Spec: corev1.PodSpec{
					ServiceAccountName:            collectorName(installation),
					DeprecatedServiceAccount:      collectorName(installation),
					AutomountServiceAccountToken:  ptr.To(false),
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: ptr.To(int64(defaultTerminationGrace)),
					DNSPolicy:                     corev1.DNSClusterFirst,
					SecurityContext:               restrictedPodSecurity(),
					SchedulerName:                 corev1.DefaultSchedulerName,
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
				},
			},
		},
	}
}

func clusterIPService(installation *kubeneuronv1alpha1.KubeNeuron, name, component, portName string, port int) *corev1.Service {
	labels := resourceLabels(installation, component)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name: portName, Protocol: corev1.ProtocolTCP, Port: int32(port), TargetPort: intstr.FromString(portName),
			}},
		},
	}
}

func collectorService(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.Service {
	return clusterIPService(installation, collectorName(installation), collectorComponent, "otlp-http", collectorOTLPPort)
}

func storeService(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.Service {
	return clusterIPService(installation, storeName(installation), storeComponent, "http", storePort)
}

func dnsEgress() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))},
			{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(53))},
		},
	}
}

func tcpPort(port int32) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(port))}
}

// operatorPeer matches the operator's pods, which export through the
// collector too. operatorNamespace is the namespace the operator runs in;
// empty matches it in any namespace.
func operatorPeer(operatorNamespace string) networkingv1.NetworkPolicyPeer {
	peer := networkingv1.NetworkPolicyPeer{
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "kube-neuron", "app.kubernetes.io/component": "operator"}},
		NamespaceSelector: &metav1.LabelSelector{},
	}
	if operatorNamespace != "" {
		peer.NamespaceSelector.MatchLabels = map[string]string{"kubernetes.io/metadata.name": operatorNamespace}
	}
	return peer
}

// agentPodLabels select the Pods of every agent DaemonSet of the
// installation: "-agent" and the journal variants differ in component.
func agentPodLabels(installation *kubeneuronv1alpha1.KubeNeuron) map[string]string {
	labels := resourceLabels(installation, "")
	delete(labels, "app.kubernetes.io/component")
	labels[AgentDaemonSetLabel] = "true"
	return labels
}

// collectorNetworkPolicy admits OTLP only from this installation's
// controller and agents and from the operator, and lets the collector reach
// DNS and the store. An external store's address cannot be named in a
// NetworkPolicy, so its port is open to any destination.
//
// The collector may also reach the dcgm-exporter pods it scrapes, on their
// metrics port only.
func collectorNetworkPolicy(installation *kubeneuronv1alpha1.KubeNeuron, operatorNamespace string, dcgm *dcgmDiscovery) (*networkingv1.NetworkPolicy, error) {
	labels := resourceLabels(installation, collectorComponent)
	egress := []networkingv1.NetworkPolicyEgressRule{dnsEgress()}
	egress = append(egress, dcgmEgress(dcgm)...)
	switch storeMode(installation) {
	case kubeneuronv1alpha1.IntegrationModeManaged:
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, storeComponent)}}},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(storePort)},
		})
	case kubeneuronv1alpha1.IntegrationModeExternal:
		port, err := endpointPort(effectiveStore(installation).Endpoint)
		if err != nil {
			return nil, fmt.Errorf("spec.observability.store.endpoint: %w", err)
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{tcpPort(port)}})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: collectorName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, "controller")}},
					{PodSelector: &metav1.LabelSelector{MatchLabels: agentPodLabels(installation)}},
					operatorPeer(operatorNamespace),
				},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(collectorOTLPPort)},
			}},
			Egress: egress,
		},
	}, nil
}

// storeNetworkPolicy admits the collector, which writes, the recording rules
// evaluator, which reads and writes, and Grafana, which reads; it allows no
// egress.
func storeNetworkPolicy(installation *kubeneuronv1alpha1.KubeNeuron) *networkingv1.NetworkPolicy {
	labels := resourceLabels(installation, storeComponent)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: storeName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, collectorComponent)}},
					{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, rulesComponent)}},
					{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, grafanaComponent)}},
				},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(storePort)},
			}},
		},
	}
}

func storePVC(installation *kubeneuronv1alpha1.KubeNeuron) (*corev1.PersistentVolumeClaim, error) {
	store := effectiveStore(installation)
	quantity, err := storeSize(store)
	if err != nil {
		return nil, err
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      storeName(installation),
			Namespace: installation.Spec.Namespace,
			Labels:    resourceLabels(installation, storeComponent),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  ptr.To(corev1.PersistentVolumeFilesystem),
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: quantity}},
		},
	}
	if store.Managed != nil {
		claim.Spec.StorageClassName = store.Managed.Storage.StorageClassName
	}
	return claim, nil
}

func storeDeployment(installation *kubeneuronv1alpha1.KubeNeuron, image string) *appsv1.Deployment {
	store := effectiveStore(installation)
	labels := resourceLabels(installation, storeComponent)
	resources := defaultStoreResources()
	if store.Managed != nil && store.Managed.Resources != nil {
		resources = *store.Managed.Resources.DeepCopy()
	}
	data := corev1.Volume{Name: "storage"}
	if storeEphemeral(store) {
		data.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	} else {
		data.VolumeSource = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: storeName(installation)}}
	}
	container := corev1.Container{
		Name:            "victoria-metrics",
		Image:           image,
		ImagePullPolicy: defaultImagePullPolicy(image),
		Args: []string{
			"-httpListenAddr=:" + strconv.Itoa(storePort),
			"-storageDataPath=/storage",
			"-retentionPeriod=" + storeRetention(store),
		},
		Resources:                resources,
		Ports:                    []corev1.ContainerPort{{Name: "http", ContainerPort: storePort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe:           defaultReadinessHTTPProbe("/health", "http"),
		LivenessProbe:            defaultHTTPProbe("/health", "http"),
		SecurityContext:          restrictedContainerSecurity(),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts:             []corev1.VolumeMount{{Name: "storage", MountPath: "/storage"}},
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: storeName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.To(int32(1)),
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			RevisionHistoryLimit:    ptr.To(int32(defaultRevisionHistoryLimit)),
			ProgressDeadlineSeconds: ptr.To(int32(defaultProgressDeadline)),
			// One writer on a ReadWriteOnce volume: the old pod stops first.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            storeName(installation),
					DeprecatedServiceAccount:      storeName(installation),
					AutomountServiceAccountToken:  ptr.To(false),
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: ptr.To(int64(defaultTerminationGrace)),
					DNSPolicy:                     corev1.DNSClusterFirst,
					SecurityContext:               restrictedPodSecurity(),
					SchedulerName:                 corev1.DefaultSchedulerName,
					Containers:                    []corev1.Container{container},
					Volumes:                       []corev1.Volume{data},
				},
			},
		},
	}
}
