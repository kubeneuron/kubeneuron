package controller

import (
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func gpuNode(labels map[string]string) *types.Node {
	return &types.Node{Name: "gpu-1", UID: "uid-1", Labels: labels}
}

func holder(command string, pid int, device string) types.AgentDeviceHolder {
	return types.AgentDeviceHolder{PID: pid, Command: command, Device: device}
}

// The measured EKS case: the device plugin comes from the machine image, so the
// GPU Operator's per-component label is absent and no label flip will remove it.
// That must be visible before a playbook cordons and drains the node.
func TestUnmanagedDevicePluginIsAnObstruction(t *testing.T) {
	node := gpuNode(map[string]string{
		"nvidia.com/gpu.deploy.dcgm":          "true",
		"nvidia.com/gpu.deploy.dcgm-exporter": "true",
		// no nvidia.com/gpu.deploy.device-plugin
	})
	got := resetObstructions(node, []types.AgentDeviceHolder{
		holder("nv-hostengine", 11850, "/dev/nvidia0"),
		holder("dcgm-exporter", 12382, "/dev/nvidia0"),
		holder("nvidia-persiste", 8140, "/dev/nvidia0"),
		holder("nvidia-device-p", 11567, "/dev/nvidia0"),
	}, nil)

	if len(got) != 1 {
		t.Fatalf("obstructions = %v, want only the unmanaged device plugin", got)
	}
	if got[0].Holder.Command != "nvidia-device-p" {
		t.Fatalf("obstruction = %+v", got[0])
	}
	// The message has to be actionable, not just a complaint.
	for _, want := range []string{"not managed by the GPU Operator", "machine image"} {
		if !strings.Contains(got[0].Reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", got[0].Reason, want)
		}
	}
}

func TestOperatorManagedHoldersAreNotObstructions(t *testing.T) {
	node := gpuNode(map[string]string{
		"nvidia.com/gpu.deploy.dcgm":          "true",
		"nvidia.com/gpu.deploy.dcgm-exporter": "true",
		"nvidia.com/gpu.deploy.device-plugin": "true",
	})
	got := resetObstructions(node, []types.AgentDeviceHolder{
		holder("nv-hostengine", 1, "/dev/nvidia0"),
		holder("dcgm-exporter", 2, "/dev/nvidia0"),
		holder("nvidia-device-p", 3, "/dev/nvidia0"),
		// Released by the agent on the node; no label needs to exist.
		holder("nvidia-persiste", 4, "/dev/nvidia0"),
	}, nil)
	if len(got) != 0 {
		t.Fatalf("obstructions = %v, want none — the quiesce can release all of these", got)
	}
}

// Some processes hold the GPU and must not be stopped. fabricmanager is the
// standing example: killing it to make room for a remediation would break the
// GPUs it manages.
func TestDeclaredForbiddenHolderRefusesTheReset(t *testing.T) {
	node := gpuNode(map[string]string{"nvidia.com/gpu.deploy.dcgm": "true"})
	got := resetObstructions(node,
		[]types.AgentDeviceHolder{holder("nv-fabricmanager", 900, "/dev/nvidia0")},
		[]string{"nv-fabricmanager"})
	if len(got) != 1 || !strings.Contains(got[0].Reason, "forbidResetWhenPresent") {
		t.Fatalf("obstructions = %v, want the declared refusal", got)
	}
}

func TestUnknownHolderIsAnObstructionWithRemediation(t *testing.T) {
	got := resetObstructions(gpuNode(nil),
		[]types.AgentDeviceHolder{holder("datadog-agent", 555, "/dev/nvidia0")}, nil)
	if len(got) != 1 {
		t.Fatalf("obstructions = %v", got)
	}
	// An operator must learn both options rather than only that it failed.
	for _, want := range []string{"no way to release", "forbidResetWhenPresent"} {
		if !strings.Contains(got[0].Reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", got[0].Reason, want)
		}
	}
}

// A process holding three device nodes is one thing for an operator to fix.
func TestObstructionsAreReportedOncePerProcess(t *testing.T) {
	got := resetObstructions(gpuNode(nil), []types.AgentDeviceHolder{
		holder("datadog-agent", 555, "/dev/nvidia0"),
		holder("datadog-agent", 555, "/dev/nvidia1"),
		holder("datadog-agent", 555, "/dev/nvidiactl"),
	}, nil)
	if len(got) != 1 {
		t.Fatalf("obstructions = %v, want one entry per process", got)
	}
}

// An empty list is evidence the node looked and found nothing. A blank declared
// name must not match it and forbid every reset.
func TestNoHoldersMeansNoObstructions(t *testing.T) {
	if got := resetObstructions(gpuNode(nil), []types.AgentDeviceHolder{}, []string{"", "  "}); len(got) != 0 {
		t.Fatalf("obstructions = %v, want none", got)
	}
}
