// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

func dsNames(sets []*appsv1.DaemonSet) []string {
	var out []string
	for _, ds := range sets {
		out = append(out, ds.Name)
	}
	return out
}

// schedulesOn evaluates the agent's node affinity against node labels, for
// the operators the agent DaemonSets use.
func schedulesOn(t *testing.T, ds *appsv1.DaemonSet, labels map[string]string) bool {
	t.Helper()
	affinity := ds.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	for _, term := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		matches := true
		for _, r := range term.MatchExpressions {
			value, present := labels[r.Key]
			switch r.Operator {
			case corev1.NodeSelectorOpIn:
				matches = matches && present && slices.Contains(r.Values, value)
			case corev1.NodeSelectorOpNotIn:
				matches = matches && (!present || !slices.Contains(r.Values, value))
			case corev1.NodeSelectorOpDoesNotExist:
				matches = matches && !present
			default:
				t.Fatalf("%s: unexpected operator %s", ds.Name, r.Operator)
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func TestJournalVariantsRunOneAgentPerNode(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Safety.NodeLevelActions = false
	sets := agentDaemonSets(installation, &Snapshot{Digest: "abc"})
	want := []string{"fleet-agent", "fleet-agent-detect-journal-var", "fleet-agent-detect-journal-run"}
	if got := dsNames(sets); !slices.Equal(got, want) {
		t.Fatalf("agent DaemonSets = %v, want %v", got, want)
	}
	for _, value := range []string{"", "var", "run", "other"} {
		labels := map[string]string{"kubernetes.io/os": "linux"}
		if value != "" {
			labels[kubeneuronv1alpha1.JournalDirLabel] = value
		}
		var on []string
		for _, ds := range sets {
			if schedulesOn(t, ds, labels) {
				on = append(on, ds.Name)
			}
		}
		wantOn := "fleet-agent"
		if value == "var" || value == "run" {
			wantOn = "fleet-agent-detect-journal-" + value
		}
		if len(on) != 1 || on[0] != wantOn {
			t.Errorf("label %q: agents %v, want exactly [%s]", value, on, wantOn)
		}
	}
	selectors := map[string]bool{}
	for _, ds := range sets {
		key := ds.Spec.Selector.MatchLabels["app.kubernetes.io/component"]
		if selectors[key] {
			t.Errorf("%s: selector component %q is shared", ds.Name, key)
		}
		selectors[key] = true
		if key != strings.TrimPrefix(ds.Name, "fleet-") {
			t.Errorf("%s: component %q", ds.Name, key)
		}
	}
}

func TestJournalVariantMountsOneDirectoryReadOnly(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Safety.NodeLevelActions = false
	sets := agentDaemonSets(installation, &Snapshot{Digest: "abc"})
	hostDirs := map[string]string{
		"fleet-agent":                    "",
		"fleet-agent-detect-journal-var": "/var/log/journal",
		"fleet-agent-detect-journal-run": "/run/log/journal",
	}
	for _, ds := range sets {
		spec := ds.Spec.Template.Spec
		container := spec.Containers[0]
		security := container.SecurityContext
		if security.Privileged == nil || *security.Privileged || spec.HostPID ||
			security.Capabilities == nil || !slices.Equal(security.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
			security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
			t.Errorf("%s: agent is not unprivileged: %+v hostPID=%t", ds.Name, security, spec.HostPID)
		}
		var journal []corev1.Volume
		for _, v := range spec.Volumes {
			if v.HostPath != nil && strings.Contains(v.HostPath.Path, "journal") {
				journal = append(journal, v)
			}
		}
		wantDir := hostDirs[ds.Name]
		hasArg := slices.Contains(container.Args, "--journal-dir=/host/journal")
		if wantDir == "" {
			if len(journal) != 0 || hasArg {
				t.Errorf("%s: mounts a journal: %+v arg=%t", ds.Name, journal, hasArg)
			}
			continue
		}
		if len(journal) != 1 || journal[0].HostPath.Path != wantDir ||
			journal[0].HostPath.Type == nil || *journal[0].HostPath.Type != corev1.HostPathDirectory {
			t.Fatalf("%s: journal volumes %+v, want one %s of type Directory", ds.Name, journal, wantDir)
		}
		if !hasArg {
			t.Errorf("%s: args %v lack --journal-dir", ds.Name, container.Args)
		}
		found := false
		for _, m := range container.VolumeMounts {
			if m.Name != journal[0].Name {
				continue
			}
			found = true
			if !m.ReadOnly || m.SubPath != "" || m.SubPathExpr != "" || m.MountPath != journalMountPath ||
				(m.MountPropagation != nil && *m.MountPropagation != corev1.MountPropagationNone) {
				t.Errorf("%s: journal mount %+v, want read-only at %s without subPath", ds.Name, m, journalMountPath)
			}
		}
		if !found {
			t.Errorf("%s: journal volume is not mounted", ds.Name)
		}
	}
}

func TestAgentDaemonSetsFollowTheMode(t *testing.T) {
	cases := []struct {
		name        string
		nodeActions bool
		journal     *bool
		want        []string
		retired     []string
	}{
		{"node-level actions read kmsg", true, nil, []string{"fleet-agent"},
			[]string{"fleet-agent-detect", "fleet-agent-detect-journal-var", "fleet-agent-detect-journal-run"}},
		{"journal disabled", false, ptr.To(false), []string{"fleet-agent"},
			[]string{"fleet-agent-detect", "fleet-agent-detect-journal-var", "fleet-agent-detect-journal-run"}},
		{"journal on by default", false, nil,
			[]string{"fleet-agent", "fleet-agent-detect-journal-var", "fleet-agent-detect-journal-run"},
			[]string{"fleet-agent-detect"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installation := testKubeNeuron()
			installation.Spec.Safety.NodeLevelActions = tc.nodeActions
			if tc.journal != nil {
				installation.Spec.Agent.KernelLog = &kubeneuronv1alpha1.AgentKernelLogSpec{
					Journal: &kubeneuronv1alpha1.AgentJournalSpec{Enabled: tc.journal},
				}
			}
			sets := agentDaemonSets(installation, &Snapshot{})
			if got := dsNames(sets); !slices.Equal(got, tc.want) {
				t.Fatalf("agent DaemonSets = %v, want %v", got, tc.want)
			}
			if got := retiredAgentDaemonSets(installation, &Snapshot{}); !slices.Equal(got, tc.retired) {
				t.Fatalf("retired = %v, want %v", got, tc.retired)
			}
			if len(sets) == 1 && sets[0].Spec.Template.Spec.Affinity != nil {
				t.Errorf("a single agent DaemonSet has an affinity: %+v", sets[0].Spec.Template.Spec.Affinity)
			}
			journal := agentJournal(installation)
			if journal.Enabled != (len(sets) == 3) {
				t.Errorf("controller journal labels enabled = %t with %d DaemonSets", journal.Enabled, len(sets))
			}
		})
	}
}

func TestRuntimeWaitsForEveryAgentVariant(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Safety.NodeLevelActions = false
	installation.UID = "installation-uid"
	owner := metav1.OwnerReference{
		APIVersion: kubeneuronv1alpha1.GroupVersion.String(), Kind: "KubeNeuron",
		Name: installation.Name, UID: installation.UID, Controller: ptr.To(true),
	}
	agentSet := func(name string, desired int32) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: installation.Spec.Namespace, Generation: 1, OwnerReferences: []metav1.OwnerReference{owner}},
			Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: desired,
				CurrentNumberScheduled: desired, UpdatedNumberScheduled: desired, NumberAvailable: desired},
		}
	}
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: installation.Name + "-controller", Namespace: installation.Spec.Namespace, Generation: 1, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	ready := func(objects ...*appsv1.DaemonSet) (bool, string) {
		t.Helper()
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)
		builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment)
		for _, o := range objects {
			builder = builder.WithObjects(o)
		}
		r := &KubeNeuronReconciler{Client: builder.Build(), Scheme: scheme}
		ok, message, err := r.workloadsReady(context.Background(), installation)
		if err != nil {
			t.Fatal(err)
		}
		return ok, message
	}
	if ok, message := ready(agentSet("fleet-agent", 1), agentSet("fleet-agent-detect-journal-var", 2), agentSet("fleet-agent-detect-journal-run", 0)); !ok {
		t.Fatalf("a variant with no node blocked readiness: %s", message)
	}
	if ok, message := ready(agentSet("fleet-agent", 1), agentSet("fleet-agent-detect-journal-var", 2)); ok || !strings.Contains(message, "fleet-agent-detect-journal-run not found") {
		t.Fatalf("missing variant: ready=%t %q", ok, message)
	}
	stuck := agentSet("fleet-agent-detect-journal-var", 2)
	stuck.Status.NumberAvailable = 1
	stuck.Status.NumberUnavailable = 1
	if ok, message := ready(agentSet("fleet-agent", 1), stuck, agentSet("fleet-agent-detect-journal-run", 0)); ok || !strings.Contains(message, "journal-var is not fully available") {
		t.Fatalf("unavailable variant: ready=%t %q", ok, message)
	}
	if ok, message := ready(agentSet("fleet-agent", 0), agentSet("fleet-agent-detect-journal-var", 0), agentSet("fleet-agent-detect-journal-run", 0)); ok || message != "agent daemonset has no desired pods" {
		t.Fatalf("empty fleet: ready=%t %q", ok, message)
	}
}
