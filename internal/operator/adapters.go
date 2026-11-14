// SPDX-License-Identifier: LicenseRef-KubeNeuron

package operator

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// agentSystemPath is the PATH the agent image runs with. Adapter tool
// directories are appended after it, so a host tool can never shadow a binary
// the image ships (nsenter, dcgmi).
const agentSystemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Bounds mirrored from the CRD schema, so adapters that arrive through the
// operator's --default-agent-adapters flag are held to the same rules.
const (
	maxAgentAdapters      = 8
	maxAdapterOptions     = 32
	maxAdapterOptionValue = 1024
	maxAdapterDirs        = 4
	maxAdapterDevices     = 16
)

var adapterOptionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ParseDefaultAgentAdapters decodes the operator's --default-agent-adapters
// value: a JSON list of spec.agent.adapters entries. Empty means none.
func ParseDefaultAgentAdapters(value string) ([]kubeneuronv1alpha1.AgentAdapterSpec, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var adapters []kubeneuronv1alpha1.AgentAdapterSpec
	dec := json.NewDecoder(strings.NewReader(value))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&adapters); err != nil {
		return nil, fmt.Errorf("default agent adapters: %w", err)
	}
	if err := validateAgentAdapters("default agent adapters", adapters); err != nil {
		return nil, err
	}
	return adapters, nil
}

// mergeAgentAdapters returns the defaults plus the installation's own
// entries; an installation entry replaces the default for the same vendor.
func mergeAgentAdapters(defaults, own []kubeneuronv1alpha1.AgentAdapterSpec) []kubeneuronv1alpha1.AgentAdapterSpec {
	if len(defaults) == 0 {
		return own
	}
	byVendor := map[string]kubeneuronv1alpha1.AgentAdapterSpec{}
	for _, adapter := range defaults {
		byVendor[adapter.Vendor] = *adapter.DeepCopy()
	}
	for _, adapter := range own {
		byVendor[adapter.Vendor] = *adapter.DeepCopy()
	}
	merged := make([]kubeneuronv1alpha1.AgentAdapterSpec, 0, len(byVendor))
	for _, adapter := range byVendor {
		merged = append(merged, adapter)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Vendor < merged[j].Vendor })
	return merged
}

// validateAgentAdapters applies the CRD rules for spec.agent.adapters in Go:
// the API server enforces them for the custom resource, and this enforces
// them for the operator's defaults and any object that bypassed admission.
func validateAgentAdapters(field string, adapters []kubeneuronv1alpha1.AgentAdapterSpec) error {
	if len(adapters) > maxAgentAdapters {
		return fmt.Errorf("%s: at most %d adapters", field, maxAgentAdapters)
	}
	seen := map[string]bool{}
	devices := map[string]string{}
	for _, adapter := range adapters {
		if !types.AcceleratorVendor(adapter.Vendor).Valid() {
			return fmt.Errorf("%s: vendor %q is not a valid vendor id (lowercase letters, digits, and hyphens, at most %d characters)",
				field, adapter.Vendor, types.MaxAcceleratorVendorLength)
		}
		if seen[adapter.Vendor] {
			return fmt.Errorf("%s: vendor %q is listed more than once", field, adapter.Vendor)
		}
		seen[adapter.Vendor] = true
		if len(adapter.Options) > maxAdapterOptions {
			return fmt.Errorf("%s[%s]: at most %d options", field, adapter.Vendor, maxAdapterOptions)
		}
		for key, value := range adapter.Options {
			if !adapterOptionKeyPattern.MatchString(key) {
				return fmt.Errorf("%s[%s]: option key %q must be 1-63 letters, digits, '.', '_', or '-'", field, adapter.Vendor, key)
			}
			if len(value) > maxAdapterOptionValue {
				return fmt.Errorf("%s[%s]: option %q value exceeds %d characters", field, adapter.Vendor, key, maxAdapterOptionValue)
			}
		}
		tooling := adapter.HostTooling
		if tooling == nil {
			continue
		}
		if len(tooling.BinDirs) > maxAdapterDirs || len(tooling.LibDirs) > maxAdapterDirs {
			return fmt.Errorf("%s[%s]: at most %d binDirs and %d libDirs", field, adapter.Vendor, maxAdapterDirs, maxAdapterDirs)
		}
		if len(tooling.Devices) > maxAdapterDevices {
			return fmt.Errorf("%s[%s]: at most %d devices", field, adapter.Vendor, maxAdapterDevices)
		}
		for _, dir := range append(append([]string(nil), tooling.BinDirs...), tooling.LibDirs...) {
			if !safeHostPath(dir) {
				return fmt.Errorf("%s[%s]: host directory %q must be an absolute path without ':' or '..'", field, adapter.Vendor, dir)
			}
		}
		for _, device := range tooling.Devices {
			if !safeHostPath(device) || !strings.HasPrefix(device, "/dev/") || filepath.Clean(device) == "/dev/kmsg" {
				return fmt.Errorf("%s[%s]: device %q must be a path under /dev/ without ':' or '..', other than /dev/kmsg", field, adapter.Vendor, device)
			}
			clean := filepath.Clean(device)
			if other, dup := devices[clean]; dup {
				return fmt.Errorf("%s[%s]: device %q is already mounted for %s", field, adapter.Vendor, device, other)
			}
			devices[clean] = adapter.Vendor
		}
	}
	return nil
}

func safeHostPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.Contains(path, ":") && !strings.Contains(path, "..")
}

// agentAdapterArgs renders one --adapter per entry and one --adapter-option
// per option, in vendor and then key order, so the DaemonSet spec is stable.
func agentAdapterArgs(adapters []kubeneuronv1alpha1.AgentAdapterSpec) []string {
	sorted := append([]kubeneuronv1alpha1.AgentAdapterSpec(nil), adapters...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Vendor < sorted[j].Vendor })
	var args []string
	for _, adapter := range sorted {
		args = append(args, "--adapter="+adapter.Vendor)
		keys := make([]string, 0, len(adapter.Options))
		for key := range adapter.Options {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			args = append(args, "--adapter-option="+adapter.Vendor+"."+key+"="+adapter.Options[key])
		}
	}
	return args
}

// agentAdapterToolingWiring mounts every adapter's host tooling. Directories
// go to fixed container paths under /host/adapters/<vendor>/; device nodes are
// mounted at their own path. It returns the container directories to append
// to PATH and LD_LIBRARY_PATH.
//
// loader is true when no NVIDIA host tooling claims /lib64: the first adapter
// library directory then also provides the ELF loader, for the same reason
// agentHostToolingWiring mounts one (the agent image has none).
func agentAdapterToolingWiring(adapters []kubeneuronv1alpha1.AgentAdapterSpec, loader bool) (mounts []corev1.VolumeMount, volumes []corev1.Volume, binPaths, libPaths []string) {
	sorted := append([]kubeneuronv1alpha1.AgentAdapterSpec(nil), adapters...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Vendor < sorted[j].Vendor })
	for _, adapter := range sorted {
		tooling := adapter.HostTooling
		if tooling == nil {
			continue
		}
		base := "/host/adapters/" + adapter.Vendor
		for i, dir := range tooling.BinDirs {
			name := fmt.Sprintf("adapter-%s-bin-%d", adapter.Vendor, i)
			containerPath := fmt.Sprintf("%s/bin/%d", base, i)
			mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: containerPath, ReadOnly: true})
			volumes = append(volumes, hostPathVolume(name, dir, corev1.HostPathDirectory))
			binPaths = append(binPaths, containerPath)
		}
		for i, dir := range tooling.LibDirs {
			name := fmt.Sprintf("adapter-%s-lib-%d", adapter.Vendor, i)
			containerPath := fmt.Sprintf("%s/lib/%d", base, i)
			mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: containerPath, ReadOnly: true})
			volumes = append(volumes, hostPathVolume(name, dir, corev1.HostPathDirectory))
			libPaths = append(libPaths, containerPath)
			if loader {
				mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: "/lib64", ReadOnly: true})
				loader = false
			}
		}
		for i, device := range tooling.Devices {
			name := fmt.Sprintf("adapter-%s-dev-%d", adapter.Vendor, i)
			mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: filepath.Clean(device)})
			volumes = append(volumes, hostPathVolume(name, device, corev1.HostPathCharDev))
		}
	}
	return mounts, volumes, binPaths, libPaths
}

func hostPathVolume(name, path string, kind corev1.HostPathType) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: path, Type: hostPathType(kind)},
	}}
}

// agentToolingWiring combines the NVIDIA host tooling and every adapter's
// tooling into one set of agent args, env, mounts, and volumes. With neither
// configured it returns nothing, so an existing NVIDIA install renders the
// same DaemonSet as before.
func agentToolingWiring(agent kubeneuronv1alpha1.AgentSpec) (args []string, env []corev1.EnvVar, mounts []corev1.VolumeMount, volumes []corev1.Volume) {
	args, env, mounts, volumes = agentHostToolingWiring(agent.HostTooling)
	adapterMounts, adapterVolumes, binPaths, libPaths := agentAdapterToolingWiring(agent.Adapters, agent.HostTooling == nil)
	mounts = append(mounts, adapterMounts...)
	volumes = append(volumes, adapterVolumes...)
	if len(binPaths) > 0 {
		env = extendEnvPath(env, "PATH", agentSystemPath, binPaths)
	}
	if len(libPaths) > 0 {
		env = extendEnvPath(env, "LD_LIBRARY_PATH", "", libPaths)
	}
	return args, env, mounts, volumes
}

// extendEnvPath appends dirs to the colon-separated variable name, creating
// it from base when it is not set yet.
func extendEnvPath(env []corev1.EnvVar, name, base string, dirs []string) []corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			env[i].Value = strings.Join(append([]string{env[i].Value}, dirs...), ":")
			return env
		}
	}
	parts := dirs
	if base != "" {
		parts = append([]string{base}, dirs...)
	}
	return append(env, corev1.EnvVar{Name: name, Value: strings.Join(parts, ":")})
}
