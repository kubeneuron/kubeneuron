// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// publishDigest makes the fake API hold the operator's runtime ConfigMap of
// installation "kn" with the given digest.
func publishDigest(t *testing.T, kube *fake.Clientset, digest string) {
	t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kn-runtime", Namespace: "kube-neuron"},
		Data: map[string]string{"config-digest": digest}}
	maps := kube.CoreV1().ConfigMaps("kube-neuron")
	if _, err := maps.Update(ctx, cm, metav1.UpdateOptions{}); err == nil {
		return
	}
	if _, err := maps.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// loadDigest installs the same configuration as loaded from digest.
func loadDigest(t *testing.T, c *Controller, digest string) {
	t.Helper()
	rc := *c.runtimeConfig(context.Background())
	rc.SourceDigest = digest
	if err := c.InstallRuntimeConfig(rc); err != nil {
		t.Fatal(err)
	}
}

func heldRows(t *testing.T, c *Controller, id string) []*types.AuditEntry {
	t.Helper()
	trail, err := c.store.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var held []*types.AuditEntry
	for _, e := range trail {
		if e.Action == "recovery-held" {
			held = append(held, e)
		}
	}
	return held
}

// The operator publishes a policy change at once; the controller loads it
// when the kubelet refreshes the mounted copy. In between, the loaded
// revision may be one the operator has already replaced, so no step is
// authorised under it. Once loaded, the step goes on.
func TestAutomaticWaitsForThePublishedConfiguration(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	kube := fake.NewSimpleClientset()
	c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
	loadDigest(t, c, "digest-old")
	publishDigest(t, kube, "digest-new")

	id := openOn(t, c, "n1")
	if inc := step(t, c, st, id, 20); inc.State != types.StateEvaluating {
		t.Fatalf("state=%s, want the step held at EVALUATING while the published configuration is not loaded", inc.State)
	}
	held := heldRows(t, c, id)
	want := "cordon held by policy " + autoPolicy + ": the controller has not yet loaded the published configuration; the step is retried when it is loaded. To check, compare source_digest in /api/v1/runtime-config with config-digest in ConfigMap kn-runtime"
	if len(held) != 1 || held[0].Result != want {
		t.Fatalf("held rows %v, want one %q", held, want)
	}
	if p := held[0].Params; p["loaded_config_digest"] != "digest-old" || p["published_config_digest"] != "digest-new" {
		t.Fatalf("held row params %v, want the loaded and the published digest", p)
	}
	if started := stepStarts(t, c, id); len(started) != 0 {
		t.Fatalf("steps %v started under the loaded revision, want none", started)
	}

	loadDigest(t, c, "digest-new")
	if inc := step(t, c, st, id, 200); inc.State != types.StateResolved {
		t.Fatalf("state=%s after the published configuration was loaded, want RESOLVED", inc.State)
	}
	trail, err := c.store.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	authorised := 0
	for _, e := range trail {
		if e.Action == policyAuthorisedAction {
			authorised++
			if e.Params["config_digest"] != "digest-new" {
				t.Fatalf("policy-authorised params %v, want config_digest digest-new", e.Params)
			}
		}
	}
	if authorised == 0 {
		t.Fatal("no policy-authorised row after the published configuration was loaded")
	}
}

// The decision is taken under the published configuration, not under the
// one being replaced: a revision that turns Automatic off parks the step for
// a person, and nothing is authorised by the policy the operator replaced.
func TestAutomaticTurnedOffInTheWindowParksAfterTheReload(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	kube := fake.NewSimpleClientset()
	c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
	loadDigest(t, c, "digest-old")
	publishDigest(t, kube, "digest-new")

	id := openOn(t, c, "n1")
	step(t, c, st, id, 20)
	if started := stepStarts(t, c, id); len(started) != 0 {
		t.Fatalf("steps %v started while Automatic was being turned off, want none", started)
	}

	rc := *c.runtimeConfig(context.Background())
	rc.Engine, rc.SourceDigest = modeEngine(t, recoverymode.Approval, autoSettings()), "digest-new"
	if err := c.InstallRuntimeConfig(rc); err != nil {
		t.Fatal(err)
	}
	if inc := step(t, c, st, id, 200); inc.State != types.StateAwaitingApproval {
		t.Fatalf("state=%s, want AWAITING_APPROVAL under the published revision", inc.State)
	}
	if actors, _ := auditActors(t, st, id); strings.Contains(strings.Join(actors, ","), "policy:") {
		t.Fatalf("actors %v: the replaced policy authorised a step", actors)
	}
}

