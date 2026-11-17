// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// The managed collector scrapes the dcgm-exporter already in the cluster;
// KubeNeuron never installs it. The operator finds its pods and writes them
// to a file the collector watches, so the collector needs no Kubernetes API
// access and does not restart when exporter pods come and go.
const (
	defaultDCGMService   = "nvidia-dcgm-exporter"
	dcgmPortName         = "gpu-metrics"
	gpuMetricsCondition  = "ObservabilityGPUMetricsReady"
	dcgmTargetsKey       = "dcgm-exporter.json"
	dcgmTargetsMountPath = "/etc/otelcol-targets"
	dcgmScrapeInterval   = "30s"
	dcgmScrapeTimeout    = "10s"
	// dcgmSampleLimit fails a scrape that returns more samples than this,
	// so a broken exporter cannot flood the store. Eight GPUs with MIG and
	// the full profiling field set stay well under it.
	dcgmSampleLimit       = 5000
	dcgmRediscoveryPeriod = 2 * time.Minute
	// The labels dcgm-exporter adds for the workload holding a GPU.
	dcgmWorkloadLabelRegex = "pod|namespace|container|pod_uid|exported_pod|exported_namespace|exported_container|exported_pod_uid"
)

// nodePoolLabels derive node_pool, lowest precedence first: the same order
// as the dcgm-exporter job of the dependency profile's vmagent.
var nodePoolLabels = []string{
	"kubeneuron.io/node-pool",
	"karpenter.sh/nodepool",
	"cloud.google.com/gke-nodepool",
	"eks.amazonaws.com/nodegroup",
}

func dcgmScrapeSpec(installation *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.DCGMExporterScrapeSpec {
	if c := collectorSpec(installation); c != nil {
		return c.DCGMExporter
	}
	return nil
}

// dcgmScrapeEnabled: a Managed collector scrapes dcgm-exporter unless told
// not to.
func dcgmScrapeEnabled(installation *kubeneuronv1alpha1.KubeNeuron) bool {
	if collectorMode(installation) != kubeneuronv1alpha1.IntegrationModeManaged {
		return false
	}
	spec := dcgmScrapeSpec(installation)
	return spec == nil || spec.Scrape == nil || *spec.Scrape
}

func dcgmServiceName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	if spec := dcgmScrapeSpec(installation); spec != nil && spec.ServiceName != "" {
		return spec.ServiceName
	}
	return defaultDCGMService
}

func dcgmKeepWorkloadLabels(installation *kubeneuronv1alpha1.KubeNeuron) bool {
	spec := dcgmScrapeSpec(installation)
	return spec != nil && spec.KeepWorkloadLabels
}

func dcgmTargetsName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	return collectorName(installation) + "-targets"
}

// dcgmService is one dcgm-exporter Service and the pods behind it.
type dcgmService struct {
	Namespace string
	Name      string
	Selector  map[string]string
	Port      intstr.IntOrString
	Targets   []dcgmTarget
}

type dcgmTarget struct {
	Address  string
	Node     string
	NodePool string
	// HostNetwork targets are the node's address: a NetworkPolicy reaches
	// them by IP block, not by pod selector.
	HostNetwork bool
	IP          string
	Port        int32
}

type dcgmDiscovery struct {
	Services []dcgmService
}

func (d *dcgmDiscovery) targets() int {
	n := 0
	for _, s := range d.Services {
		n += len(s.Targets)
	}
	return n
}

func (r *KubeNeuronReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// discoverDCGMExporter finds the dcgm-exporter Services by name, in the
// configured namespace or in all, and the running pods behind their
// gpu-metrics port. It reads with the uncached reader: the operator keeps
// no cluster-wide cache of Services or Pods for this.
func (r *KubeNeuronReconciler) discoverDCGMExporter(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) (*dcgmDiscovery, error) {
	reader := r.reader()
	name := dcgmServiceName(installation)
	var services []corev1.Service
	if spec := dcgmScrapeSpec(installation); spec != nil && spec.Namespace != "" {
		var service corev1.Service
		err := reader.Get(ctx, types.NamespacedName{Name: name, Namespace: spec.Namespace}, &service)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("get dcgm-exporter Service %s/%s: %w", spec.Namespace, name, err)
		default:
			services = append(services, service)
		}
	} else {
		var list corev1.ServiceList
		if err := reader.List(ctx, &list, client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("metadata.name", name)}); err != nil {
			return nil, fmt.Errorf("list dcgm-exporter Services named %s: %w", name, err)
		}
		services = list.Items
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Namespace < services[j].Namespace })

	discovery := &dcgmDiscovery{}
	var nodes map[string]string
	for i := range services {
		service := &services[i]
		port, ok := gpuMetricsPort(service)
		if !ok || len(service.Spec.Selector) == 0 {
			continue
		}
		found := dcgmService{Namespace: service.Namespace, Name: service.Name, Selector: copyStringMap(service.Spec.Selector), Port: port}
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(service.Namespace),
			client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(service.Spec.Selector)}); err != nil {
			return nil, fmt.Errorf("list dcgm-exporter pods in %s: %w", service.Namespace, err)
		}
		for j := range pods.Items {
			pod := &pods.Items[j]
			if !podReady(pod) {
				continue
			}
			number, ok := resolvePodPort(pod, port)
			if !ok {
				continue
			}
			if nodes == nil {
				var err error
				if nodes, err = r.nodePools(ctx); err != nil {
					return nil, err
				}
			}
			pool := nodes[pod.Spec.NodeName]
			found.Targets = append(found.Targets, dcgmTarget{
				Address:     joinHostPort(pod.Status.PodIP, number),
				Node:        pod.Spec.NodeName,
				NodePool:    pool,
				HostNetwork: pod.Spec.HostNetwork,
				IP:          pod.Status.PodIP,
				Port:        number,
			})
		}
		sort.Slice(found.Targets, func(a, b int) bool { return found.Targets[a].Address < found.Targets[b].Address })
		discovery.Services = append(discovery.Services, found)
	}
	return discovery, nil
}

