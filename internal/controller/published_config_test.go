// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

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

func heldResults(t *testing.T, c *Controller, id string) []string {
	t.Helper()
	trail, err := c.store.AuditTrail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for _, e := range trail {
		if e.Action == "recovery-held" {
			held = append(held, e.Result)
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
	held := heldResults(t, c, id)
	want := "drain held by policy " + autoPolicy + ": the controller has not yet loaded the published configuration; the step is retried when it is loaded. To check, compare source_digest in /api/v1/runtime-config with config-digest in ConfigMap kn-runtime"
	if len(held) != 1 || held[0] != want {
		t.Fatalf("held rows %q, want one %q", held, want)
	}
	if actors, _ := auditActors(t, st, id); strings.Contains(strings.Join(actors, ","), "policy:") {
		t.Fatalf("actors %v: nothing may be authorised under the loaded revision", actors)
	}

	loadDigest(t, c, "digest-new")
	if inc := step(t, c, st, id, 200); inc.State != types.StateResolved {
		t.Fatalf("state=%s after the published configuration was loaded, want RESOLVED", inc.State)
	}
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
			held := heldResults(t, c, id)
			if len(held) == 0 || !strings.Contains(held[0], ": "+tc.reason) ||
				!strings.HasSuffix(held[0], "; the step is retried when it is loaded. To check, compare source_digest in /api/v1/runtime-config with config-digest in ConfigMap kn-runtime") {
				t.Fatalf("held rows %q, want %q in the unloaded-configuration frame", held, tc.reason)
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
