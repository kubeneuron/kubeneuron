// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/inventory"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// InventorySummary reports observed hardware and runtime attestation. It never
// requests an action or changes a remediation gate.
func (c *Controller) InventorySummary(ctx context.Context) (inventory.Summary, error) {
	now := time.Now().UTC()
	s := inventory.Summary{ObservedAt: now}
	nodes, err := c.Nodes(ctx)
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
	profiles := config.Config{AcceleratorProfiles: c.runtimeConfig(ctx).AcceleratorProfiles}
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
	if s.GPUs == 0 && (s.AdvertisedNodes > 0 || s.ReportingNodes == 0) && c.inventoryDiagnostic != nil {
		s.Diagnostic, err = c.inventoryDiagnostic(ctx)
		if err != nil {
			return s, err
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
	c.inventoryDiagnostic = func(ctx context.Context) (string, error) {
		ds, err := kube.AppsV1().DaemonSets(namespace).Get(ctx, installation+"-agent", metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		owner := metav1.GetControllerOf(ds)
		if owner == nil || string(owner.UID) != uid {
			return "", errors.New("agent DaemonSet belongs to another installation")
		}
		for _, container := range ds.Spec.Template.Spec.Containers {
			if container.Name == "agent" && !slices.Contains(container.Args, "--require-real-driver") {
				return "HostToolingNotConfigured", nil
			}
		}
		pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(ds.Spec.Selector.MatchLabels).String()})
		if err != nil {
			return "", err
		}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || !metav1.IsControlledBy(&pod, ds) {
				continue
			}
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
