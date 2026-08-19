package operator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// internal/detect/fault_coverage_test.go proves every problem class is
// REACHABLE — that a fault described in the neutral encoding lands in the same
// class an XID would. This file is the other half, and it is the half that was
// missing: a class can be perfectly reachable and still have nothing bound to
// it, in which case the incident opens, observes, quiet-resolves, and — until
// the recovery report learned to count it separately — was reported as
// capacity this product returned to service without a human.
//
// The shipped pack is config/policies/. deploy/install.sh embeds a copy of it,
// because a `curl … | bash` installation never sees a repository checkout, and
// the two are pinned to each other below so the copy cannot drift.

const (
	policyPackDir = "../../config/policies"
	installScript = "../../deploy/install.sh"
	// The pack's root object name, and the default install.sh installs under.
	packInstallation = "kubeneuron"
)

// unboundClasses lists the problem classes the shipped pack deliberately does
// NOT bind, each with the reason. Everything a detector can emit that is not
// listed here must have a GPURemediationPolicy in the pack.
//
// A line here is a decision, not an exemption: an unbound class behaves
// exactly like a forgotten one — no error, no alert, an incident that observes
// and closes — so the only thing distinguishing the two is written down here.
// Adding a line is fine; adding one without a reason is not.
var unboundClasses = map[types.ProblemClass]string{
	types.ClassDiagFailure: "no shipped detector emits it: GpuDcgmDiagFailed exists in " +
		"configs/vmalert/gpu-rules.yaml but is not wired into detect.alertClassMap, and no XID " +
		"or neutral fault row classifies into it. A binding would be configuration that can " +
		"never run. TestEveryEmittableClassIsBoundOrExcused fails the moment that changes.",
}

// TestEveryEmittableClassIsBoundOrExcused is the regression gate. It fails when
// a class the shipped detectors can emit has no binding in the pack.
func TestEveryEmittableClassIsBoundOrExcused(t *testing.T) {
	_, policies := loadPolicyPack(t)
	bound := map[types.ProblemClass]string{}
	for _, p := range policies {
		bound[types.ProblemClass(p.Spec.Match.Class)] = p.Spec.PlaybookRef
	}

	var missing []string
	for class, sources := range emittableClasses() {
		if _, ok := bound[class]; ok {
			continue
		}
		if _, excused := unboundClasses[class]; excused {
			continue
		}
		missing = append(missing, string(class)+" (emitted by "+strings.Join(sources, ", ")+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("problem classes the detectors can emit have no policy in the shipped pack:\n  %s\n"+
			"An unbound class does not error and does not alert — the incident observes, quiet-resolves, "+
			"and shows up in the recovery report's \"nothing done\" column instead of as remediation. "+
			"Bind it in %s/policies.yaml, or add it to unboundClasses with the reason it needs no "+
			"automated response.",
			strings.Join(missing, "\n  "), policyPackDir)
	}

	// The excuse list must not outlive what it excuses. Two ways it can rot,
	// and both turn a documented decision into a silent gap.
	emittable := emittableClasses()
	for class, reason := range unboundClasses {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("unboundClasses[%s] carries no reason", class)
		}
		if _, stillDeclared := declaredClasses()[class]; !stillDeclared {
			t.Errorf("unboundClasses excuses %q, which pkg/types no longer declares", class)
		}
		if sources, nowEmitted := emittable[class]; nowEmitted {
			t.Errorf("unboundClasses excuses %q on the grounds that nothing emits it, but %s now does; "+
				"bind it or rewrite the reason", class, strings.Join(sources, ", "))
		}
		if playbook, alsoBound := bound[class]; alsoBound {
			t.Errorf("unboundClasses excuses %q, but the pack binds it to %q; one of the two is wrong",
				class, playbook)
		}
	}
}

