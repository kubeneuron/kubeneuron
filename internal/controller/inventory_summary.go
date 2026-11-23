// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/ptr"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/inventory"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// InventorySummary reports observed hardware and runtime attestation. It never
// requests an action or changes a remediation gate.
func (c *Controller) InventorySummary(ctx context.Context) (inventory.Summary, error) {
	now := time.Now().UTC()
	s := inventory.Summary{ObservedAt: now}
	// Rows are sorted; report one affected pool and direct operators to the full list.
	for _, row := range c.PoolAvailability().Pools {
		if row.Cloud == "EKS" && row.ReasonCode == cloud.PoolFixedSizeUndeclared {
			s.UndeclaredEKSPool = row.Pool
			break
		}
	}
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return s, err
	}
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return s, errors.New("accelerator report store unavailable")
	}
	currentNodes := map[string]types.Node{}
	if c.platform != nil {
		current, err := c.platform.ListNodes(ctx)
		if err != nil {
			return s, err
		}
		s.AdvertisedNodes = len(current)
		for _, n := range current {
			currentNodes[n.Name] = n
		}
	}
	rc := c.runtimeConfig(ctx)
	s.NodeLevelActions = inventory.ActionsDisabled
	if rc.NodeLevelActions {
		s.NodeLevelActions = inventory.ActionsEnabled
	}
	profiles := config.Config{AcceleratorProfiles: rc.AcceleratorProfiles}
	var byNode map[string][]*types.AgentAcceleratorReport
	for _, node := range nodes {
		if node.AgentLastSeen.After(now) || node.AgentLastSeen.IsZero() {
			continue
		}
		if node.AgentLastSeen.After(s.LastReportAt) {
			s.LastReportAt = node.AgentLastSeen
		}
		if now.Sub(node.AgentLastSeen) > 3*inventory.Interval {
			continue
		}
		s.ReportingNodes++
		s.GPUs += len(node.GPUs)
		if s.ResetConfirmed || len(node.GPUs) == 0 || node.UID == "" {
			continue
		}
		current := currentNodes[node.Name]
		if current.UID == "" || current.UID != node.UID {
			continue
		}
		profile, err := profiles.ResolveAcceleratorRuntimeProfile(current.Labels, types.AcceleratorVendorNVIDIA)
		if err != nil {
			continue
		}
		if byNode == nil {
			all, err := reports.ListAcceleratorReports(ctx, "")
			if err != nil {
				return s, err
			}
			byNode = make(map[string][]*types.AgentAcceleratorReport)
			for _, report := range all {
				if report.Vendor == types.AcceleratorVendorNVIDIA {
					byNode[report.Node] = append(byNode[report.Node], report)
				}
			}
		}
		for _, report := range byNode[node.Name] {
			if report.Vendor != types.AcceleratorVendorNVIDIA || report.ObservedAt.After(now) {
				continue
			}
			for _, gpu := range node.GPUs {
				if checkNVIDIAResetEvidence(profile, report, node.UID, types.Target{Node: node.Name, GPUUUID: gpu.UUID}) == nil {
					s.ResetConfirmed = true
					break
				}
			}
		}
	}
	// Agents that do not run with the owner's RuntimeClass are named even
	// while others report: a rollout it stopped leaves the old agents running.
	// A failed read leaves it to the diagnostics below, which read the same.
	if c.runtimeClassDiagnostic != nil {
		if reason, nodes, err := c.runtimeClassDiagnostic(ctx); err == nil && reason != "" {
			s.Diagnostic, s.DiagnosticNodes = reason, nodes
			return s, nil
		}
	}
	if s.GPUs == 0 && (s.AdvertisedNodes > 0 || s.ReportingNodes == 0) && c.inventoryDiagnostic != nil {
		s.Diagnostic, err = c.inventoryDiagnostic(ctx)
		if err != nil {
			return s, err
		}
		if s.Diagnostic == "RuntimeDidNotExposeGPUs" || s.Diagnostic == "DeviceAccessDisabled" {
			if s.NVIDIANodes, err = c.nvidiaNodes(ctx, s.AdvertisedNodes); err != nil {
				return s, err
			}
		}
		switch {
		case (s.Diagnostic == "RuntimeDidNotExposeGPUs" || s.Diagnostic == "DeviceAccessDisabled") && s.NVIDIANodes == 0,
			s.Diagnostic == "RuntimeDidNotExposeGPUs" && s.ReportingNodes == 0:
			// About the NVIDIA devices agents report: a fleet that
			// advertises none, or whose agents have not reported yet, is
			// left to the summary's counts.
			s.Diagnostic = ""
		}
	}
	return s, nil
}

