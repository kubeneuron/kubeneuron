// Command hwprobe exercises the real nvidia-smi driver paths on a GPU host
// and prints the observation as JSON. It is a hardware-qualification tool:
// it runs exactly the parsing and preflight code the agent uses (DetectSMI,
// inventory, driver version, MIG mode, accelerator preflight) with zero side
// effects — no reset, no controller, no writes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/accelerator/nvidia"
	"github.com/kubeneuron/kubeneuron/internal/agent/dcgm"
	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
)

func main() {
	smiPath := flag.String("nvidia-smi", "", "nvidia-smi path (default: from PATH)")
	dcgmiPath := flag.String("dcgmi", "", "dcgmi path (default: from PATH)")
	flag.Parse()

	out := map[string]any{"probed_at": time.Now().UTC()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	detected := nvml.DetectSMI(*smiPath)
	out["detect_smi"] = detected
	if !detected {
		out["error"] = "nvidia-smi not detected"
		emit(out)
		os.Exit(1)
	}

	driver := nvml.NewSMI(*smiPath)
	if err := driver.Init(); err != nil {
		out["init_error"] = err.Error()
		emit(out)
		os.Exit(1)
	}
	defer func() { _ = driver.Shutdown() }()

	gpus, err := driver.ListGPUs(ctx)
	if err != nil {
		out["inventory_error"] = err.Error()
	} else {
		out["inventory"] = gpus
	}

	hostname, _ := os.Hostname()
	adapter, err := nvidia.New(nvidia.Config{NodeName: hostname}, driver)
	if err != nil {
		out["adapter_error"] = err.Error()
		emit(out)
		os.Exit(1)
	}
	report := adapter.Preflight(ctx)
	out["preflight"] = report

	if version, err := dcgm.New(*dcgmiPath).Version(ctx); err != nil {
		out["dcgm_version_error"] = err.Error()
	} else {
		out["dcgm_version"] = version
	}

	emit(out)
}

func emit(v map[string]any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