// A person's approval is not lost while the configuration loads: the step
// waits, the approval stays recorded, and the step runs once it is loaded.
func TestApprovedStepWaitsForThePublishedConfiguration(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Approval, autoSettings(), 4)
	kube := fake.NewSimpleClientset()
	c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
	ctx := context.Background()
	loadDigest(t, c, "digest-a")
	publishDigest(t, kube, "digest-a")

	id := openOn(t, c, "n1")
	inc := step(t, c, st, id, 200)
	if inc.State != types.StateAwaitingApproval {
		t.Fatalf("state=%s, want the drain parked for approval", inc.State)
	}
	publishDigest(t, kube, "digest-b")
	if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, inc.ApprovalEpoch, "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		c.reconcile(ctx)
	}
	if inc, _ = st.GetIncident(ctx, id); inc.State != types.StateAwaitingApproval {
		t.Fatalf("state=%s, want the approved drain held while the published configuration is not loaded", inc.State)
	}
	held := heldRows(t, c, id)
	want := "drain held: the controller has not yet loaded the published configuration; the step is retried when it is loaded. To check, compare source_digest in /api/v1/runtime-config with config-digest in ConfigMap kn-runtime"
	if len(held) != 1 || held[0].Result != want {
		t.Fatalf("held rows %v, want one %q", held, want)
	}
	if decision, err := st.FirstApprovalDecision(ctx, id, inc.ApprovalEpoch); err != nil || decision.Actor != "alice" {
		t.Fatalf("approval %v (%v), want alice's approval still recorded", decision, err)
	}

	loadDigest(t, c, "digest-b")
	for i := 0; i < 20; i++ {
		c.reconcile(ctx)
		time.Sleep(2 * time.Millisecond)
	}
	if started := stepStarts(t, c, id); !containsString(started, "alice:drain") {
		t.Fatalf("steps %v, want the drain started under alice's approval once the configuration was loaded", started)
	}
}

// A notification acts on no node and is not held.
func TestNotificationStepIsNotHeldForThePublishedConfiguration(t *testing.T) {
	c, _, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	kube := fake.NewSimpleClientset()
	c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
	loadDigest(t, c, "digest-old")
	publishDigest(t, kube, "digest-new")
	inc := &types.Incident{ID: "notify-only", Class: types.ClassFellOffBus, Target: types.Target{Node: "n1"}}
	if c.holdUnloadedConfig(context.Background(), inc, &playbook.Step{Name: "page", Action: "notify.webhook"}) {
		t.Fatal("a notification step was held")
	}
	if !c.holdUnloadedConfig(context.Background(), inc, &playbook.Step{Name: "cordon", Action: "platform.cordon"}) {
		t.Fatal("a cordon was not held")
	}
}

// stepStarts lists actor:step for every step that entered EXECUTING.
func stepStarts(t *testing.T, c *Controller, id string) []string {
	t.Helper()
	trail, err := c.store.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, e := range trail {
		if strings.HasPrefix(e.Result, "step-start: ") {
			started = append(started, e.Actor+":"+e.Action)
		}
	}
	return started
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A published digest that cannot be read is not taken as loaded.
func TestAutomaticHoldsWhenThePublishedConfigurationCannotBeRead(t *testing.T) {
	for name, tc := range map[string]struct {
		react  func(*fake.Clientset)
		reason string
	}{
		"forbidden": {func(kube *fake.Clientset) {
			kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("configmaps \"kn-runtime\" is forbidden")
			})
		}, `the published configuration cannot be read: configmaps "kn-runtime" is forbidden`},
		"missing":   {func(*fake.Clientset) {}, "the published configuration cannot be read: "},
		"no digest": {func(kube *fake.Clientset) { publishDigest(t, kube, "") }, "the published configuration has no digest"},
	} {
		t.Run(name, func(t *testing.T) {
			c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
			kube := fake.NewSimpleClientset()
			tc.react(kube)
			c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
			loadDigest(t, c, "digest-old")
			id := openOn(t, c, "n1")
			if inc := step(t, c, st, id, 20); inc.State != types.StateEvaluating {
				t.Fatalf("state=%s, want the step held", inc.State)
			}
			held := heldRows(t, c, id)
			if len(held) == 0 || !strings.Contains(held[0].Result, ": "+tc.reason) ||
				!strings.HasSuffix(held[0].Result, "; the step is retried when it is loaded. To check, compare source_digest in /api/v1/runtime-config with config-digest in ConfigMap kn-runtime") {
				t.Fatalf("held rows %v, want %q in the unloaded-configuration frame", held, tc.reason)
			}
		})
	}
}