func (c *Controller) PublishInventory(ctx context.Context, maps typedcore.ConfigMapInterface, installation, uid string) {
	ticker := time.NewTicker(inventory.Interval)
	defer ticker.Stop()
	for {
		if err := c.publishInventory(ctx, maps, installation, uid); err != nil && ctx.Err() == nil {
			c.log.Warn("could not publish inventory summary", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.inventoryWake:
			// Coalesce bursts of registrations before reading fleet-wide state.
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		case <-ticker.C:
		}
	}
}

func (c *Controller) publishInventory(ctx context.Context, maps typedcore.ConfigMapInterface, installation, uid string) error {
	cm, err := maps.Get(ctx, installation+inventory.Suffix, metav1.GetOptions{})
	if err != nil {
		return err
	}
	owner := metav1.GetControllerOf(cm)
	if uid == "" || owner == nil || string(owner.UID) != uid || owner.Name != installation || owner.Kind != "KubeNeuron" {
		return errors.New("inventory ConfigMap belongs to another installation")
	}
	s, err := c.InventorySummary(ctx)
	if err != nil {
		return err
	}
	// Heartbeats can arrive on every node at once. Publish changes immediately,
	// but refresh unchanged aggregate values at most once per interval.
	if previous, err := inventory.Parse(cm.Data, s.ObservedAt); err == nil && s.ObservedAt.Sub(previous.ObservedAt) < inventory.Interval {
		previous.ObservedAt, previous.LastReportAt = s.ObservedAt, s.LastReportAt
		if previous == s {
			return nil
		}
	}
	cm.Data = inventory.Merge(cm.Data, s)
	_, err = maps.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func (c *Controller) notifyInventory() {
	select {
	case c.inventoryWake <- struct{}{}:
	default:
	}
}

// ConfigureInventoryDiagnostics binds startup diagnostics to the installed agent
// DaemonSet. Call before serving requests or starting the publisher.
func (c *Controller) ConfigureInventoryDiagnostics(kube kubernetes.Interface, namespace, installation, uid string) {
	agents := func(ctx context.Context) (*appsv1.DaemonSet, []corev1.Pod, error) {
		ds, err := kube.AppsV1().DaemonSets(namespace).Get(ctx, installation+"-agent", metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		owner := metav1.GetControllerOf(ds)
		if owner == nil || string(owner.UID) != uid {
			return nil, nil, errors.New("agent DaemonSet belongs to another installation")
		}
		pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(ds.Spec.Selector.MatchLabels).String()})
		if err != nil {
			return nil, nil, err
		}
		var owned []corev1.Pod
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp == nil && metav1.IsControlledBy(&pod, ds) {
				owned = append(owned, pod)
			}
		}
		return ds, owned, nil
	}
	c.runtimeClassDiagnostic = func(ctx context.Context) (string, int, error) {
		ds, pods, err := agents(ctx)
		if err != nil || ds.Spec.Template.Spec.RuntimeClassName == nil {
			return "", 0, err
		}
		reason, nodes := runtimeClassDiagnostic(ds, pods, time.Now())
		return reason, nodes, nil
	}
	c.inventoryDiagnostic = func(ctx context.Context) (string, error) {
		ds, pods, err := agents(ctx)
		if err != nil {
			return "", err
		}
		for _, container := range ds.Spec.Template.Spec.Containers {
			switch {
			case container.Name != "agent" || slices.Contains(container.Args, "--require-real-driver"):
			case slices.Contains(container.Args, "--enable-node-level-actions"):
				return "HostToolingNotConfigured", nil
			case slices.Contains(container.Args, "--nvidia-runtime-devices"):
				// The agent runs on whatever the runtime gave it, so its
				// Pods say nothing: no GPU is all there is to go by.
				return "RuntimeDidNotExposeGPUs", nil
			default:
				return "DeviceAccessDisabled", nil
			}
		}
		for _, pod := range pods {
			for _, container := range pod.Status.ContainerStatuses {
				if container.Name != "agent" {
					continue
				}
				state := container.State.Terminated
				if state == nil && container.State.Waiting != nil && container.State.Waiting.Reason == "CrashLoopBackOff" {
					state = container.LastTerminationState.Terminated
				}
				if diagnostic := startupDiagnostic(state); diagnostic != "" {
					return diagnostic, nil
				}
			}
		}
		return "", nil
	}
}

