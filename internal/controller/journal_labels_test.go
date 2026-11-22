// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// journalPlatform is a cluster whose nodes carry the journal label.
type journalPlatform struct {
	platform.Platform
	mu      sync.Mutex
	nodes   map[string]*platform.JournalNode
	readErr error
	writes  int
}

func (p *journalPlatform) Name() string { return "journal-test" }

func (p *journalPlatform) JournalNodes(_ context.Context, namespace, installation string) ([]platform.JournalNode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readErr != nil {
		return nil, p.readErr
	}
	if namespace != "kube-neuron-system" || installation != "fleet" {
		return nil, errors.New("unexpected installation")
	}
	var out []platform.JournalNode
	for _, n := range p.nodes {
		copied := *n
		out = append(out, copied)
	}
	return out, nil
}

func (p *journalPlatform) SetJournalDir(_ context.Context, node, dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes++
	p.nodes[node].Dir = dir
	if dir == "" {
		p.nodes[node].WaitingSince = time.Time{}
	}
	return nil
}

func (p *journalPlatform) dir(node string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nodes[node].Dir
}

func journalFixture(t *testing.T, cfg *config.AgentJournal, nodes ...platform.JournalNode) (*Controller, *sqlite.Store, *journalPlatform) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p := &journalPlatform{nodes: map[string]*platform.JournalNode{}}
	for i := range nodes {
		n := nodes[i]
		p.nodes[n.Name] = &n
	}
	c := New(st, st, nil, safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2}), nil, p, nil, &recordingNotifier{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.AgentJournal = cfg })
	return c, st, p
}

func journalConfig() *config.AgentJournal {
	return &config.AgentJournal{Enabled: true, Namespace: "kube-neuron-system", Installation: "fleet"}
}

func TestJournalLabelFollowsTheOSImage(t *testing.T) {
	cfg := journalConfig()
	cfg.AgentNodeSelector = map[string]string{"pool": "gpu"}
	gpu := map[string]string{"pool": "gpu"}
	c, st, p := journalFixture(t, cfg,
		platform.JournalNode{Name: "al2023", OSImage: "Amazon Linux 2023.12.20261112", Labels: gpu, Accelerator: true},
		platform.JournalNode{Name: "ubuntu", OSImage: "Ubuntu 22.04.5 LTS", Labels: gpu, Accelerator: true},
		platform.JournalNode{Name: "kind", OSImage: "Debian GNU/Linux 12 (bookworm)", ProviderID: "kind://docker/c/kind", Labels: gpu, Accelerator: true},
		platform.JournalNode{Name: "debian", OSImage: "Debian GNU/Linux 12 (bookworm)", Labels: gpu, Accelerator: true},
		platform.JournalNode{Name: "cpu", OSImage: "Amazon Linux 2023.12.20261112", Labels: gpu},
		platform.JournalNode{Name: "elsewhere", OSImage: "Amazon Linux 2023.12.20261112", Labels: map[string]string{"pool": "cpu"}, Accelerator: true},
		// Another installation's node: its label is left alone.
		platform.JournalNode{Name: "stale", OSImage: "Ubuntu 22.04.5 LTS", Labels: map[string]string{}, Dir: "var"},
		platform.JournalNode{Name: "agent-only", OSImage: "Bottlerocket OS 1.50.0 (aws-k8s-1.34)", Labels: gpu},
	)
	if err := st.UpsertNode(context.Background(), &types.Node{Name: "agent-only", GPUs: []types.GPUInfo{{Index: 0}}}); err != nil {
		t.Fatal(err)
	}
	c.reconcileJournalLabels(context.Background())
	want := map[string]string{
		"al2023": "var", "ubuntu": "var", "kind": "run", "debian": "", "cpu": "", "elsewhere": "", "stale": "var", "agent-only": "var",
	}
	for node, dir := range want {
		if got := p.dir(node); got != dir {
			t.Errorf("%s: label %q, want %q", node, got, dir)
		}
	}
	if got := c.JournalReason("debian"); got != JournalReasonOSImageNotRecognized {
		t.Errorf("debian reason = %q", got)
	}
	if got := c.JournalReason("al2023"); got != "" {
		t.Errorf("al2023 reason = %q", got)
	}
	writes := p.writes
	c.reconcileJournalLabels(context.Background())
	if p.writes != writes {
		t.Errorf("a second pass wrote %d labels", p.writes-writes)
	}
}

func TestJournalOverrideComesFirst(t *testing.T) {
	cfg := journalConfig()
	cfg.NodeOverrides = []config.JournalNodeOverride{
		{NodeSelector: map[string]string{"image": "custom"}, Dir: "run"},
		{NodeSelector: map[string]string{"journal": "off"}, Dir: "none"},
		{NodeSelector: map[string]string{"image": "custom"}, Dir: "var"},
	}
	c, _, p := journalFixture(t, cfg,
		platform.JournalNode{Name: "custom", OSImage: "Amazon Linux 2023", Labels: map[string]string{"image": "custom"}, Accelerator: true},
		platform.JournalNode{Name: "off", OSImage: "Amazon Linux 2023", Labels: map[string]string{"journal": "off"}, Accelerator: true, Dir: "var"},
		platform.JournalNode{Name: "unknown", OSImage: "Some Linux", Labels: map[string]string{"image": "custom"}, Accelerator: true},
	)
	c.reconcileJournalLabels(context.Background())
	if p.dir("custom") != "run" || p.dir("unknown") != "run" || p.dir("off") != "" {
		t.Fatalf("labels custom=%q unknown=%q off=%q", p.dir("custom"), p.dir("unknown"), p.dir("off"))
	}
	if got := c.JournalReason("off"); got != JournalReasonDisabled {
		t.Errorf("off reason = %q", got)
	}
}

