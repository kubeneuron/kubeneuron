package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

func groupPod(ns, name, node, group string, phase corev1.PodPhase) *corev1.Pod {
	p := pod(name, controllerRef("Job"), phase, false)
	p.Namespace = ns
	p.Spec.NodeName = node
	p.UID = k8stypes.UID("uid-" + name)
	p.Annotations = map[string]string{checkpoint.AnnotationOptIn: checkpoint.OptInValue}
	if group != "" {
		p.Annotations[checkpoint.AnnotationGroup] = group
	}
	return p
}

func TestListGroupMembersIsNamespaceScopedLiveAndBounded(t *testing.T) {
	client := fake.NewSimpleClientset(
		groupPod("training", "rank-0", "n1", "run-42", corev1.PodRunning),
		groupPod("training", "rank-1", "n2", "run-42", corev1.PodRunning),
		groupPod("training", "rank-2", "n3", "run-42", corev1.PodSucceeded), // finished: not a member
		groupPod("training", "other", "n1", "run-7", corev1.PodRunning),
		groupPod("training", "loner", "n1", "", corev1.PodRunning),
		groupPod("research", "rank-0", "n1", "run-42", corev1.PodRunning), // another namespace: another group
	)
	p := &Platform{client: client}
	policy := checkpointPolicy()
	policy.Groups = checkpoint.GroupPolicy{Enabled: true, MaxGroupSize: 64}

	members, truncated, err := p.ListGroupMembers(context.Background(), policy, "training", "run-42", 64)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if len(members) != 2 {
		t.Fatalf("members = %+v, want rank-0 and rank-1", members)
	}
	byName := map[string]platform.Workload{}
	for _, m := range members {
		byName[m.Name] = m
	}
	if byName["rank-1"].Node != "n2" || byName["rank-0"].Node != "n1" || byName["rank-1"].UID != "uid-rank-1" {
		t.Fatalf("members carry no node or UID: %+v", members)
	}
	if !byName["rank-0"].Checkpoint().OptedIn() {
		t.Fatal("annotations were not copied")
	}

	// The limit bounds what is returned and reports truncation.
	members, truncated, err = p.ListGroupMembers(context.Background(), policy, "training", "run-42", 1)
	if err != nil || !truncated || len(members) != 1 {
		t.Fatalf("limited: members=%d truncated=%v err=%v", len(members), truncated, err)
	}

	// Scope: a namespace off the allowlist is refused before any read.
	if _, _, err := p.ListGroupMembers(context.Background(), policy, "research", "run-42", 64); !errors.Is(err, platform.ErrCheckpointScope) {
		t.Fatalf("off-allowlist err = %v, want scope", err)
	}
	if _, _, err := p.ListGroupMembers(context.Background(), policy, "training", "", 64); !errors.Is(err, platform.ErrCheckpointScope) {
		t.Fatalf("empty group err = %v, want scope", err)
	}
	if _, _, err := p.ListGroupMembers(context.Background(), checkpoint.Policy{}, "training", "run-42", 64); !errors.Is(err, platform.ErrCheckpointScope) {
		t.Fatalf("disabled policy err = %v, want scope", err)
	}
	// Nothing was written.
	for _, a := range client.Actions() {
		if a.GetVerb() != "list" {
			t.Fatalf("group listing performed a %s", a.GetVerb())
		}
	}
	_ = fmt.Sprint
}
