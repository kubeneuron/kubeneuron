// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func automaticSpec() *kubeneuronv1alpha1.AutomaticRecoverySpec {
	return &kubeneuronv1alpha1.AutomaticRecoverySpec{
		NodeSelector:        map[string]string{"pool": "train"},
		PoolLabel:           "pool",
		MaxConcurrentNodes:  2,
		NodeCooldown:        "6h",
		MaxUnhealthyPercent: 25,
		EnabledBy:           "alice@example.com",
		EnabledAt:           metav1.NewTime(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)),
		Acknowledgement:     recoverymode.Acknowledgement,
	}
}

func compileOne(t *testing.T, mutate func(*kubeneuronv1alpha1.GPURemediationPolicy)) (recoverymode.Mode, *recoverymode.Settings, error) {
	t.Helper()
	policies, _ := testPolicyFixture()
	mutate(&policies[0])
	books := map[string]*playbook.Playbook{"gpu-reset": {Name: "gpu-reset"}}
	compiled, err := compilePolicies(testKubeNeuron(), policies, books)
	if err != nil {
		return "", nil, err
	}
	if compiled[0].Name != policies[0].Name {
		t.Fatalf("compiled name = %q, want the policy name for audit records", compiled[0].Name)
	}
	return compiled[0].Mode, compiled[0].Automatic, nil
}

func TestCompileRecoveryModes(t *testing.T) {
	mode, auto, err := compileOne(t, func(p *kubeneuronv1alpha1.GPURemediationPolicy) {})
	if err != nil || mode != recoverymode.Unset || auto != nil {
		t.Fatalf("no mode: %q %+v %v, want unset (approval)", mode, auto, err)
	}
	mode, _, err = compileOne(t, func(p *kubeneuronv1alpha1.GPURemediationPolicy) { p.Spec.Mode = "DryRun" })
	if err != nil || mode != recoverymode.DryRun {
		t.Fatalf("DryRun: %q %v", mode, err)
	}
	mode, auto, err = compileOne(t, func(p *kubeneuronv1alpha1.GPURemediationPolicy) {
		p.Spec.Mode, p.Spec.Automatic = "Automatic", automaticSpec()
	})
	if err != nil || mode != recoverymode.Automatic || auto == nil || auto.NodeCooldown != 6*time.Hour ||
		auto.EnabledBy != "alice@example.com" || auto.NodeSelector["pool"] != "train" {
		t.Fatalf("Automatic: %q %+v %v", mode, auto, err)
	}
}

