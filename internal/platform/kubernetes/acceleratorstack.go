package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// The NVIDIA GPU Operator selects each of its DaemonSets with a
// nvidia.com/gpu.deploy.<component> node label and removes the component's pod
// from any node whose label is not "true". That is the vendor's own supported
// switch — it is how the GPU Operator itself stands its stack down for driver
// upgrades — so KubeNeuron uses it rather than touching NVIDIA's workloads
// directly.
//
// Only the components that hold an open handle on the GPU device nodes are
// listed. Measured on a stock GPU Operator install: nv-hostengine (dcgm),
// dcgm-exporter and the device plugin each keep /dev/nvidia0 open while
// running, and every one of them must be gone before nvidia-smi --gpu-reset
// can succeed.
var acceleratorStackComponents = []string{
	"dcgm",
	"dcgm-exporter",
	"device-plugin",
}

const (
	// acceleratorStackLabelPrefix is the GPU Operator's per-component switch.
	acceleratorStackLabelPrefix = "nvidia.com/gpu.deploy."
	// acceleratorStackQuiescedAnnotation records which components KubeNeuron
	// switched off, so a restore puts back exactly what it took away and never
	// enables a component the cluster had deliberately disabled.
	acceleratorStackQuiescedAnnotation = "kubeneuron.io/quiesced-accelerator-stack"
	// acceleratorHostQuiescedAnnotation survives even when no GPU Operator
	// component carried an explicit deploy=true label. The host action may still
	// stop nvidia-persistenced, so recovery needs a durable marker for it too.
	acceleratorHostQuiescedAnnotation = "kubeneuron.io/quiesced-accelerator-host"
)

var _ platform.AcceleratorStackController = (*Platform)(nil)

// QuiesceAcceleratorStack switches off the NVIDIA components that hold the GPU
// device nodes and records which ones it changed.
//
// Components that were already off are left alone and not recorded: restoring
// must not turn on something the cluster had switched off for its own reasons.
func (p *Platform) QuiesceAcceleratorStack(ctx context.Context, node string) ([]string, error) {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	labels := map[string]*string{}
	var quiesced []string
	for _, component := range acceleratorStackComponents {
		key := acceleratorStackLabelPrefix + component
		value, present := current.Labels[key]
		if !present || value != "true" {
			continue // absent or already off: nothing of ours to undo later
		}
		labels[key] = strPtr("false")
		quiesced = append(quiesced, component)
	}
	// Merge with anything a previous quiesce recorded: a retry must not lose
	// the first attempt's record, or a restore would leave components off.
	recorded := union(readQuiescedComponents(current.Annotations), quiesced)
	if len(labels) == 0 && equalSets(recorded, readQuiescedComponents(current.Annotations)) && current.Annotations[acceleratorHostQuiescedAnnotation] == "true" {
		return nil, nil
	}
	if err := p.patchNode(ctx, node, labels, recorded); err != nil {
		return nil, err
	}
	return quiesced, nil
}

// RestoreAcceleratorStack switches the recorded components back on. It is safe
// on a node that was never quiesced, and safe to call twice.
func (p *Platform) RestoreAcceleratorStack(ctx context.Context, node string) ([]string, error) {
	current, err := p.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	recorded := readQuiescedComponents(current.Annotations)
	if len(recorded) == 0 && current.Annotations[acceleratorHostQuiescedAnnotation] != "true" {
		return nil, nil
	}
	labels := map[string]*string{}
	for _, component := range recorded {
		labels[acceleratorStackLabelPrefix+component] = strPtr("true")
	}
	if err := p.patchNode(ctx, node, labels, nil); err != nil {
		return nil, err
	}
	return recorded, nil
}

// QuiescedNodes lists nodes carrying KubeNeuron's quiesce record. The record is
// on the node rather than in the controller precisely so a controller restart
// cannot lose track of monitoring it switched off.
func (p *Platform) QuiescedNodes(ctx context.Context) ([]string, error) {
	nodes, err := p.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range nodes.Items {
		if len(readQuiescedComponents(nodes.Items[i].Annotations)) != 0 || nodes.Items[i].Annotations[acceleratorHostQuiescedAnnotation] == "true" {
			out = append(out, nodes.Items[i].Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p *Platform) patchNode(ctx context.Context, node string, labels map[string]*string, quiesced []string) error {
	annotations := map[string]*string{}
	if len(quiesced) == 0 {
		annotations[acceleratorStackQuiescedAnnotation] = nil
	} else {
		annotations[acceleratorStackQuiescedAnnotation] = strPtr(strings.Join(quiesced, ","))
	}
	if quiesced == nil {
		annotations[acceleratorHostQuiescedAnnotation] = nil
	} else {
		annotations[acceleratorHostQuiescedAnnotation] = strPtr("true")
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": labels, "annotations": annotations},
	})
	if err != nil {
		return err
	}
	_, err = p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patching accelerator stack labels on %s: %w", node, err)
	}
	return nil
}

func readQuiescedComponents(annotations map[string]string) []string {
	raw := strings.TrimSpace(annotations[acceleratorStackQuiescedAnnotation])
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	sort.Strings(out)
	return out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	for _, v := range append(append([]string(nil), a...), b...) {
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func strPtr(s string) *string { return &s }
