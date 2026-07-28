// Command simulator is a fake GPU node: it registers with the controller
// like a real kubeneuron-agent and emits synthetic XID events on an interval,
// cycling through the failure catalog. It makes the whole detection ->
// incident -> playbook -> (dry-run) remediation path exercisable without
// GPUs — this is the backbone of local development and e2e tests.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// scenarios are the XID codes the simulator cycles through, weighted toward
// the interesting hardware failures.
var scenarios = []int{79, 48, 95, 63, 64, 74, 94, 119, 13, 31}

func main() {
	var (
		controllerURL = flag.String("controller", "http://localhost:8080", "controller base URL")
		nodeName      = flag.String("node", "sim-node-01", "simulated node name")
		gpus          = flag.Int("gpus", 8, "simulated GPU count")
		interval      = flag.Duration("interval", 30*time.Second, "time between synthetic XID events")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	inventory := make([]types.GPUInfo, *gpus)
	for i := range inventory {
		inventory[i] = types.GPUInfo{
			Index: i,
			UUID:  fmt.Sprintf("GPU-simulated-%s-%02d", *nodeName, i),
			Model: "Simulated H100",
		}
	}

	register := func() {
		url := *controllerURL + types.AgentRegistrationPath
		if err := requireRegistrationCapability(ctx, client, url); err != nil {
			log.Warn("registration capability check failed", "err", err)
			return
		}
		registration := types.AgentRegistration{
			Name:   *nodeName,
			GPUs:   inventory,
			BootID: "simulated-boot-id",
		}
		if err := postExpectStatus(ctx, client, url, registration, http.StatusNoContent); err != nil {
			log.Warn("registration failed", "err", err)
		}
	}
	register()

	log.Info("simulator running", "node", *nodeName, "gpus", *gpus, "interval", *interval)
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			xid := scenarios[i%len(scenarios)]
			gpu := inventory[rand.Intn(len(inventory))]
			ev := types.AgentEvent{
				Node:      *nodeName,
				GPUIndex:  gpu.Index,
				GPUUUID:   gpu.UUID,
				XID:       xid,
				Raw:       fmt.Sprintf("NVRM: Xid (PCI:0000:%02x:00): %d, simulated event", gpu.Index+0x10, xid),
				Timestamp: time.Now(),
			}
			if err := post(ctx, client, *controllerURL+"/api/v1/events", ev); err != nil {
				log.Warn("event push failed", "xid", xid, "err", err)
				continue
			}
			log.Info("emitted synthetic XID", "xid", xid, "gpu", gpu.Index)
			register() // heartbeat
		}
	}
}

func post(ctx context.Context, client *http.Client, url string, v any) error {
	return postExpectStatus(ctx, client, url, v, 0)
}

func postExpectStatus(ctx context.Context, client *http.Client, url string, v any, expected int) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if expected != 0 && resp.StatusCode != expected {
		return fmt.Errorf("POST %s: expected %d, got %s", url, expected, resp.Status)
	}
	if expected == 0 && resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s", url, resp.Status)
	}
	return nil
}

func requireRegistrationCapability(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: expected %d, got %s", url, http.StatusOK, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 129))
	if err != nil {
		return err
	}
	if got, want := string(body), types.AgentRegistrationProtocol+"\n"; got != want {
		return fmt.Errorf("GET %s: incompatible registration protocol capability %q", url, got)
	}
	return nil
}
