package operator

import (
	"reflect"
	"strings"
	"testing"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// The agent's PATH starts with the host tooling directory, because nvidia-smi
// must come from the host to match the host driver. That must not silently drag
// DCGM attestation along with it: the attested version gates destructive
// actions, and a profile pins one exact value for the whole fleet.
func TestAgentUsesTheBundledDCGMClientUnlessOneIsNamed(t *testing.T) {
	args, env, _, _ := agentHostToolingWiring(&kubeneuronv1alpha1.HostToolingSpec{
		BinDir: "/usr/bin", LibDirs: []string{"/usr/lib64"},
	})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--nvidia-dcgmi") {
		t.Fatalf("args = %v, want no client override so the agent's bundled default applies", args)
	}
	var path string
	for _, e := range env {
		if e.Name == "PATH" {
			path = e.Value
		}
	}
	if !strings.HasPrefix(path, hostToolingBinMount+":") {
		t.Fatalf("PATH = %q, want the host tooling directory first for nvidia-smi", path)
	}
}

func TestNamedDCGMClientIsWiredThroughTheMountedPath(t *testing.T) {
	args, _, _, _ := agentHostToolingWiring(&kubeneuronv1alpha1.HostToolingSpec{
		BinDir: "/opt/nvidia/bin", DCGMIPath: "/opt/nvidia/bin/dcgmi",
	})
	want := "--nvidia-dcgmi=" + hostToolingBinMount + "/dcgmi"
	found := false
	for _, a := range args {
		if a == want {
			found = true
		}
	}
	if !found {
		// The host path is not the container path; only binDir is mounted.
		t.Fatalf("args = %v, want %q", args, want)
	}
}

func TestDCGMClientOutsideBinDirIsRejectedAtCompileTime(t *testing.T) {
	err := validateHostTooling(&kubeneuronv1alpha1.HostToolingSpec{
		BinDir: "/usr/bin", DCGMIPath: "/opt/dcgm/bin/dcgmi",
	})
	if err == nil || !strings.Contains(err.Error(), "must be directly inside binDir") {
		t.Fatalf("err = %v, want a compile-time rejection: that path would not exist in the container", err)
	}

	if err := validateHostTooling(&kubeneuronv1alpha1.HostToolingSpec{
		BinDir: "/usr/bin", DCGMIPath: "/usr/bin/dcgmi",
	}); err != nil {
		t.Fatalf("client inside binDir = %v, want accepted", err)
	}

	// binDir defaults to /usr/bin when unset, and the check must use that.
	if err := validateHostTooling(&kubeneuronv1alpha1.HostToolingSpec{
		DCGMIPath: "/usr/bin/dcgmi",
	}); err != nil {
		t.Fatalf("client inside the default binDir = %v, want accepted", err)
	}
	if err := validateHostTooling(&kubeneuronv1alpha1.HostToolingSpec{
		DCGMIPath: "/usr/local/bin/dcgmi",
	}); err == nil {
		t.Fatal("want rejection against the default binDir")
	}
}

// TestConfigEditDoesNotRollTheAgentFleet covers a leftover from the retired
// two-DaemonSet era: the agent pod template carried the config digest.
//
// The agent mounts no ConfigMap and takes no config-file flag. It boots unarmed
// and receives its arming and its profile from the controller at registration,
// so nothing in the snapshot reaches it through its own spec. With the digest
// on the POD template, editing any child CR — one GPUSignalMapping, to quiet a
// noisy XID — changed the template and rolled every agent in the fleet,
// serially at maxUnavailable 1. On 500 nodes that is a multi-hour rolling blind
// spot walking the fleet: for each node's gap there is no kmsg watcher and no
// DCGM poll, which is detection latency on precisely the node that might be
// failing during the window.
func TestConfigEditDoesNotRollTheAgentFleet(t *testing.T) {
	inst := testKubeNeuron()

	before := agentDaemonSet(inst, &Snapshot{Digest: "a8b30c2e"}, "tls-rev-1")
	// An unrelated child CR is edited, so the snapshot digest moves.
	after := agentDaemonSet(inst, &Snapshot{Digest: "309e7c1b"}, "tls-rev-1")

	if !reflect.DeepEqual(before.Spec.Template, after.Spec.Template) {
		t.Fatal("an unrelated configuration edit changed the agent pod template, so every agent " +
			"in the fleet restarts one node at a time for a snapshot the agent never reads")
	}

	// And the digest is still on the DaemonSet object, where it says which
	// snapshot the install is on without causing a rollout.
	if before.Annotations["kubeneuron.io/config-digest"] == "" {
		t.Fatal("the config digest was dropped from the DaemonSet object too; the rollout is " +
			"fixed but the observability it provided is gone")
	}

	// A TLS rotation MUST still roll the fleet: the agent reads that material
	// from its mounted Secret, so a pod that does not restart keeps presenting
	// a retired client certificate.
	rotated := agentDaemonSet(inst, &Snapshot{Digest: "a8b30c2e"}, "tls-rev-2")
	if reflect.DeepEqual(before.Spec.Template, rotated.Spec.Template) {
		t.Fatal("a TLS rotation did not change the agent pod template; agents would keep using " +
			"the old client certificate until something else restarted them")
	}
}