// podReady: only a Ready pod with an address is a target; a terminating
// pod is not.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func joinHostPort(ip string, port int32) string {
	if strings.Contains(ip, ":") {
		return "[" + ip + "]:" + strconv.Itoa(int(port))
	}
	return ip + ":" + strconv.Itoa(int(port))
}

func gpuMetricsPort(service *corev1.Service) (intstr.IntOrString, bool) {
	for _, port := range service.Spec.Ports {
		if port.Name != dcgmPortName || (port.Protocol != "" && port.Protocol != corev1.ProtocolTCP) {
			continue
		}
		if port.TargetPort.Type == intstr.String || port.TargetPort.IntValue() != 0 {
			return port.TargetPort, true
		}
		return intstr.FromInt32(port.Port), true
	}
	return intstr.IntOrString{}, false
}

func resolvePodPort(pod *corev1.Pod, port intstr.IntOrString) (int32, bool) {
	if port.Type == intstr.Int {
		return port.IntVal, port.IntVal > 0
	}
	for _, container := range pod.Spec.Containers {
		for _, p := range container.Ports {
			if p.Name == port.StrVal {
				return p.ContainerPort, true
			}
		}
	}
	return 0, false
}

// nodePools maps every node that has one to its pool. The operator keeps no
// Node cache, so this is one List per discovery, of metadata only, and only
// once there is a target to label.
func (r *KubeNeuronReconciler) nodePools(ctx context.Context) (map[string]string, error) {
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("NodeList"))
	if err := r.reader().List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	pools := make(map[string]string, len(list.Items))
	for _, node := range list.Items {
		pool := ""
		for _, key := range nodePoolLabels {
			if value := node.Labels[key]; value != "" {
				pool = value
			}
		}
		pools[node.Name] = pool
	}
	return pools, nil
}

// dcgmTargetsFile renders the discovered pods as Prometheus file_sd groups,
// labelled node and, when the node has one, node_pool. The order is fixed
// (services by namespace, targets by address), so an unchanged fleet
// rewrites nothing.
func dcgmTargetsFile(discovery *dcgmDiscovery) (string, error) {
	type group struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	groups := []group{}
	if discovery != nil {
		for _, service := range discovery.Services {
			for _, target := range service.Targets {
				l := map[string]string{"node": target.Node}
				if target.NodePool != "" {
					l["node_pool"] = target.NodePool
				}
				groups = append(groups, group{Targets: []string{target.Address}, Labels: l})
			}
		}
	}
	data, err := json.Marshal(groups)
	if err != nil {
		return "", fmt.Errorf("render dcgm-exporter targets: %w", err)
	}
	return string(data), nil
}

func dcgmTargetsConfigMap(installation *kubeneuronv1alpha1.KubeNeuron, discovery *dcgmDiscovery) (*corev1.ConfigMap, error) {
	data, err := dcgmTargetsFile(discovery)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dcgmTargetsName(installation),
			Namespace: installation.Spec.Namespace,
			Labels:    resourceLabels(installation, collectorComponent),
		},
		Data: map[string]string{dcgmTargetsKey: data},
	}, nil
}