// TestFileBasedPolicySetCoversTheSameClasses applies the same coverage rule to
// the OTHER configuration plane.
//
// configs/policies.yaml is what a controller started with -config reads
// straight off disk — the development, kind-integration and bare-metal path.
// It had the wider coverage of the two and was still missing agent-down, and a
// gap here produces exactly the same silent quiet-resolve as a gap in the CRD
// pack. One rule, both planes, one exceptions list.
func TestFileBasedPolicySetCoversTheSameClasses(t *testing.T) {
	cfg, err := config.Load("../../configs/policies.yaml")
	if err != nil {
		t.Fatalf("loading the shipped file-based config: %v", err)
	}
	bound := map[types.ProblemClass]bool{}
	for _, p := range cfg.Policies {
		bound[p.Match.Class] = true
	}
	var missing []string
	for class, sources := range emittableClasses() {
		if bound[class] {
			continue
		}
		if _, excused := unboundClasses[class]; excused {
			continue
		}
		missing = append(missing, string(class)+" (emitted by "+strings.Join(sources, ", ")+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("configs/policies.yaml leaves these emittable classes unbound:\n  %s\n"+
			"Bind them, or add them to unboundClasses in this file with the reason.",
			strings.Join(missing, "\n  "))
	}
}

// TestPolicyPackCompiles runs the pack through the real compiler an operator
// reconcile would. A pack that lists every class and does not compile binds
// nothing at all: CompileSnapshot is all-or-nothing, so one unresolvable
// playbook reference, one escalation cycle, or one cloud action on an
// installation with no spec.cloud takes the WHOLE snapshot down and leaves the
// installation running its previous configuration.
func TestPolicyPackCompiles(t *testing.T) {
	playbooks, policies := loadPolicyPack(t)

	installation := testKubeNeuron()
	installation.ObjectMeta = metav1.ObjectMeta{Name: packInstallation, UID: "installation-uid"}
	for i := range playbooks {
		playbooks[i].Spec.KubeNeuronRef = packInstallation
	}
	for i := range policies {
		policies[i].Spec.KubeNeuronRef = packInstallation
	}

	snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err != nil {
		t.Fatalf("the shipped policy pack does not compile: %v\n"+
			"CompileSnapshot is all-or-nothing — an installation that applies this pack would keep "+
			"running whatever configuration it had before, silently.", err)
	}
	if !strings.Contains(string(snapshot.PoliciesYAML), "dry_run: true") {
		t.Fatal("the pack compiled against a default installation and did not come out dry-run")
	}
	for i := range playbooks {
		if _, ok := snapshot.Playbooks[playbooks[i].Name+".yaml"]; !ok {
			t.Errorf("playbook %q is in the pack but not in the compiled snapshot", playbooks[i].Name)
		}
	}
}

// TestPolicyPackArmsNothingByDefault pins the property that makes it safe to
// SHIP a full policy set rather than one observe-only binding.
//
// The pack exists because an incomplete one inflates the recovery number. A
// complete one that cordons, drains, resets and reboots on its own authority
// would be a considerably worse defect than the one it fixes, and the property
// that prevents it is not "dry-run is the default" — dry-run is stamped per
// incident at open time and an operator will eventually turn it off. It is
// that every step which ends running work carries an approval requirement.
func TestPolicyPackArmsNothingByDefault(t *testing.T) {
	// The actions that end somebody's running work or take the machine away.
	// Cordon is deliberately absent: it stops NEW work from landing, changes
	// nothing that is already running, and is undone by the uncordon at the
	// end of every ladder that applies it.
	endsRunningWork := map[kubeneuronv1alpha1.PlaybookAction]bool{
		kubeneuronv1alpha1.ActionDrain:            true,
		kubeneuronv1alpha1.ActionEvictGPUWorkload: true,
		kubeneuronv1alpha1.ActionReboot:           true,
		kubeneuronv1alpha1.ActionRecycleNode:      true,
		kubeneuronv1alpha1.ActionReplaceNode:      true,
	}
	playbooks, _ := loadPolicyPack(t)
	for i := range playbooks {
		book := &playbooks[i]
		gated := false
		for _, step := range book.Spec.Steps {
			if step.Approval == kubeneuronv1alpha1.ApprovalRequired {
				gated = true
			}
			if endsRunningWork[step.Action] && !gated {
				t.Errorf("playbook %q reaches %s at step %q with no approval asked anywhere above it: "+
					"the shipped pack would end running work on its own authority",
					book.Name, step.Action, step.Name)
			}
			// A device reset is not covered by the rule above — an idle device
			// has no work to end — but it must never be the FIRST thing that
			// happens either, or a stale idle reading becomes a lost job.
			if step.Action == kubeneuronv1alpha1.ActionGPUReset && !gated {
				t.Errorf("playbook %q resets a device at step %q with nothing approved above it",
					book.Name, step.Name)
			}
		}
	}
}

// TestInstallScriptShipsTheSamePack pins the copy of the pack embedded in
// deploy/install.sh to config/policies.
//
// The duplication is not an accident and cannot be removed: the advertised
// install is `curl … | bash`, which has no repository checkout to read the
// pack out of, and the release asset is `kustomize build config/default` —
// CRDs and the operator, no policies. So the installer carries its own copy,
// and the one thing that must never drift is WHICH CLASS GETS WHICH LADDER: a
// checkout install and a piped install that remediate differently is a support
// case nobody would ever guess at.
func TestInstallScriptShipsTheSamePack(t *testing.T) {
	_, policies := loadPolicyPack(t)
	want := map[string]string{}
	for _, p := range policies {
		want[p.Spec.Match.Class] = p.Spec.PlaybookRef
	}

	script, err := os.ReadFile(installScript)
	if err != nil {
		t.Fatalf("reading %s: %v", installScript, err)
	}
	// install.sh writes the pack with $NAME-prefixed object names so two
	// installations cannot collide on these cluster-scoped resources.
	text := strings.ReplaceAll(string(script), "$NAME", packInstallation)

	// The class and its playbook on consecutive lines, which is how both copies
	// are written. Matching them as a PAIR matters: playbookRef also appears
	// under onFailure, so two independent searches would pair a policy's class
	// with an escalation target.
	bindingRe := regexp.MustCompile(`(?m)^\s*match:\s*\{class:\s*([a-z0-9-]+)\}\n\s*playbookRef:\s*([a-z0-9-]+)\s*$`)
	got := map[string]string{}
	for _, m := range bindingRe.FindAllStringSubmatch(text, -1) {
		got[m[1]] = m[2]
	}
	if len(got) == 0 {
		t.Fatalf("found no class bindings in %s; the embedded pack's shape changed and this test "+
			"stopped checking anything", installScript)
	}

	var problems []string
	for class, playbook := range want {
		switch installed, ok := got[class]; {
		case !ok:
			problems = append(problems, class+": bound in config/policies to "+playbook+", absent from install.sh")
		case installed != playbook:
			problems = append(problems, class+": config/policies binds "+playbook+", install.sh binds "+installed)
		}
	}
	for class, playbook := range got {
		if _, ok := want[class]; !ok {
			problems = append(problems, class+": install.sh binds "+playbook+", absent from config/policies")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("the pack install.sh embeds and the pack in %s disagree:\n  %s\n"+
			"A piped install and a checkout install must remediate the same fleet the same way.",
			policyPackDir, strings.Join(problems, "\n  "))
	}
}

// emittableClasses maps every problem class a SHIPPED detector can produce to
// the detectors that produce it. All three ingestion paths are read from the
// code rather than listed here, so a new XID, a new neutral fault row, or a
// new alert mapping puts its class in scope for the coverage gate on the
// commit that adds it.
func emittableClasses() map[types.ProblemClass][]string {
	out := map[types.ProblemClass][]string{}
	add := func(class types.ProblemClass, source string) {
		for _, existing := range out[class] {
			if existing == source {
				return
			}
		}
		out[class] = append(out[class], source)
	}
	for _, x := range detect.XIDTable() {
		add(x.Class, "the XID catalog")
	}
	for _, f := range detect.FaultTable() {
		add(f.Class, "the neutral fault catalog ("+f.Vendor+")")
	}
	for _, class := range detect.AlertClasses() {
		add(class, "an Alertmanager rule")
	}
	for _, sources := range out {
		sort.Strings(sources)
	}
	return out
}

// declaredClasses is the vocabulary pkg/types declares, spelled out so that
// deleting a constant makes its excuse stale rather than silently valid.
func declaredClasses() map[types.ProblemClass]bool {
	return map[types.ProblemClass]bool{
		types.ClassXIDApp: true, types.ClassECCDBE: true, types.ClassECCSBERate: true,
		types.ClassECCContained: true, types.ClassRowRemapOK: true, types.ClassRowRemapFailure: true,
		types.ClassRowRemapBudget: true, types.ClassNVLink: true, types.ClassFellOffBus: true,
		types.ClassGSPError: true, types.ClassThermal: true, types.ClassPower: true,
		types.ClassPCIe: true, types.ClassDriverHang: true, types.ClassGPULost: true,
		types.ClassDiagFailure: true, types.ClassAgentDown: true,
	}
}

func loadPolicyPack(t *testing.T) ([]kubeneuronv1alpha1.GPUPlaybook, []kubeneuronv1alpha1.GPURemediationPolicy) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(policyPackDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var playbooks []kubeneuronv1alpha1.GPUPlaybook
	var policies []kubeneuronv1alpha1.GPURemediationPolicy
	for _, file := range files {
		if filepath.Base(file) == "kustomization.yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, doc := range splitYAMLDocuments(string(raw)) {
			var head struct {
				Kind string `json:"kind"`
			}
			if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			switch head.Kind {
			case "GPUPlaybook":
				var book kubeneuronv1alpha1.GPUPlaybook
				if err := yaml.UnmarshalStrict([]byte(doc), &book); err != nil {
					t.Fatalf("%s: GPUPlaybook: %v", file, err)
				}
				playbooks = append(playbooks, book)
			case "GPURemediationPolicy":
				var policy kubeneuronv1alpha1.GPURemediationPolicy
				if err := yaml.UnmarshalStrict([]byte(doc), &policy); err != nil {
					t.Fatalf("%s: GPURemediationPolicy: %v", file, err)
				}
				policies = append(policies, policy)
			case "":
				// A comment-only document (the pack's file headers).
			default:
				t.Fatalf("%s: unexpected kind %q in the policy pack", file, head.Kind)
			}
		}
	}
	if len(playbooks) == 0 || len(policies) == 0 {
		t.Fatalf("loaded %d playbooks and %d policies from %s; the pack is not being read at all",
			len(playbooks), len(policies), policyPackDir)
	}
	return playbooks, policies
}

// splitYAMLDocuments splits a multi-document YAML stream on its `---` markers.
// Deliberately simple: the pack is written by hand and contains no block
// scalar that could hold that sequence, and a full stream decoder here would
// hide a pack that stopped parsing behind a partially-decoded result.
func splitYAMLDocuments(text string) []string {
	var out []string
	for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(text, -1) {
		if strings.TrimSpace(stripYAMLComments(doc)) != "" {
			out = append(out, doc)
		}
	}
	return out
}

func stripYAMLComments(doc string) string {
	var kept []string
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// vendorScopedByDesign records bindings whose ladder contains a vendor-scoped
// action that a NON-matching vendor can also reach, with why that is the right
// answer rather than a gap to close.
//
// Policies match on class and nothing else — GPURemediationPolicySpec rejects
// source, severity and nodeSelector at admission, and there is no vendor field
// — so a class both vendors emit gets exactly one ladder. Where that ladder
// resets a GPU, the reset action is scoped to NVIDIA, and an AMD incident is
// refused by resetVendorMismatch and parked for a human rather than executed.
//
// That is fail-closed and it matches what docs/reference-capabilities.md
// already promises AMD ("detect, protect and close only: no arming, no reset").
// It is recorded here because the pack otherwise reads as full coverage, and
// the first person to run it on a mixed fleet deserves to find this written
// down rather than in an incident that never moved.
var vendorScopedByDesign = map[types.ProblemClass]string{
	types.ClassECCDBE: "the ladder resets the device; there is no AMD reset action, " +
		"so an AMD incident is refused before the first disruptive step and parked for a human",
	types.ClassNVLink: "same: XGMI link errors classify here, and the repair rung is an NVIDIA reset",
	types.ClassRowRemapOK: "same: AMD page retirement classifies here, and the ladder waits " +
		"for an idle device to reset it",
	types.ClassDriverHang: "same: an amdgpu ring timeout classifies here",
}

// TestVendorScopedLaddersAreDeclared fails when a binding's ladder carries a
// vendor-scoped action reachable by a vendor it is not scoped to, unless that
// pairing is recorded above.
//
// It exists because nothing else could see it. TestPolicyPackCompiles proves
// the pack is valid and TestEveryEmittableClassIsBoundOrExcused proves every
// class has a binding; neither constructs an incident, so neither notices that
// four of the sixteen bindings cannot execute their ladder on AMD silicon.
func TestVendorScopedLaddersAreDeclared(t *testing.T) {
	playbooks, policies := loadPolicyPack(t)

	byName := map[string]kubeneuronv1alpha1.GPUPlaybook{}
	for _, pb := range playbooks {
		byName[pb.Name] = pb
	}

	// Which vendors can emit each class, from the detector tables themselves.
	emitters := map[types.ProblemClass]map[types.AcceleratorVendor]bool{}
	for _, f := range detect.FaultTable() {
		if emitters[f.Class] == nil {
			emitters[f.Class] = map[types.AcceleratorVendor]bool{}
		}
		emitters[f.Class][types.AcceleratorVendor(f.Vendor)] = true
	}

	var undeclared, stale []string
	seen := map[types.ProblemClass]bool{}
	for _, pol := range policies {
		class := types.ProblemClass(pol.Spec.Match.Class)
		pb, ok := byName[pol.Spec.PlaybookRef]
		if !ok {
			continue // TestPolicyPackCompiles owns dangling references
		}
		for _, step := range pb.Spec.Steps {
			def, known := action.ByPlaybookAction(step.Action)
			if !known || def.Vendor == "" {
				continue
			}
			for vendor := range emitters[class] {
				if vendor == def.Vendor || vendor == "" {
					continue
				}
				seen[class] = true
				if _, declared := vendorScopedByDesign[class]; !declared {
					undeclared = append(undeclared, fmt.Sprintf(
						"class %s is emitted for vendor %q but its ladder %q resets via %s, which is scoped to %s: "+
							"that incident is refused and parked, never remediated",
						class, vendor, pb.Name, def.Wire, def.Vendor))
				}
			}
		}
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Fatalf("bindings whose ladder cannot run for a vendor that emits the class:\n  %s\n"+
			"Either bind a ladder that works for both, or record the pairing in vendorScopedByDesign "+
			"with the reason a human should be the one to act.", strings.Join(undeclared, "\n  "))
	}
	for class, reason := range vendorScopedByDesign {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("vendorScopedByDesign[%s] carries no reason", class)
		}
		if !seen[class] {
			stale = append(stale, string(class))
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("vendorScopedByDesign records pairings that no longer exist: %s\n"+
			"A vendor-neutral ladder or a second adapter would do that — remove the entries so the "+
			"list keeps meaning what it says.", strings.Join(stale, ", "))
	}
}