// Without an operator the configuration comes from files and nothing is
// published: automatic steps run as before, whatever digest is loaded.
func TestAutomaticWithoutAnOperatorDoesNotWait(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	loadDigest(t, c, "")
	if inc := step(t, c, st, openOn(t, c, "n1"), 200); inc.State != types.StateResolved {
		t.Fatalf("state=%s, want RESOLVED without a published configuration to wait for", inc.State)
	}
}

// The publication time is the ConfigMap's last write.
func TestPublishedConfigReportsTheLastWrite(t *testing.T) {
	c, _, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 1)
	if _, _, configured, err := c.PublishedConfig(context.Background()); configured || err != nil {
		t.Fatalf("configured=%v err=%v without an operator, want neither", configured, err)
	}
	created := metav1.NewTime(time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC))
	written := metav1.NewTime(created.Add(time.Hour))
	kube := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kn-runtime", Namespace: "kube-neuron", CreationTimestamp: created,
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubeneuron-operator", Time: &written}}},
		Data: map[string]string{"config-digest": "digest-new"},
	})
	c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
	digest, at, configured, err := c.PublishedConfig(context.Background())
	if err != nil || !configured || digest != "digest-new" || !at.Equal(written.Time) {
		t.Fatalf("PublishedConfig = %q %v %v %v, want digest-new written at %v", digest, at, configured, err, written.Time)
	}
}

