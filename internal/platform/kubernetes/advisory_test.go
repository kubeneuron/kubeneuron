package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

func advisoryPolicy() checkpoint.Policy {
	p := checkpointPolicy()
	p.Advisory = checkpoint.AdvisoryPolicy{Enabled: true, Budget: checkpoint.DefaultAdvisoryBudget}
	return p
}

func checkpointAdvisory(incident string, round int) checkpoint.Advisory {
	return checkpoint.Advisory{
		IncidentID: incident, Round: round, AdvisedAt: checkpointNow,
		Reason: "ecc-dbe", NextAction: "platform.drain",
	}
}

func TestAdviseCheckpointStampsOnlyAdvisoryKeysWithAGuardedPatch(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset(p1)
	count, last := recordPatches(client)
	p := &Platform{client: client}
	w := listedWorkload(p1)

	if err := p.AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", w, checkpointAdvisory("inc-1", 2)); err != nil {
		t.Fatal(err)
	}
	if *count != 1 {
		t.Fatalf("patches = %d, want 1", *count)
	}
	var ops []map[string]any
	if err := json.Unmarshal(*last, &ops); err != nil {
		t.Fatal(err)
	}
	var tests, adds int
	for _, op := range ops {
		switch op["op"] {
		case "test":
			tests++
		case "add":
			adds++
			path := op["path"].(string)
			key := unescapeAnnotationPath(t, path)
			if _, ok := checkpointAdvisoryKeys[key]; !ok {
				t.Fatalf("patch writes %q, which is not an advisory key", key)
			}
		default:
			t.Fatalf("unexpected op %v", op)
		}
	}
	// identity (uid, node) plus one test per written key, then the writes
	if tests != 2+6 || adds != 6 {
		t.Fatalf("patch shape tests=%d adds=%d, want 8/6: %s", tests, adds, *last)
	}
	live := livePod(t, client, "training", "trainer")
	got, ok := checkpoint.ParseAdvisory(live.Annotations)
	if !ok || got.IncidentID != "inc-1" || got.Round != 2 {
		t.Fatalf("live advisory = %+v (%v), want inc-1 round 2", got, ok)
	}
	for _, key := range []string{checkpoint.AnnotationOptIn, checkpoint.AnnotationMaxWait, "example.com/owner"} {
		if live.Annotations[key] != p1.Annotations[key] {
			t.Fatalf("%s changed: %q -> %q", key, p1.Annotations[key], live.Annotations[key])
		}
	}
	if _, isRequest := checkpoint.ParseRequest(live.Annotations); isRequest {
		t.Fatal("an advisory must never read as a request")
	}

	// The same round again is a no-op; a new round supersedes.
	if err := p.AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", w, checkpointAdvisory("inc-1", 2)); err != nil {
		t.Fatal(err)
	}
	if *count != 1 {
		t.Fatalf("a same-round stamp wrote again: patches = %d", *count)
	}
	later := checkpointAdvisory("inc-1", 3)
	later.AdvisedAt = checkpointNow.Add(time.Minute)
	if err := p.AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", w, later); err != nil {
		t.Fatal(err)
	}
	if got, _ := checkpoint.ParseAdvisory(livePod(t, client, "training", "trainer").Annotations); got.Round != 3 {
		t.Fatalf("round 3 did not supersede round 2: %+v", got)
	}
}

func unescapeAnnotationPath(t *testing.T, path string) string {
	t.Helper()
	const prefix = "/metadata/annotations/"
	if len(path) <= len(prefix) || path[:len(prefix)] != prefix {
		t.Fatalf("patch path %q is not an annotation path", path)
	}
	key := path[len(prefix):]
	// JSON Pointer escaping: ~1 is "/", ~0 is "~".
	out := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		if key[i] == '~' && i+1 < len(key) {
			switch key[i+1] {
			case '1':
				out = append(out, '/')
				i++
				continue
			case '0':
				out = append(out, '~')
				i++
				continue
			}
		}
		out = append(out, key[i])
	}
	return string(out)
}

