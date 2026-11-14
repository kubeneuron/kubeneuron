// SPDX-License-Identifier: LicenseRef-KubeNeuron

package operator

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

func agentContainer(t *testing.T, installation *kubeneuronv1alpha1.KubeNeuron) corev1.Container {
	t.Helper()
	ds := agentDaemonSet(installation, &Snapshot{Digest: "abc123"})
	return ds.Spec.Template.Spec.Containers[0]
}

func adapterArgs(args []string) []string {
	var out []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--adapter") {
			out = append(out, arg)
		}
	}
	return out
}

func TestOperatorRendersAdapterFlagsFromTheCR(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Agent.Adapters = []kubeneuronv1alpha1.AgentAdapterSpec{
		{Vendor: "intel"},
		{Vendor: "amd", Options: map[string]string{"tool": "amd-smi", "interval": "30s", "expr": "a=b"}},
	}
	container := agentContainer(t, installation)
	want := []string{
		"--adapter=amd",
		"--adapter-option=amd.expr=a=b",
		"--adapter-option=amd.interval=30s",
		"--adapter-option=amd.tool=amd-smi",
		"--adapter=intel",
	}
	if got := adapterArgs(container.Args); !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter args = %q, want %q", got, want)
	}
	// NVIDIA observation stays on exactly as before.
	if !containsArg(container.Args, "--nvidia-observation") || !containsArg(container.Args, "--nvidia-controller-profile") {
		t.Fatalf("NVIDIA observation flags missing: %v", container.Args)
	}
	// The order is stable however the list is written.
	installation.Spec.Agent.Adapters[0], installation.Spec.Agent.Adapters[1] = installation.Spec.Agent.Adapters[1], installation.Spec.Agent.Adapters[0]
	if got := adapterArgs(agentContainer(t, installation).Args); !reflect.DeepEqual(got, want) {
		t.Fatalf("reordered adapter args = %q, want %q", got, want)
	}
}

func TestNoAdaptersRendersTheSameAgentAsBefore(t *testing.T) {
	plain := testKubeNeuron()
	listed := testKubeNeuron()
	listed.Spec.Agent.Adapters = []kubeneuronv1alpha1.AgentAdapterSpec{}
	a, b := agentContainer(t, plain), agentContainer(t, listed)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("an empty adapter list changed the agent container:\n%+v\n%+v", a, b)
	}
	if got := adapterArgs(a.Args); len(got) != 0 {
		t.Fatalf("default install renders adapter flags: %v", got)
	}
	for _, env := range a.Env {
		if env.Name == "PATH" || env.Name == "LD_LIBRARY_PATH" {
			t.Fatalf("default install sets %s", env.Name)
		}
	}
}

func TestAdapterHostToolingIsMountedForANonNVIDIAVendor(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Agent.Adapters = []kubeneuronv1alpha1.AgentAdapterSpec{{
		Vendor: "amd",
		HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{
			BinDirs: []string{"/opt/rocm/bin"},
			LibDirs: []string{"/opt/rocm/lib"},
			Devices: []string{"/dev/kfd", "/dev/dri/renderD128"},
		},
	}}
	ds := agentDaemonSet(installation, &Snapshot{Digest: "abc123"})
	container := ds.Spec.Template.Spec.Containers[0]

	mounts := map[string]corev1.VolumeMount{}
	for _, m := range container.VolumeMounts {
		mounts[m.MountPath] = m
	}
	volumes := map[string]corev1.Volume{}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		volumes[v.Name] = v
	}
	for path, host := range map[string]string{
		"/host/adapters/amd/bin/0": "/opt/rocm/bin",
		"/host/adapters/amd/lib/0": "/opt/rocm/lib",
		"/lib64":                   "/opt/rocm/lib", // no NVIDIA tooling claims the loader
		"/dev/kfd":                 "/dev/kfd",
		"/dev/dri/renderD128":      "/dev/dri/renderD128",
	} {
		mount, ok := mounts[path]
		if !ok {
			t.Fatalf("no mount at %s: %+v", path, container.VolumeMounts)
		}
		volume := volumes[mount.Name]
		if volume.HostPath == nil || volume.HostPath.Path != host {
			t.Fatalf("mount %s comes from %+v, want host path %s", path, volume.VolumeSource, host)
		}
		if strings.HasPrefix(path, "/host/") && !mount.ReadOnly {
			t.Fatalf("tooling directory %s must be read-only", path)
		}
	}
	if got := *volumes[mounts["/dev/kfd"].Name].HostPath.Type; got != corev1.HostPathCharDev {
		t.Fatalf("device volume type = %s, want CharDevice", got)
	}

	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	// Adapter tools come after the system directories: a host binary must
	// never shadow one shipped in the agent image.
	if want := agentSystemPath + ":/host/adapters/amd/bin/0"; env["PATH"] != want {
		t.Fatalf("PATH = %q, want %q", env["PATH"], want)
	}
	if env["LD_LIBRARY_PATH"] != "/host/adapters/amd/lib/0" {
		t.Fatalf("LD_LIBRARY_PATH = %q", env["LD_LIBRARY_PATH"])
	}
	// Adapter tooling alone does not arm the NVIDIA real-driver requirement.
	if containsArg(container.Args, "--require-real-driver") {
		t.Fatalf("adapter tooling must not require a real NVIDIA driver: %v", container.Args)
	}
}