// engineWithDrain is modeEngine with the drain step under another name, or
// without the playbook the incident runs.
func engineWithDrain(t *testing.T, mode recoverymode.Mode, drain string, book string) *playbook.Engine {
	t.Helper()
	pb := &playbook.Playbook{Name: book, Target: "node", Steps: []playbook.Step{
		{Name: "cordon", Action: "platform.cordon"},
		{Name: drain, Action: "platform.drain", Approval: "required"},
		{Name: "replace", Action: "platform.replace_node", Approval: "required"},
	}}
	engine, err := playbook.NewEngine(map[string]*playbook.Playbook{book: pb}, []playbook.Policy{{
		Name: autoPolicy, Class: types.ClassFellOffBus, Playbook: book, Mode: mode, Automatic: autoSettings(),
		ReplacementStrategy: recoverymode.RemoveFirst,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// An approval is decided under the configuration the operator published.
// While a reloaded but already replaced configuration no longer matches the
// approved step, the round is neither re-parked nor quarantined: the approval
// stays recorded, and the published configuration decides once it is loaded.
func TestApprovalIsDecidedUnderThePublishedConfiguration(t *testing.T) {
	for name, tc := range map[string]struct {
		engine func(*testing.T) *playbook.Engine
		after  types.IncidentState
	}{
		"step renamed": {func(t *testing.T) *playbook.Engine {
			return engineWithDrain(t, recoverymode.Approval, "drain-pods", "cloud-replace")
		}, types.StateAwaitingApproval},
		"playbook gone": {func(t *testing.T) *playbook.Engine {
			return engineWithDrain(t, recoverymode.Approval, "drain", "other")
		}, types.StateNeedsHuman},
	} {
		t.Run(name, func(t *testing.T) {
			c, st, _ := autoFixture(t, recoverymode.Approval, autoSettings(), 4)
			kube := fake.NewSimpleClientset()
			c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
			ctx := context.Background()
			loadDigest(t, c, "digest-a")
			publishDigest(t, kube, "digest-a")
			id := openOn(t, c, "n1")
			inc := step(t, c, st, id, 200)
			if inc.State != types.StateAwaitingApproval {
				t.Fatalf("state=%s, want the drain parked for approval", inc.State)
			}
			epoch := inc.ApprovalEpoch
			if err := c.DecideApproval(ctx, id, "alice", "cli", types.ApprovalApproved, epoch, "", ""); err != nil {
				t.Fatal(err)
			}
			// The controller has loaded a configuration the operator has
			// already replaced.
			rc := *c.runtimeConfig(ctx)
			rc.Engine, rc.SourceDigest = tc.engine(t), "digest-b"
			if err := c.InstallRuntimeConfig(rc); err != nil {
				t.Fatal(err)
			}
			publishDigest(t, kube, "digest-c")
			for i := 0; i < 20; i++ {
				c.reconcile(ctx)
			}
			if inc, _ = st.GetIncident(ctx, id); inc.State != types.StateAwaitingApproval || inc.ApprovalEpoch != epoch {
				t.Fatalf("state=%s round %d, want round %d still awaiting its approved step", inc.State, inc.ApprovalEpoch, epoch)
			}
			if decision, err := st.FirstApprovalDecision(ctx, id, epoch); err != nil || decision.Actor != "alice" {
				t.Fatalf("approval %v (%v), want alice's approval still recorded", decision, err)
			}
			if held := heldRows(t, c, id); len(held) == 0 || !strings.Contains(held[len(held)-1].Result, "the controller has not yet loaded the published configuration") {
				t.Fatalf("held rows %v, want the approved step held", held)
			}

			loadDigest(t, c, "digest-c")
			for i := 0; i < 20; i++ {
				c.reconcile(ctx)
			}
			if inc, _ = st.GetIncident(ctx, id); inc.State != tc.after {
				t.Fatalf("state=%s after the published configuration was loaded, want %s", inc.State, tc.after)
			}
			if tc.after == types.StateAwaitingApproval && inc.ApprovalEpoch == epoch {
				t.Fatalf("round %d unchanged, want the renamed step asked about in a new round", epoch)
			}
		})
	}
}

// cordonCounter fails the test on any cordon: nothing may run for real.
type cordonCounter struct {
	*autoCloudPlatform
	t *testing.T
}

func (p cordonCounter) Cordon(context.Context, string, string) error {
	p.t.Error("a cordon ran for real")
	return nil
}

// A switch to DryRun applies to the first step that starts after it: while
// the controller still runs the Enabled configuration, the step waits, and
// once DryRun is loaded it is simulated.
func TestSwitchToDryRunAppliesToTheNextStep(t *testing.T) {
	c, st, _ := autoFixture(t, recoverymode.Automatic, autoSettings(), 4)
	c.platform = cordonCounter{&autoCloudPlatform{}, t}
	kube := fake.NewSimpleClientset()
	c.ConfigurePublishedConfig(kube.CoreV1().ConfigMaps("kube-neuron"), "kn")
	ctx := context.Background()
	rc := *c.runtimeConfig(ctx)
	enabled := *rc.SafetyLimits
	enabled.DryRun = false
	rc.SafetyLimits, rc.SourceDigest = &enabled, "digest-enabled"
	if err := c.InstallRuntimeConfig(rc); err != nil {
		t.Fatal(err)
	}
	publishDigest(t, kube, "digest-dryrun")

	id := openOn(t, c, "n1")
	step(t, c, st, id, 20)
	if started := stepStarts(t, c, id); len(started) != 0 {
		t.Fatalf("steps %v started under the Enabled configuration being replaced, want none", started)
	}

	dry := enabled
	dry.DryRun = true
	rc = *c.runtimeConfig(ctx)
	rc.SafetyLimits, rc.SourceDigest = &dry, "digest-dryrun"
	if err := c.InstallRuntimeConfig(rc); err != nil {
		t.Fatal(err)
	}
	step(t, c, st, id, 20)
	_, results := auditActors(t, st, id)
	if !containsString(results, "step-outcome: simulating platform.cordon") {
		t.Fatalf("audit %q, want the cordon simulated under the loaded DryRun", results)
	}
}