// sandboxGrace is how long an agent Pod may wait for its sandbox before a
// RuntimeClass the owner named is taken to be what stops it.
const sandboxGrace = 2 * time.Minute

// runtimeClassDiagnostic says why agent Pods of a DaemonSet with an
// owner-named RuntimeClass do not run, and on how many nodes where it counts
// them. No Pod of the current template exists while the DaemonSet wants one
// and some are unavailable, as when the RuntimeClass does not exist: on a
// fresh install, or a rollout that removed an old Pod and could not create
// its successor. Or Pods with the RuntimeClass wait for their sandbox past
// the grace, as when its handler is not configured on their nodes. "" when
// neither holds.
func runtimeClassDiagnostic(ds *appsv1.DaemonSet, pods []corev1.Pod, now time.Time) (string, int) {
	class := *ds.Spec.Template.Spec.RuntimeClassName
	current := slices.ContainsFunc(pods, func(pod corev1.Pod) bool { return ptr.Deref(pod.Spec.RuntimeClassName, "") == class })
	if !current && ds.Status.ObservedGeneration >= ds.Generation && ds.Status.DesiredNumberScheduled > 0 &&
		ds.Status.UpdatedNumberScheduled == 0 && ds.Status.NumberUnavailable > 0 {
		return "AgentRuntimeClassPodsNotCreated", 0
	}
	nodes := map[string]bool{}
	for _, pod := range pods {
		if ptr.Deref(pod.Spec.RuntimeClassName, "") != class || now.Sub(pod.CreationTimestamp.Time) < sandboxGrace {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReadyToStartContainers && condition.Status == corev1.ConditionFalse {
				nodes[cmp.Or(pod.Spec.NodeName, pod.Name)] = true
			}
		}
	}
	if len(nodes) > 0 {
		return "AgentRuntimeClassNotStarting", len(nodes)
	}
	return "", 0
}

// nvidiaNodes counts the nodes that advertise nvidia.com/gpu. ListNodes does
// not say which resource a node advertises, so it is read only for the
// diagnostic that names them, and never above advertised: the two reads may
// see the cache a moment apart.
func (c *Controller) nvidiaNodes(ctx context.Context, advertised int) (int, error) {
	devices, ok := c.platform.(platform.AcceleratorInventory)
	if !ok || advertised == 0 {
		return 0, nil
	}
	all, err := devices.NodeInventory(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, node := range all {
		if advertisesNVIDIA(node) {
			n++
		}
	}
	return min(n, advertised), nil
}

func advertisesNVIDIA(n types.Node) bool {
	for _, d := range n.Devices {
		if d.Resource == "nvidia.com/gpu" && d.Capacity > 0 {
			return true
		}
	}
	return false
}

func startupDiagnostic(state *corev1.ContainerStateTerminated) string {
	if state != nil {
		switch state.ExitCode {
		case inventory.ExitSMIMissing:
			return "SMIMissing"
		case inventory.ExitDriverUnavailable:
			return "DriverUnavailable"
		}
	}
	return ""
}