func TestDisabledJournalRemovesEveryLabel(t *testing.T) {
	cfg := journalConfig()
	cfg.Enabled = false
	c, _, p := journalFixture(t, cfg,
		platform.JournalNode{Name: "a", OSImage: "Amazon Linux 2023", Accelerator: true, Dir: "var"},
		platform.JournalNode{Name: "b", OSImage: "Ubuntu 24.04", Dir: "run"},
	)
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "" || p.dir("b") != "" {
		t.Fatalf("labels a=%q b=%q", p.dir("a"), p.dir("b"))
	}
	if got := c.JournalReason("a"); got != JournalReasonDisabled {
		t.Errorf("reason = %q", got)
	}
}

func TestJournalLabelLeftAloneWithoutConfigurationOrRead(t *testing.T) {
	c, _, p := journalFixture(t, nil, platform.JournalNode{Name: "a", OSImage: "Ubuntu", Accelerator: true, Dir: "run"})
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "run" || p.writes != 0 {
		t.Fatalf("a configuration without agent_journal changed the label: %q, %d writes", p.dir("a"), p.writes)
	}
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.AgentJournal = journalConfig() })
	p.readErr = errors.New("apiserver down")
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "run" || p.writes != 0 {
		t.Fatalf("a failed read changed the label: %q, %d writes", p.dir("a"), p.writes)
	}
}

func TestJournalMountFailureTakesTheLabelOffForAnHour(t *testing.T) {
	c, _, p := journalFixture(t, journalConfig(),
		platform.JournalNode{Name: "a", OSImage: "Amazon Linux 2023", Accelerator: true, Dir: "var",
			WaitingSince: time.Now().Add(-2 * time.Minute)},
	)
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "var" {
		t.Fatalf("a Pod waiting 2m lost its label")
	}
	p.nodes["a"].WaitingSince = time.Now().Add(-journalMountWait - time.Second)
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "" || c.JournalReason("a") != JournalReasonMountFailed {
		t.Fatalf("after 3m: label %q reason %q", p.dir("a"), c.JournalReason("a"))
	}
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "" {
		t.Fatalf("the label came back within the hour")
	}
	// A new OS image is a new chance.
	p.nodes["a"].OSImage = "Amazon Linux 2023.13"
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "var" || c.JournalReason("a") != "" {
		t.Fatalf("after an OS image change: label %q reason %q", p.dir("a"), c.JournalReason("a"))
	}
	// Fails again, then the hour passes.
	p.nodes["a"].WaitingSince = time.Now().Add(-journalMountWait - time.Second)
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "" {
		t.Fatal("second failure kept the label")
	}
	c.journal.mu.Lock()
	f := c.journal.failed["a"]
	f.at = f.at.Add(-journalMountRetry)
	c.journal.failed["a"] = f
	c.journal.mu.Unlock()
	c.reconcileJournalLabels(context.Background())
	if p.dir("a") != "var" {
		t.Fatalf("after the hour: label %q", p.dir("a"))
	}
}

// health shows why a node's agent has no kernel log: the agent knows only
// that it has none, the controller knows why.
func TestKernelLogSourceCarriesTheJournalReason(t *testing.T) {
	c, _, _ := journalFixture(t, journalConfig(),
		platform.JournalNode{Name: "debian", OSImage: "Debian GNU/Linux 12 (bookworm)", Accelerator: true},
		platform.JournalNode{Name: "al2023", OSImage: "Amazon Linux 2023", Accelerator: true},
	)
	c.reconcileJournalLabels(context.Background())
	now := time.Now()
	for _, node := range []string{"debian", "al2023"} {
		c.RecordAgentSignalSources(node, []types.AgentSignalSource{
			{Node: node, Source: "nvidia-kmsg", Class: types.ClassXIDApp, Reason: "kernel-log-unavailable", ReadAt: now},
			{Node: node, Source: "nvidia-dcgm", Class: types.ClassXIDApp, Reason: "xid-field-unavailable", ReadAt: now},
		})
	}
	got := map[string]string{}
	for _, r := range c.AgentSignalSources("") {
		got[r.Node+"/"+r.Source] = r.Reason
	}
	want := map[string]string{
		"debian/nvidia-kmsg": JournalReasonOSImageNotRecognized,
		"debian/nvidia-dcgm": "xid-field-unavailable",
		"al2023/nvidia-kmsg": "kernel-log-unavailable",
		"al2023/nvidia-dcgm": "xid-field-unavailable",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s reason = %q, want %q", k, got[k], v)
		}
	}
}

// Two installations with their agents on separate node pools: one switching
// the journal off leaves the other's labels alone.
func TestDisabledJournalLeavesOtherInstallationsNodes(t *testing.T) {
	cfg := journalConfig()
	cfg.Enabled = false
	cfg.AgentNodeSelector = map[string]string{"pool": "a"}
	c, _, p := journalFixture(t, cfg,
		platform.JournalNode{Name: "mine", OSImage: "Amazon Linux 2023", Labels: map[string]string{"pool": "a"}, Accelerator: true, Dir: "var"},
		platform.JournalNode{Name: "theirs", OSImage: "Amazon Linux 2023", Labels: map[string]string{"pool": "b"}, Accelerator: true, Dir: "var"},
	)
	c.reconcileJournalLabels(context.Background())
	if p.dir("mine") != "" || p.dir("theirs") != "var" {
		t.Fatalf("labels mine=%q theirs=%q", p.dir("mine"), p.dir("theirs"))
	}
	if got := c.JournalReason("theirs"); got != "" {
		t.Errorf("theirs reason = %q", got)
	}
}