func TestAdviseCheckpointFailsClosedOnScopeAndIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(pod *corev1.Pod, w *platform.Workload, policy *checkpoint.Policy, adv *checkpoint.Advisory)
		want   error
	}{
		"namespace off the allowlist": {
			mutate: func(_ *corev1.Pod, _ *platform.Workload, policy *checkpoint.Policy, _ *checkpoint.Advisory) {
				policy.Namespaces = []string{"research"}
			},
			want: platform.ErrCheckpointScope,
		},
		"no UID": {
			mutate: func(_ *corev1.Pod, w *platform.Workload, _ *checkpoint.Policy, _ *checkpoint.Advisory) { w.UID = "" },
			want:   platform.ErrCheckpointScope,
		},
		"replaced pod": {
			mutate: func(pod *corev1.Pod, _ *platform.Workload, _ *checkpoint.Policy, _ *checkpoint.Advisory) {
				pod.UID = k8stypes.UID("uid-2")
			},
			want: platform.ErrCheckpointScope,
		},
		"moved node": {
			mutate: func(pod *corev1.Pod, _ *platform.Workload, _ *checkpoint.Policy, _ *checkpoint.Advisory) {
				pod.Spec.NodeName = "n2"
			},
			want: platform.ErrCheckpointScope,
		},
		"invalid advisory": {
			mutate: func(_ *corev1.Pod, _ *platform.Workload, _ *checkpoint.Policy, adv *checkpoint.Advisory) {
				adv.Round = 0
			},
			want: errCheckpointRequestInvalid,
		},
		"disabled policy": {
			mutate: func(_ *corev1.Pod, _ *platform.Workload, policy *checkpoint.Policy, _ *checkpoint.Advisory) {
				*policy = checkpoint.Policy{}
			},
			want: platform.ErrCheckpointScope,
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := trainingPod()
			w := listedWorkload(pod)
			policy := advisoryPolicy()
			adv := checkpointAdvisory("inc-1", 1)
			tc.mutate(pod, &w, &policy, &adv)
			client := fake.NewSimpleClientset(pod)
			count, _ := recordPatches(client)
			err := (&Platform{client: client}).AdviseCheckpoint(context.Background(), policy, "n1", w, adv)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if *count != 0 {
				t.Fatalf("a refused advisory still patched: %d", *count)
			}
		})
	}

	// A pod that is gone.
	client := fake.NewSimpleClientset()
	err := (&Platform{client: client}).AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", listedWorkload(trainingPod()), checkpointAdvisory("inc-1", 1))
	if !errors.Is(err, platform.ErrCheckpointWorkloadGone) {
		t.Fatalf("err = %v, want workload gone", err)
	}

	// A write the apiserver refuses because a guard failed.
	pod := trainingPod()
	client = fake.NewSimpleClientset(pod)
	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "trainer", nil)
	})
	err = (&Platform{client: client}).AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", listedWorkload(pod), checkpointAdvisory("inc-1", 1))
	if !errors.Is(err, platform.ErrCheckpointConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestWithdrawAdvisoryOnlyMarksTheNoticeItIsAbout(t *testing.T) {
	pod := trainingPod()
	client := fake.NewSimpleClientset(pod)
	count, last := recordPatches(client)
	p := &Platform{client: client}
	w := listedWorkload(pod)
	at := checkpointNow.Add(5 * time.Minute)

	// Nothing to withdraw yet.
	withdrawn, err := p.WithdrawAdvisory(context.Background(), advisoryPolicy(), "n1", w, "inc-1", 1, at)
	if err != nil || withdrawn {
		t.Fatalf("withdrawn=%v err=%v on a pod with no advisory", withdrawn, err)
	}
	if err := p.AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", w, checkpointAdvisory("inc-1", 1)); err != nil {
		t.Fatal(err)
	}
	// Another round or incident is not this notice.
	for _, tc := range []struct {
		incident string
		round    int
	}{{"inc-1", 2}, {"inc-other", 1}} {
		withdrawn, err := p.WithdrawAdvisory(context.Background(), advisoryPolicy(), "n1", w, tc.incident, tc.round, at)
		if err != nil || withdrawn {
			t.Fatalf("withdrawn=%v err=%v for %s/%d, want nothing", withdrawn, err, tc.incident, tc.round)
		}
	}
	before := *count
	withdrawn, err = p.WithdrawAdvisory(context.Background(), advisoryPolicy(), "n1", w, "inc-1", 1, at)
	if err != nil || !withdrawn {
		t.Fatalf("withdrawn=%v err=%v, want the withdrawal", withdrawn, err)
	}
	if *count != before+1 {
		t.Fatalf("withdrawal patches = %d, want 1", *count-before)
	}
	var ops []map[string]any
	if err := json.Unmarshal(*last, &ops); err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op["op"] == "add" {
			if key := unescapeAnnotationPath(t, op["path"].(string)); key != checkpoint.AnnotationAdvisoryWithdrawnAt {
				t.Fatalf("withdrawal wrote %q", key)
			}
		}
		if op["op"] == "remove" {
			t.Fatal("a withdrawal must never remove a key")
		}
	}
	live := livePod(t, client, "training", "trainer")
	if !checkpoint.IsAdvisoryWithdrawn(live.Annotations) {
		t.Fatalf("live object is not withdrawn: %v", live.Annotations)
	}
	if got, ok := checkpoint.ParseAdvisory(live.Annotations); !ok || got.Round != 1 {
		t.Fatal("the advisory keys must survive a withdrawal")
	}
	// Idempotent.
	withdrawn, err = p.WithdrawAdvisory(context.Background(), advisoryPolicy(), "n1", w, "inc-1", 1, at.Add(time.Minute))
	if err != nil || withdrawn {
		t.Fatalf("a second withdrawal wrote again: withdrawn=%v err=%v", withdrawn, err)
	}
	// Scope still applies.
	policy := advisoryPolicy()
	policy.Namespaces = []string{"research"}
	if _, err := p.WithdrawAdvisory(context.Background(), policy, "n1", w, "inc-1", 1, at); !errors.Is(err, platform.ErrCheckpointScope) {
		t.Fatalf("err = %v, want scope", err)
	}
}