func TestCompileRejectsUnboundedAutomaticPolicies(t *testing.T) {
	cases := map[string]struct {
		mutate func(*kubeneuronv1alpha1.GPURemediationPolicy)
		want   string
	}{
		"automatic without settings": {func(p *kubeneuronv1alpha1.GPURemediationPolicy) { p.Spec.Mode = "Automatic" }, "requires the automatic settings"},
		"settings without automatic": {func(p *kubeneuronv1alpha1.GPURemediationPolicy) { p.Spec.Automatic = automaticSpec() }, "only allowed with mode"},
		"empty node selector": {func(p *kubeneuronv1alpha1.GPURemediationPolicy) {
			p.Spec.Mode, p.Spec.Automatic = "Automatic", automaticSpec()
			p.Spec.Automatic.NodeSelector = nil
		}, "node_selector must name"},
		"wrong acknowledgement": {func(p *kubeneuronv1alpha1.GPURemediationPolicy) {
			p.Spec.Mode, p.Spec.Automatic = "Automatic", automaticSpec()
			p.Spec.Automatic.Acknowledgement = "yes"
		}, "acknowledgement"},
		"checkpoint wait without coordination": {func(p *kubeneuronv1alpha1.GPURemediationPolicy) {
			p.Spec.Mode, p.Spec.Automatic = "Automatic", automaticSpec()
			p.Spec.Automatic.CheckpointWait = "10m"
		}, "checkpointCoordination"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := compileOne(t, tc.mutate)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("compile = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestEffectiveModeShowsInstallationDryRun(t *testing.T) {
	installation := testKubeNeuron() // executionMode unset: DryRun
	if got := effectivePolicyMode(installation, recoverymode.Automatic); got != "DryRun" {
		t.Fatalf("effective mode under installation dry-run = %q, want DryRun", got)
	}
	installation.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModeEnabled
	if got := effectivePolicyMode(installation, recoverymode.Automatic); got != "Automatic" {
		t.Fatalf("effective mode on an Enabled installation = %q, want Automatic", got)
	}
	if got := effectivePolicyMode(installation, recoverymode.Unset); got != "Approval" {
		t.Fatalf("unset mode = %q, want Approval", got)
	}
}

func TestCompileReplacementAndRecycle(t *testing.T) {
	policies, _ := testPolicyFixture()
	books := map[string]*playbook.Playbook{"gpu-reset": {Name: "gpu-reset"}}
	compiled, err := compilePolicies(testKubeNeuron(), policies, books)
	if err != nil || compiled[0].Replacement.Strategy != recoverymode.AddFirst || compiled[0].Recycle.Enabled {
		t.Fatalf("defaults = %+v %v, want add-first and recycle off", compiled[0], err)
	}
	policies[0].Spec.Replacement = &kubeneuronv1alpha1.ReplacementSpec{Strategy: "RemoveFirst"}
	policies[0].Spec.Recycle = &kubeneuronv1alpha1.RecycleSpec{Enabled: true}
	compiled, err = compilePolicies(testKubeNeuron(), policies, books)
	if err != nil || compiled[0].Replacement.Strategy != recoverymode.RemoveFirst || !compiled[0].Recycle.Enabled {
		t.Fatalf("explicit = %+v %v", compiled[0], err)
	}
}

// An Automatic policy shows at once whether it can replace a node without
// approval; other modes carry no such condition.
func TestAutomaticReplacementCondition(t *testing.T) {
	policy := &kubeneuronv1alpha1.GPURemediationPolicy{}
	policy.Spec.Automatic = automaticSpec()
	setAutomaticReplacementCondition(policy, recoverymode.Automatic)
	c := meta.FindStatusCondition(policy.Status.Conditions, "AutomaticReplacement")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NoQualifiedProfiles" {
		t.Fatalf("empty list: %+v", c)
	}
	policy.Spec.Automatic.QualifiedProfiles = []string{"p4d-h100"}
	setAutomaticReplacementCondition(policy, recoverymode.Automatic)
	c = meta.FindStatusCondition(policy.Status.Conditions, "AutomaticReplacement")
	if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "p4d-h100") {
		t.Fatalf("listed: %+v", c)
	}
	setAutomaticReplacementCondition(policy, recoverymode.Approval)
	if meta.FindStatusCondition(policy.Status.Conditions, "AutomaticReplacement") != nil {
		t.Fatal("an Approval policy kept the condition")
	}
}

func TestAutomaticPolicyChecksEscalatedActions(t *testing.T) {
	policies, _ := testPolicyFixture()
	policy := &policies[0]
	policy.Spec.Mode, policy.Spec.Automatic = "Automatic", automaticSpec()
	root := policy.Spec.PlaybookRef
	books := map[string]*playbook.Playbook{
		root:       {Name: root, Steps: []playbook.Step{{Action: "notify.observe"}}, OnFailure: playbook.OnFailure{EscalateTo: "fallback"}},
		"fallback": {Name: "fallback", Steps: []playbook.Step{{Action: "platform.replace_node", Approval: "required"}}},
	}
	installation := testKubeNeuron()
	installation.Spec.Safety.NodeLevelActions = true
	if _, err := compilePolicies(installation, policies, books); err != nil {
		t.Fatalf("cloud replacement escalation: %v", err)
	}
	for _, hostAction := range []string{"agent.reboot", "agent.gpu_reset", "agent.run_diag", "platform.quiesce_accelerator_stack", "platform.restore_accelerator_stack"} {
		books["fallback"].Steps[0].Action = hostAction
		_, err := compilePolicies(installation, policies, books)
		if err == nil || !strings.Contains(err.Error(), hostAction) || !strings.Contains(err.Error(), "fallback") {
			t.Errorf("escalation to %s: got %v, want error naming action and fallback", hostAction, err)
		}
	}
}

func TestCompileRejectsInterruptionPolicyThatDrains(t *testing.T) {
	policies, _ := testPolicyFixture()
	policies[0].Spec.Match.Class = string(types.ClassCloudInterruption)
	books := map[string]*playbook.Playbook{"gpu-reset": {Name: "gpu-reset", Steps: []playbook.Step{{Action: "platform.drain", Approval: "required"}}}}
	if _, err := compilePolicies(testKubeNeuron(), policies, books); err == nil || !strings.Contains(err.Error(), "platform.drain") {
		t.Fatalf("interruption policy with a drain: got %v", err)
	}
	books["gpu-reset"].Steps = []playbook.Step{{Action: playbook.CheckpointWarningAction}, {Action: "platform.cordon", Approval: "required"}}
	if _, err := compilePolicies(testKubeNeuron(), policies, books); err != nil {
		t.Fatalf("interruption policy that warns and cordons: %v", err)
	}
}

func TestCompileInterruptionLead(t *testing.T) {
	policies, _ := testPolicyFixture()
	books := map[string]*playbook.Playbook{"gpu-reset": {Name: "gpu-reset"}}
	compiled, err := compilePolicies(testKubeNeuron(), policies, books)
	if err != nil || compiled[0].Interruption.Lead != 0 {
		t.Fatalf("default = %+v %v, want the default lead", compiled[0].Interruption, err)
	}
	policies[0].Spec.Interruption = &kubeneuronv1alpha1.InterruptionSpec{Lead: "15m"}
	if compiled, err = compilePolicies(testKubeNeuron(), policies, books); err != nil || compiled[0].Interruption.Lead != 15*time.Minute {
		t.Fatalf("explicit = %+v %v", compiled[0].Interruption, err)
	}
	for _, lead := range []string{"30s", "2h", "soon"} {
		policies[0].Spec.Interruption.Lead = lead
		if _, err := compilePolicies(testKubeNeuron(), policies, books); err == nil {
			t.Errorf("lead %s compiled", lead)
		}
	}
}

func TestProfileReferencesCondition(t *testing.T) {
	policy := &kubeneuronv1alpha1.GPURemediationPolicy{}
	policy.Spec.Mode = "Automatic"
	policy.Spec.Automatic = automaticSpec()
	policy.Spec.Automatic.QualifiedProfiles = []string{"existing", "missing"}
	names := map[string]bool{"existing": true}
	setProfileReferencesCondition(policy, names)
	cond := meta.FindStatusCondition(policy.Status.Conditions, "QualifiedProfilesResolved")
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "missing") || strings.Contains(cond.Message, "existing") {
		t.Fatalf("missing reference: %+v", cond)
	}
	names["missing"] = true
	setProfileReferencesCondition(policy, names)
	cond = meta.FindStatusCondition(policy.Status.Conditions, "QualifiedProfilesResolved")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("restored reference: %+v", cond)
	}
	policy.Spec.Mode = "Approval"
	setProfileReferencesCondition(policy, names)
	if meta.FindStatusCondition(policy.Status.Conditions, "QualifiedProfilesResolved") != nil {
		t.Fatal("stale condition")
	}
}
