// Command paneldemo runs a real controller in-process (the same wiring as
// test/e2e) with a simulated fleet: four nodes register like agents and a
// scenario driver emits XIDs over time. It exists for developing the
// embedded panel and the Grafana dashboard against live, moving data:
//
//	go run ./test/paneldemo .
//	open http://127.0.0.1:18080  (token: demo-operator-token)
//	metrics for Grafana/Prometheus at http://127.0.0.1:18080/metrics
//
// Everything is dry-run against a temp SQLite store; both listeners bind
// 127.0.0.1 only.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/kubeneuron/kubeneuron/web"
)

type staticAuth struct{ node string }

func (s staticAuth) AuthenticateAgent(r *http.Request) (httpapi.AgentPrincipal, error) {
	node := r.Header.Get("X-Demo-Node")
	if node == "" {
		node = s.node
	}
	return httpapi.AgentPrincipal{NodeName: node}, nil
}

const token = "demo-operator-token"

func main() {
	repo := os.Args[1]
	dir, _ := os.MkdirTemp("", "paneldemo")
	st, err := sqlite.Open(filepath.Join(dir, "demo.db"))
	if err != nil {
		panic(err)
	}
	cfg, err := config.Load(filepath.Join(repo, "configs/policies.yaml"))
	if err != nil {
		panic(err)
	}
	books, err := playbook.LoadDir(filepath.Join(repo, "configs/playbooks"))
	if err != nil {
		panic(err)
	}
	var policies []playbook.Policy
	for _, p := range cfg.Policies {
		policies = append(policies, playbook.Policy{Class: p.Match.Class, Playbook: p.Playbook, Params: p.Params})
	}
	engine, err := playbook.NewEngine(books, policies)
	if err != nil {
		panic(err)
	}
	gate := safety.NewGate(safety.Limits{
		MaxConcurrentRemediations: cfg.Safety.MaxConcurrentRemediations,
		MaxConcurrentReboots:      cfg.Safety.MaxConcurrentReboots,
		DryRun:                    true,
	})
	flap := safety.NewFlapDetector(cfg.Safety.Flap.Count, cfg.Safety.Flap.Window.Std())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctrl := controller.New(st, st, engine, gate, flap, nil, nil, &notify.Log{Logger: log}, log)
	ctrl.SetTimings(10*time.Minute, time.Hour)
	ctrl.SetReconcileInterval(300 * time.Millisecond)

	api := httpapi.New(ctrl)
	api.EnableOperatorAPI(ctrl, token)
	// Password sign-in for the login page: demo / kubeneuron.
	usersDir := filepath.Join(dir, "users")
	_ = os.MkdirAll(usersDir, 0o700)
	hash, err := bcrypt.GenerateFromPassword([]byte("kubeneuron"), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(usersDir, "demo"), hash, 0o600); err != nil {
		panic(err)
	}
	api.SetBasicUsersDir(usersDir)
	metrics.RegisterIncidentStates(func() map[types.IncidentState]int {
		counts, err := st.CountIncidentsByState(context.Background())
		if err != nil {
			return nil
		}
		if pending, err := st.CountPendingActions(context.Background()); err == nil {
			metrics.ActionsPending.Set(float64(pending))
		}
		return counts
	})
	api.SetMetricsHandler(metrics.Handler())
	if dist, err := web.Dist(); err == nil {
		api.SetUI(http.FS(dist))
	} else {
		panic(err)
	}

	ctx := context.Background()
	go func() { _ = ctrl.Run(ctx) }()
	go func() { _ = http.ListenAndServe("127.0.0.1:18090", api.AgentRoutes(staticAuth{})) }()
	go func() { _ = http.ListenAndServe("127.0.0.1:18080", api.Routes()) }()
	time.Sleep(300 * time.Millisecond)

	// A small believable fleet.
	nodes := []struct {
		name string
		gpus int
	}{{"gpu-a100-prod-01", 8}, {"gpu-a100-prod-02", 8}, {"gpu-h100-train-01", 8}, {"gpu-t4-infer-01", 4}}
	for _, n := range nodes {
		registerNode(n.name, n.gpus)
	}
	// Background heartbeats keep agent_last_seen fresh.
	go func() {
		for {
			for _, n := range nodes {
				registerNode(n.name, n.gpus)
			}
			time.Sleep(20 * time.Second)
		}
	}()
	fmt.Println("READY http://127.0.0.1:18080")

	// Scenario driver: emit XIDs over time so metrics and the incident
	// table have texture. XID 63 (row-remap, observe-only) + 79 + 48.
	scenario := []struct {
		node  string
		gpu   int
		xid   int
		sleep time.Duration
	}{
		{"gpu-t4-infer-01", 1, 63, 2 * time.Second},
		{"gpu-a100-prod-02", 3, 63, 15 * time.Second},
		{"gpu-a100-prod-01", 5, 48, 20 * time.Second},
		{"gpu-h100-train-01", 2, 79, 25 * time.Second},
		{"gpu-a100-prod-02", 3, 63, 30 * time.Second},
	}
	go func() {
		for _, s := range scenario {
			time.Sleep(s.sleep)
			emitXID(s.node, s.gpu, s.xid)
		}
		// Keep a gentle drip of observe-only signals for metric movement.
		for i := 0; ; i++ {
			time.Sleep(45 * time.Second)
			emitXID("gpu-a100-prod-02", 3, 63)
		}
	}()
	select {}
}

func registerNode(name string, gpus int) {
	reg := map[string]any{"name": name, "gpus": []map[string]any{}}
	list := make([]map[string]any, 0, gpus)
	for i := 0; i < gpus; i++ {
		list = append(list, map[string]any{
			"index": i, "uuid": fmt.Sprintf("GPU-%s-%04d", name[len(name)-2:], i), "model": modelFor(name),
		})
	}
	reg["gpus"] = list
	post("http://127.0.0.1:18090/api/v1/agents/register/narrow-v1", name, reg)
}

func modelFor(name string) string {
	switch {
	case bytes.Contains([]byte(name), []byte("a100")):
		return "NVIDIA A100-SXM4-80GB"
	case bytes.Contains([]byte(name), []byte("h100")):
		return "NVIDIA H100 80GB HBM3"
	default:
		return "Tesla T4"
	}
}

func emitXID(node string, gpu, xid int) {
	post("http://127.0.0.1:18090/api/v1/events", node, map[string]any{
		"event_id": fmt.Sprintf("ev-%d", time.Now().UnixNano()),
		"node":     node, "gpu_index": gpu,
		"gpu_uuid":  fmt.Sprintf("GPU-%s-%04d", node[len(node)-2:], gpu),
		"xid":       xid,
		"timestamp": time.Now().Format(time.RFC3339Nano),
		"raw":       fmt.Sprintf("NVRM: Xid (PCI:0000:%02x:00): %d, demo", gpu, xid),
	})
}

func post(url, node string, v any) {
	body, _ := json.Marshal(v)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Demo-Node", node)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("post", url, err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