// dcgmScrapeReceiver is the collector's Prometheus receiver for
// dcgm-exporter, with the label contract of the dependency profile's
// vmagent job: gpu_uuid, gpu_model and mig_profile copied from the
// exporter's own labels. The workload labels are dropped unless kept
// explicitly.
func dcgmScrapeReceiver(installation *kubeneuronv1alpha1.KubeNeuron) map[string]any {
	relabel := []any{
		map[string]any{"source_labels": []string{"UUID"}, "regex": "(.+)", "target_label": "gpu_uuid"},
		map[string]any{"source_labels": []string{"modelName"}, "regex": "(.+)", "target_label": "gpu_model"},
		map[string]any{"source_labels": []string{"GPU_I_PROFILE"}, "regex": "(.+)", "target_label": "mig_profile"},
	}
	if !dcgmKeepWorkloadLabels(installation) {
		relabel = append(relabel, map[string]any{"action": "labeldrop", "regex": dcgmWorkloadLabelRegex})
	}
	return map[string]any{"config": map[string]any{"scrape_configs": []any{map[string]any{
		"job_name":        "dcgm-exporter",
		"scrape_interval": dcgmScrapeInterval,
		"scrape_timeout":  dcgmScrapeTimeout,
		"sample_limit":    dcgmSampleLimit,
		"file_sd_configs": []any{map[string]any{
			"files":            []string{dcgmTargetsMountPath + "/" + dcgmTargetsKey},
			"refresh_interval": "1m",
		}},
		"metric_relabel_configs": relabel,
	}}}}
}

func gpuMetricsStatus(installation *kubeneuronv1alpha1.KubeNeuron, discovery *dcgmDiscovery) *metav1.Condition {
	where := "in any namespace"
	if spec := dcgmScrapeSpec(installation); spec != nil && spec.Namespace != "" {
		where = "in namespace " + spec.Namespace
	}
	if discovery == nil || len(discovery.Services) == 0 {
		return notReady("NotFound", fmt.Sprintf(
			"no dcgm-exporter found: no Service %s with a %s port %s; KubeNeuron does not install dcgm-exporter",
			dcgmServiceName(installation), dcgmPortName, where))
	}
	var names []string
	for _, s := range discovery.Services {
		names = append(names, s.Namespace+"/"+s.Name)
	}
	if discovery.targets() == 0 {
		return notReady("NoReadyPods", "dcgm-exporter Service "+strings.Join(names, ", ")+" has no Ready pods")
	}
	return &metav1.Condition{
		Status:  metav1.ConditionTrue,
		Reason:  "Scraping",
		Message: fmt.Sprintf("scraping %d Ready dcgm-exporter pods behind %s", discovery.targets(), strings.Join(names, ", ")),
	}
}

// dcgmResourceProcessor stamps the scraped series with the installation, the
// same resource attribute the components' own metrics carry, so one filter
// selects both.
func dcgmResourceProcessor(installation *kubeneuronv1alpha1.KubeNeuron) map[string]any {
	return map[string]any{"attributes": []any{map[string]any{
		"key": "kubeneuron.installation", "value": installation.Name, "action": "upsert",
	}}}
}

// dcgmEgress lets the collector reach the exporter pods it scrapes and
// nothing else: by namespace and pod selector on the pod network, and by
// the node's address for a host-network exporter, which no pod selector
// matches.
func dcgmEgress(dcgm *dcgmDiscovery) []networkingv1.NetworkPolicyEgressRule {
	if dcgm == nil {
		return nil
	}
	var rules []networkingv1.NetworkPolicyEgressRule
	for _, service := range dcgm.Services {
		// Ports by number, resolved on each pod: not every CNI honours a
		// named port in an egress rule.
		podPorts := map[int32]bool{}
		hostPorts := map[int32][]string{}
		for _, t := range service.Targets {
			if t.HostNetwork {
				hostPorts[t.Port] = append(hostPorts[t.Port], t.IP)
			} else {
				podPorts[t.Port] = true
			}
		}
		if len(podPorts) > 0 {
			numbers := make([]int32, 0, len(podPorts))
			for p := range podPorts {
				numbers = append(numbers, p)
			}
			sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
			ports := make([]networkingv1.NetworkPolicyPort, 0, len(numbers))
			for _, p := range numbers {
				ports = append(ports, tcpPort(p))
			}
			rules = append(rules, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": service.Namespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: copyStringMap(service.Selector)},
				}},
				Ports: ports,
			})
		}
		ports := make([]int32, 0, len(hostPorts))
		for p := range hostPorts {
			ports = append(ports, p)
		}
		sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
		for _, p := range ports {
			ips := hostPorts[p]
			sort.Strings(ips)
			var peers []networkingv1.NetworkPolicyPeer
			seen := map[string]bool{}
			for _, ip := range ips {
				cidr := ip + "/32"
				if strings.Contains(ip, ":") {
					cidr = ip + "/128"
				}
				if seen[cidr] {
					continue
				}
				seen[cidr] = true
				peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
			}
			rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: peers, Ports: []networkingv1.NetworkPolicyPort{tcpPort(p)}})
		}
	}
	return rules
}
