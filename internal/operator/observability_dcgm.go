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
	defaultDCGMService     = "nvidia-dcgm-exporter"
	dcgmPortName           = "gpu-metrics"
	gpuMetricsCondition    = "ObservabilityGPUMetricsReady"
	dcgmTargetsKey         = "dcgm-exporter.json"
	dcgmTargetsMountPath   = "/etc/otelcol-targets"
	dcgmScrapeInterval     = "30s"
	dcgmRediscoveryPeriod  = 2 * time.Minute
	dcgmWorkloadLabelRegex = "pod|namespace|container|exported_pod|exported_namespace|exported_container"
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
	nodes := map[string]string{}
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
			if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
				continue
			}
			number, ok := resolvePodPort(pod, port)
			if !ok {
				continue
			}
			pool, seen := nodes[pod.Spec.NodeName]
			if !seen {
				var err error
				if pool, err = r.nodePool(ctx, pod.Spec.NodeName); err != nil {
					return nil, err
				}
				nodes[pod.Spec.NodeName] = pool
			}
			found.Targets = append(found.Targets, dcgmTarget{
				Address:  joinHostPort(pod.Status.PodIP, number),
				Node:     pod.Spec.NodeName,
				NodePool: pool,
			})
		}
		sort.Slice(found.Targets, func(a, b int) bool { return found.Targets[a].Address < found.Targets[b].Address })
		discovery.Services = append(discovery.Services, found)
	}
	return discovery, nil
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

func (r *KubeNeuronReconciler) nodePool(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	var node corev1.Node
	if err := r.reader().Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get node %s: %w", name, err)
	}
	pool := ""
	for _, key := range nodePoolLabels {
		if value := node.Labels[key]; value != "" {
			pool = value
		}
	}
	return pool, nil
}

// dcgmTargetsFile renders the discovered pods as Prometheus file_sd groups,
// labelled node and, when the node has one, node_pool.
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
		return notReady("NoTargets", "dcgm-exporter Service "+strings.Join(names, ", ")+" has no running pods")
	}
	return &metav1.Condition{
		Status:  metav1.ConditionTrue,
		Reason:  "Scraping",
		Message: fmt.Sprintf("scraping %d dcgm-exporter pods behind %s", discovery.targets(), strings.Join(names, ", ")),
	}
}