func TestAdapterToolingExtendsNVIDIAToolingWithoutTakingTheLoader(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Agent.HostTooling = &kubeneuronv1alpha1.HostToolingSpec{BinDir: "/usr/bin", LibDirs: []string{"/usr/lib64"}}
	installation.Spec.Agent.Adapters = []kubeneuronv1alpha1.AgentAdapterSpec{{
		Vendor:      "amd",
		HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{BinDirs: []string{"/opt/rocm/bin"}, LibDirs: []string{"/opt/rocm/lib"}},
	}}
	container := agentContainer(t, installation)
	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	if want := hostToolingBinMount + ":" + agentSystemPath + ":/host/adapters/amd/bin/0"; env["PATH"] != want {
		t.Fatalf("PATH = %q, want %q", env["PATH"], want)
	}
	if want := "/host/nvidia/lib/0:/host/adapters/amd/lib/0"; env["LD_LIBRARY_PATH"] != want {
		t.Fatalf("LD_LIBRARY_PATH = %q, want %q", env["LD_LIBRARY_PATH"], want)
	}
	loaders := 0
	for _, m := range container.VolumeMounts {
		if m.MountPath == "/lib64" {
			loaders++
			if m.Name != "nvidia-lib-0" {
				t.Fatalf("/lib64 comes from %s, want the NVIDIA library directory", m.Name)
			}
		}
	}
	if loaders != 1 {
		t.Fatalf("/lib64 mounted %d times, want 1", loaders)
	}
}

func TestAgentAdapterValidation(t *testing.T) {
	ok := []kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd"}, {Vendor: "aws-trainium", Options: map[string]string{"neuron.ls": "x"}}}
	if err := validateAgentAdapters("spec.agent.adapters", ok); err != nil {
		t.Fatalf("valid adapters rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		adapters []kubeneuronv1alpha1.AgentAdapterSpec
		want     string
	}{
		"malformed vendor": {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "AMD"}}, "not a valid vendor id"},
		"empty vendor":     {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: ""}}, "not a valid vendor id"},
		"duplicate":        {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd"}, {Vendor: "amd"}}, "more than once"},
		"bad option key":   {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd", Options: map[string]string{"a=b": "c"}}}, "option key"},
		"relative dir": {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd",
			HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{BinDirs: []string{"opt/rocm"}}}}, "absolute path"},
		"dot-dot dir": {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd",
			HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{LibDirs: []string{"/opt/../etc"}}}}, "absolute path"},
		"device outside /dev": {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd",
			HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{Devices: []string{"/etc/shadow"}}}}, "under /dev/"},
		"kmsg": {[]kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd",
			HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{Devices: []string{"/dev/kmsg"}}}}, "/dev/kmsg"},
		"shared device": {[]kubeneuronv1alpha1.AgentAdapterSpec{
			{Vendor: "amd", HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{Devices: []string{"/dev/dri/card0"}}},
			{Vendor: "intel", HostTooling: &kubeneuronv1alpha1.AdapterHostToolingSpec{Devices: []string{"/dev/dri/card0"}}},
		}, "already mounted"},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateAgentAdapters("spec.agent.adapters", tc.adapters)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	// The compiler rejects an invalid list even if it bypassed admission.
	installation := testKubeNeuron()
	installation.Spec.Agent.Adapters = []kubeneuronv1alpha1.AgentAdapterSpec{{Vendor: "amd"}, {Vendor: "amd"}}
	if err := validateRuntimeSupport(installation); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("validateRuntimeSupport = %v, want the duplicate rejected", err)
	}
}

func TestDefaultAgentAdaptersMergeWithTheInstallation(t *testing.T) {
	defaults, err := ParseDefaultAgentAdapters(`[{"vendor":"amd","options":{"tool":"rocm-smi"}},{"vendor":"intel"}]`)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeAgentAdapters(defaults, []kubeneuronv1alpha1.AgentAdapterSpec{
		{Vendor: "amd", Options: map[string]string{"tool": "amd-smi"}},
		{Vendor: "acme"},
	})
	var vendors []string
	for _, adapter := range merged {
		vendors = append(vendors, adapter.Vendor)
	}
	if !reflect.DeepEqual(vendors, []string{"acme", "amd", "intel"}) {
		t.Fatalf("merged vendors = %v", vendors)
	}
	if merged[1].Options["tool"] != "amd-smi" {
		t.Fatalf("the installation entry must replace the default: %+v", merged[1])
	}
	if got := mergeAgentAdapters(nil, nil); got != nil {
		t.Fatalf("no defaults and no entries = %v, want nil", got)
	}

	for _, bad := range []string{`[{"vendor":"AMD"}]`, `[{"vendor":"amd","bogus":1}]`, `{"vendor":"amd"}`, `[{"vendor":"amd"},{"vendor":"amd"}]`} {
		if _, err := ParseDefaultAgentAdapters(bad); err == nil {
			t.Errorf("ParseDefaultAgentAdapters(%s) succeeded, want an error", bad)
		}
	}
	if got, err := ParseDefaultAgentAdapters("  "); err != nil || got != nil {
		t.Fatalf("empty flag = %v, %v", got, err)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
