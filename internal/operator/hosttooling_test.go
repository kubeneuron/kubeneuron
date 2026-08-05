package operator

import (
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