// The withdrawal patch tests the advisory's incident and round: a stamp of
// a newer round that lands between the read and the write makes the write
// fail instead of withdrawing a notice this call never read.
func TestWithdrawAdvisoryIsGuardedByIncidentAndRound(t *testing.T) {
	pod := trainingPod()
	client := fake.NewSimpleClientset(pod)
	_, last := recordPatches(client)
	p := &Platform{client: client}
	w := listedWorkload(pod)
	if err := p.AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", w, checkpointAdvisory("inc-1", 1)); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := p.WithdrawAdvisory(context.Background(), advisoryPolicy(), "n1", w, "inc-1", 1, checkpointNow.Add(time.Minute))
	if err != nil || !withdrawn {
		t.Fatalf("withdrawn=%v err=%v", withdrawn, err)
	}
	var ops []map[string]any
	if err := json.Unmarshal(*last, &ops); err != nil {
		t.Fatal(err)
	}
	tested := map[string]any{}
	for _, op := range ops {
		if op["op"] == "test" && strings.HasPrefix(op["path"].(string), "/metadata/annotations/") {
			tested[unescapeAnnotationPath(t, op["path"].(string))] = op["value"]
		}
	}
	if tested[checkpoint.AnnotationAdvisoryIncident] != "inc-1" || tested[checkpoint.AnnotationAdvisoryRound] != "1" {
		t.Fatalf("the withdrawal did not test the incident and round it is about: %v", tested)
	}

	// Round 2 lands between this call's read and its write: the read still
	// shows round 1 (a stale GET), the object already carries round 2.
	stale := pod.DeepCopy()
	for k, v := range checkpointAdvisory("inc-1", 1).Annotations() {
		stale.Annotations[k] = v
	}
	delete(stale.Annotations, checkpoint.AnnotationAdvisoryWithdrawnAt)
	if err := p.AdviseCheckpoint(context.Background(), advisoryPolicy(), "n1", w, checkpointAdvisory("inc-1", 2)); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, stale, nil })
	withdrawn, err = p.WithdrawAdvisory(context.Background(), advisoryPolicy(), "n1", w, "inc-1", 1, checkpointNow.Add(2*time.Minute))
	// A real apiserver answers a failed test op with 422 (ErrCheckpointConflict
	// after mapping); the fake tracker answers a plain error. Either way the
	// write is refused and nothing is withdrawn.
	if withdrawn || err == nil || !strings.Contains(err.Error(), "checkpoint-advisory-round") {
		t.Fatalf("withdrawn=%v err=%v, want the write refused on the round test: the object moved to round 2 under it", withdrawn, err)
	}
	live, _ := client.CoreV1().Pods("training").Get(context.Background(), "trainer", metav1.GetOptions{})
	if checkpoint.IsAdvisoryWithdrawn(live.Annotations) {
		t.Fatal("round 2's notice was withdrawn by round 1's request")
	}
}
