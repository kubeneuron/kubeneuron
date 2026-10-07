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
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The capability is optional on the Platform contract, and Kubernetes is the
// adapter that provides it.
var _ platform.WorkloadCheckpointer = (*Platform)(nil)

var (
	checkpointNow      = time.Date(2026, 8, 5, 11, 2, 13, 0, time.UTC)
	checkpointDeadline = checkpointNow.Add(8 * time.Minute)
)

func checkpointPolicy() checkpoint.Policy {
	return checkpoint.Policy{
		Enabled:     true,
		DefaultWait: 5 * time.Minute,
		MaxWait:     15 * time.Minute,
		Namespaces:  []string{"training"},
	}
}

// trainingPod is an opted-in, running pod on n1 with a fixed UID, the shape
// every request test starts from.
func trainingPod() *corev1.Pod {
	p := pod("trainer", controllerRef("Job"), corev1.PodRunning, false)
	p.Namespace = "training"
	p.UID = k8stypes.UID("uid-1")
	p.ResourceVersion = "1"
	p.Annotations = map[string]string{
		checkpoint.AnnotationOptIn:   checkpoint.OptInValue,
		checkpoint.AnnotationMaxWait: "8m",
		"example.com/owner":          "team-a",
	}
	return p
}

func listedWorkload(p *corev1.Pod) platform.Workload {
	return platform.Workload{
		Name: p.Name, Namespace: p.Namespace, Kind: "Pod", UID: string(p.UID),
		Annotations: copyAnnotations(p.Annotations),
	}
}

func checkpointRequest(incident string) checkpoint.Request {
	return checkpoint.Request{
		IncidentID:  incident,
		RequestedAt: checkpointNow,
		DeadlineAt:  checkpointDeadline,
		Reason:      types.ProblemClass("ecc-dbe"),
		NextAction:  "platform.drain",
	}
}

func livePod(t *testing.T, client *fake.Clientset, ns, name string) *corev1.Pod {
	t.Helper()
	got, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// recordPatches counts every pod patch the fake sees and captures the last
// body, without interfering with the write.
func recordPatches(client *fake.Clientset) (count *int, last *[]byte) {
	count, last = new(int), new([]byte)
	client.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		*count++
		*last = action.(k8stesting.PatchAction).GetPatch()
		return false, nil, nil
	})
	return count, last
}

func TestNodeWorkloadsSnapshotsIdentityAndAnnotations(t *testing.T) {
	p1 := trainingPod()
	bare := pod("bare", controllerRef("ReplicaSet"), corev1.PodRunning, false)
	bare.UID = k8stypes.UID("uid-bare")
	client := fake.NewSimpleClientset(p1, bare)
	p := &Platform{client: client}

	workloads, err := p.NodeWorkloads(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]platform.Workload{}
	for _, w := range workloads {
		byName[w.Name] = w
	}
	got := byName["trainer"]
	if got.UID != "uid-1" || got.Namespace != "training" || got.Kind != "Pod" {
		t.Fatalf("trainer = %+v, want its live UID and identity", got)
	}
	if got.Annotations[checkpoint.AnnotationOptIn] != checkpoint.OptInValue || got.Annotations["example.com/owner"] != "team-a" {
		t.Fatalf("annotations = %v, want the pod's annotations", got.Annotations)
	}
	if !got.Checkpoint().OptedIn() {
		t.Fatal("the checkpoint view of the listed workload must see the opt-in")
	}
	// The listing is a copy: writing to it must touch nothing the fake holds.
	got.Annotations[checkpoint.AnnotationOptIn] = "tampered"
	if livePod(t, client, "training", "trainer").Annotations[checkpoint.AnnotationOptIn] != checkpoint.OptInValue {
		t.Fatal("NodeWorkloads must return a defensive copy of the annotations")
	}
	if byName["bare"].UID != "uid-bare" || byName["bare"].Annotations != nil {
		t.Fatalf("bare = %+v, want UID and nil annotations for a pod without any", byName["bare"])
	}
}

func TestRequestCheckpointWritesOnlyOperatorOwnedRequestKeys(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, last := recordPatches(client)

	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if err != nil {
		t.Fatal(err)
	}
	if *patches != 1 {
		t.Fatalf("patches = %d, want exactly one", *patches)
	}
	if inForce != checkpointRequest("inc-1") {
		t.Fatalf("in-force request = %+v, want the request as given", inForce)
	}

	var ops []map[string]any
	if err := json.Unmarshal(*last, &ops); err != nil {
		t.Fatalf("patch is not a JSON patch: %v (%s)", err, *last)
	}
	// Identity and node guards come first, then a race guard per key written.
	if ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "uid-1" {
		t.Fatalf("ops[0] = %v, want a UID test", ops[0])
	}
	if ops[1]["op"] != "test" || ops[1]["path"] != "/spec/nodeName" || ops[1]["value"] != "n1" {
		t.Fatalf("ops[1] = %v, want a nodeName test", ops[1])
	}
	written := map[string]bool{}
	for _, op := range ops[2:] {
		path := op["path"].(string)
		if !strings.HasPrefix(path, "/metadata/annotations/") {
			t.Fatalf("op %v writes outside annotations", op)
		}
		key := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(path, "/metadata/annotations/"), "~1", "/"), "~0", "~")
		if _, owned := checkpointRequestKeys[key]; !owned {
			t.Fatalf("op %v touches %q, which is not an operator-owned request key", op, key)
		}
		switch op["op"] {
		case "test":
			if op["value"] != nil {
				t.Fatalf("op %v must test that the key was absent", op)
			}
		case "add":
			written[key] = true
		default:
			t.Fatalf("unexpected op %v", op)
		}
	}
	if len(written) != len(checkpointRequestKeys) {
		t.Fatalf("written keys = %v, want every request key", written)
	}

	live := livePod(t, client, "training", "trainer")
	stamped, ok := checkpoint.ParseRequest(live.Annotations)
	if !ok || stamped != checkpointRequest("inc-1") {
		t.Fatalf("live stamp = %+v (ok=%v), want the request", stamped, ok)
	}
	// The workload-owned keys and foreign metadata are untouched.
	if live.Annotations[checkpoint.AnnotationOptIn] != checkpoint.OptInValue ||
		live.Annotations[checkpoint.AnnotationMaxWait] != "8m" ||
		live.Annotations["example.com/owner"] != "team-a" {
		t.Fatalf("annotations = %v, want the workload's own keys left alone", live.Annotations)
	}
	if _, present := live.Annotations[checkpoint.AnnotationState]; present {
		t.Fatal("the adapter must never write the workload-owned state key")
	}
	if live.Spec.NodeName != "n1" || live.Status.Phase != corev1.PodRunning || len(live.Finalizers) != 0 {
		t.Fatal("spec, status and finalizers must be untouched")
	}
}

// A pod with no annotations at all is the nil-map case: the patch must create
// the map under a null test rather than testing members of a missing map.
func TestRequestCheckpointCreatesTheAnnotationMapWhenAbsent(t *testing.T) {
	p1 := trainingPod()
	p1.Annotations = nil
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	_, last := recordPatches(client)

	if _, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1")); err != nil {
		t.Fatal(err)
	}
	var ops []map[string]any
	if err := json.Unmarshal(*last, &ops); err != nil {
		t.Fatal(err)
	}
	if ops[2]["op"] != "test" || ops[2]["path"] != "/metadata/annotations" || ops[2]["value"] != nil {
		t.Fatalf("ops[2] = %v, want a null test on the whole annotation map", ops[2])
	}
	if ops[3]["op"] != "add" || ops[3]["path"] != "/metadata/annotations" {
		t.Fatalf("ops[3] = %v, want the map created", ops[3])
	}
	if _, ok := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations); !ok {
		t.Fatal("the request must land on a pod that had no annotations")
	}
}

func TestRequestCheckpointRefusesOutOfScopeWithoutPatching(t *testing.T) {
	cases := []struct {
		name   string
		node   string
		mutate func(w *platform.Workload, pol *checkpoint.Policy)
	}{
		{"namespace not on the allowlist", "n1", func(w *platform.Workload, pol *checkpoint.Policy) { pol.Namespaces = []string{"other"} }},
		{"policy disabled", "n1", func(w *platform.Workload, pol *checkpoint.Policy) { pol.Enabled = false }},
		{"different node", "n2", func(*platform.Workload, *checkpoint.Policy) {}},
		{"empty node", "", func(*platform.Workload, *checkpoint.Policy) {}},
		{"listed UID replaced", "n1", func(w *platform.Workload, pol *checkpoint.Policy) { w.UID = "uid-old" }},
		{"no UID to confine to", "n1", func(w *platform.Workload, pol *checkpoint.Policy) { w.UID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p1 := trainingPod()
			client := fake.NewSimpleClientset(p1)
			p := &Platform{client: client}
			patches, _ := recordPatches(client)
			w, pol := listedWorkload(p1), checkpointPolicy()
			tc.mutate(&w, &pol)

			got, err := p.RequestCheckpoint(context.Background(), pol, tc.node, w, checkpointRequest("inc-1"))
			if !errors.Is(err, platform.ErrCheckpointScope) {
				t.Fatalf("err = %v, want ErrCheckpointScope", err)
			}
			if got != (checkpoint.Request{}) {
				t.Fatalf("request = %+v, want zero on refusal", got)
			}
			if *patches != 0 {
				t.Fatal("an out-of-scope request must never patch")
			}
			if _, stamped := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations); stamped {
				t.Fatal("the live pod must be untouched")
			}
		})
	}
}

func TestRequestCheckpointReportsAMissingPodWithoutPatching(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset() // nothing in the cluster
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	_, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if !errors.Is(err, platform.ErrCheckpointWorkloadGone) {
		t.Fatalf("err = %v, want ErrCheckpointWorkloadGone", err)
	}
	if *patches != 0 {
		t.Fatal("a missing pod must never be patched")
	}
}

// A same-named pod in a namespace the policy does not list, on a node that
// matches, with the listed UID: the allowlist alone must stop it, and the fake
// must never see a read of that namespace's pod as a write.
func TestRequestCheckpointNeverPatchesAnotherNamespace(t *testing.T) {
	p1 := trainingPod()
	p1.Namespace = "untrusted"
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	_, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if !errors.Is(err, platform.ErrCheckpointScope) || *patches != 0 {
		t.Fatalf("err = %v, patches = %d; want a scope refusal and no write", err, *patches)
	}
}

func TestRequestCheckpointDoesNotOverwriteALiveForeignRequest(t *testing.T) {
	p1 := trainingPod()
	foreign := checkpointRequest("inc-other")
	foreign.DeadlineAt = checkpointNow.Add(time.Minute) // still live at checkpointNow
	for k, v := range foreign.Annotations() {
		p1.Annotations[k] = v
	}
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if !errors.Is(err, platform.ErrCheckpointForeignRequest) {
		t.Fatalf("err = %v, want ErrCheckpointForeignRequest", err)
	}
	if *patches != 0 {
		t.Fatal("a live foreign request must be left in place")
	}
	// The contract: the parsed live foreign request comes back with the error,
	// so the caller binds its wait to the incident actually on the object.
	if inForce != foreign {
		t.Fatalf("returned request = %+v, want the live foreign request %+v", inForce, foreign)
	}
	stamped, _ := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations)
	if stamped.IncidentID != "inc-other" {
		t.Fatalf("live stamp = %+v, want the foreign incident's request intact", stamped)
	}
}

// The listing a caller holds is a snapshot; the request is refused against the
// LIVE object. When another incident stamped the pod between the two, the
// request returned with the foreign error is the live one, never the listed
// one: a caller that bound itself to the listed incident would read a retained
// acknowledgement of it as an answer to a request that is no longer in force.
func TestRequestCheckpointReturnsTheLiveForeignRequestNotTheListed(t *testing.T) {
	p1 := trainingPod()
	listedForeign := checkpointRequest("inc-A")
	listedForeign.DeadlineAt = checkpointNow.Add(time.Minute)
	for k, v := range listedForeign.Annotations() {
		p1.Annotations[k] = v
	}
	// The workload answered inc-A before inc-B replaced the stamp.
	p1.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-A")
	listed := listedWorkload(p1) // the snapshot still shows inc-A

	liveForeign := checkpointRequest("inc-B")
	liveForeign.RequestedAt = checkpointNow.Add(-30 * time.Second)
	liveForeign.DeadlineAt = checkpointNow.Add(3 * time.Minute)
	for k, v := range liveForeign.Annotations() {
		p1.Annotations[k] = v
	}
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listed, checkpointRequest("inc-1"))
	if !errors.Is(err, platform.ErrCheckpointForeignRequest) || *patches != 0 {
		t.Fatalf("err = %v, patches = %d; want ErrCheckpointForeignRequest and no write", err, *patches)
	}
	if inForce != liveForeign {
		t.Fatalf("returned request = %+v, want the LIVE foreign request %+v, not the listed %+v", inForce, liveForeign, listedForeign)
	}
	for _, leaked := range []string{"inc-A", "inc-B"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error %q carries the annotation value %q", err, leaked)
		}
	}
	// Bound to the live request, the retained inc-A acknowledgement answers
	// nothing; bound to the listed one it would have.
	obs := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listed)
	if got := checkpoint.Classify(checkpointNow, inForce, listed.Checkpoint(), obs); got != checkpoint.OutcomePending {
		t.Fatalf("Classify bound to the live request = %q, want pending", got)
	}
}

// Every error other than the foreign-request one returns the zero request.
func TestRequestCheckpointReturnsZeroRequestOnOtherErrors(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset()
	p := &Platform{client: client}
	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if !errors.Is(err, platform.ErrCheckpointWorkloadGone) || inForce != (checkpoint.Request{}) {
		t.Fatalf("got (%+v, %v), want the zero request with ErrCheckpointWorkloadGone", inForce, err)
	}
}

// The foreign-request error reaches logs and audit rows, so it must carry
// nothing read off the object: the incident annotation is a value a tenant
// with patch rights could have written. The error names the workload only.
func TestForeignRequestErrorCarriesNoAnnotationValue(t *testing.T) {
	const malicious = "inc-other\n{\"level\":\"ERROR\",\"msg\":\"injected log line\"} <script>"
	p1 := trainingPod()
	foreign := checkpointRequest(malicious)
	foreign.DeadlineAt = checkpointNow.Add(time.Minute) // still live at checkpointNow
	for k, v := range foreign.Annotations() {
		p1.Annotations[k] = v
	}
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	_, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if !errors.Is(err, platform.ErrCheckpointForeignRequest) || *patches != 0 {
		t.Fatalf("err = %v, patches = %d; want ErrCheckpointForeignRequest and no write", err, *patches)
	}
	for _, leaked := range []string{malicious, "inc-other", "injected", "<script>", foreign.DeadlineAt.UTC().Format(time.RFC3339)} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error %q carries the annotation value %q", err, leaked)
		}
	}
	if !strings.Contains(err.Error(), "training/trainer") {
		t.Fatalf("error %q does not name the workload", err)
	}
}

// An earlier incident's stamp whose deadline has passed is residue, not a
// hold: the new incident replaces it.
func TestRequestCheckpointReplacesAnExpiredForeignRequest(t *testing.T) {
	p1 := trainingPod()
	stale := checkpointRequest("inc-old")
	stale.DeadlineAt = checkpointNow.Add(-time.Second)
	for k, v := range stale.Annotations() {
		p1.Annotations[k] = v
	}
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}

	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if err != nil {
		t.Fatal(err)
	}
	if inForce.IncidentID != "inc-1" || !inForce.DeadlineAt.Equal(checkpointDeadline) {
		t.Fatalf("in-force = %+v, want the new incident's full window", inForce)
	}
	stamped, _ := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations)
	if stamped.IncidentID != "inc-1" {
		t.Fatalf("live stamp = %+v, want the new incident", stamped)
	}
}

// A restart mid-wait re-requests with a fresh, later deadline; the object's
// earlier deadline for the same incident wins and nothing is rewritten.
func TestRequestCheckpointResumesTheSameIncidentWithoutExtending(t *testing.T) {
	p1 := trainingPod()
	original := checkpointRequest("inc-1")
	for k, v := range original.Annotations() {
		p1.Annotations[k] = v
	}
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	later := original
	later.RequestedAt = checkpointNow.Add(3 * time.Minute)
	later.DeadlineAt = checkpointDeadline.Add(3 * time.Minute)
	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), later)
	if err != nil {
		t.Fatal(err)
	}
	if inForce != original {
		t.Fatalf("in-force = %+v, want the original window %+v", inForce, original)
	}
	if *patches != 0 {
		t.Fatal("an object already carrying the resolved request must not be rewritten")
	}
}

// The same incident with a SHORTER proposed deadline (a shrunken step budget)
// is cut down on the object: nothing on the object may extend a wait.
func TestRequestCheckpointShortensTheSameIncidentDeadline(t *testing.T) {
	p1 := trainingPod()
	original := checkpointRequest("inc-1")
	for k, v := range original.Annotations() {
		p1.Annotations[k] = v
	}
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}

	shorter := original
	shorter.DeadlineAt = checkpointDeadline.Add(-2 * time.Minute)
	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), shorter)
	if err != nil {
		t.Fatal(err)
	}
	if !inForce.DeadlineAt.Equal(shorter.DeadlineAt) {
		t.Fatalf("in-force deadline = %v, want the shorter %v", inForce.DeadlineAt, shorter.DeadlineAt)
	}
	stamped, _ := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations)
	if !stamped.DeadlineAt.Equal(shorter.DeadlineAt) {
		t.Fatalf("live deadline = %v, want %v", stamped.DeadlineAt, shorter.DeadlineAt)
	}
}

func TestRequestCheckpointSurfacesAWriteConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"409 conflict", apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "trainer", nil)},
		{"422 failed test op", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "trainer", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p1 := trainingPod()
			client := fake.NewSimpleClientset(p1)
			p := &Platform{client: client}
			attempts := 0
			client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				attempts++
				return true, nil, tc.err
			})

			got, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
			if !errors.Is(err, platform.ErrCheckpointConflict) {
				t.Fatalf("err = %v, want ErrCheckpointConflict", err)
			}
			if got != (checkpoint.Request{}) || attempts != 1 {
				t.Fatalf("request = %+v, attempts = %d; want zero and a single, unretried write", got, attempts)
			}
		})
	}
}

// The guards are real: a pod whose UID changed between the read and the write
// is rejected by the patch itself, not just by the pre-check.
func TestRequestCheckpointGuardRejectsAReplacementRacingTheWrite(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	// Serve the GET from the listed pod, but let the patch land on a replacement.
	replacement := trainingPod()
	replacement.UID = k8stypes.UID("uid-2")
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, p1.DeepCopy(), nil
	})
	if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, "training"); err != nil {
		t.Fatal(err)
	}

	_, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if err == nil {
		t.Fatal("a UID test that fails must reject the write")
	}
	if _, stamped := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations); stamped {
		t.Fatal("the replacement must not carry the original's request")
	}
}

// recordGets counts every pod GET the fake sees, without interfering with it.
func recordGets(client *fake.Clientset) *int {
	count := new(int)
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		*count++
		return false, nil, nil
	})
	return count
}

// A request the protocol could not carry durably, or one made under a policy
// that cannot be enforced, is refused before the pod is even read: the
// adapter is the last check before `pods: patch` is exercised.
func TestRequestCheckpointRefusesAMalformedRequestBeforeReading(t *testing.T) {
	pol := checkpointPolicy()
	cases := []struct {
		name   string
		mutate func(req *checkpoint.Request, pol *checkpoint.Policy)
	}{
		{"blank incident", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.IncidentID = "" }},
		{"whitespace incident", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.IncidentID = "  \t" }},
		{"padded incident would not read back", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.IncidentID = " inc-1 " }},
		{"blank reason", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.Reason = " " }},
		{"blank next action", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.NextAction = "" }},
		{"zero requested-at", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.RequestedAt = time.Time{} }},
		{"zero deadline", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.DeadlineAt = time.Time{} }},
		{"requested-at not RFC 3339 representable", func(req *checkpoint.Request, _ *checkpoint.Policy) {
			req.RequestedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			req.DeadlineAt = req.RequestedAt.Add(time.Minute)
		}},
		{"deadline not RFC 3339 representable", func(req *checkpoint.Request, _ *checkpoint.Policy) {
			req.DeadlineAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}},
		{"deadline equal to requested-at", func(req *checkpoint.Request, _ *checkpoint.Policy) { req.DeadlineAt = req.RequestedAt }},
		{"deadline before requested-at", func(req *checkpoint.Request, _ *checkpoint.Policy) {
			req.DeadlineAt = req.RequestedAt.Add(-time.Second)
		}},
		{"window one second over maxWait", func(req *checkpoint.Request, pol *checkpoint.Policy) {
			req.DeadlineAt = req.RequestedAt.Add(pol.MaxWait + time.Second)
		}},
		{"policy maxWait over the ceiling", func(_ *checkpoint.Request, pol *checkpoint.Policy) {
			pol.MaxWait = checkpoint.MaxWaitCeiling + time.Minute
		}},
		{"policy maxWait zero", func(_ *checkpoint.Request, pol *checkpoint.Policy) { pol.MaxWait = 0 }},
		{"policy defaultWait over maxWait", func(_ *checkpoint.Request, pol *checkpoint.Policy) {
			pol.DefaultWait = pol.MaxWait + time.Minute
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p1 := trainingPod()
			client := fake.NewSimpleClientset(p1)
			p := &Platform{client: client}
			gets := recordGets(client)
			patches, _ := recordPatches(client)
			req, pol := checkpointRequest("inc-1"), pol.Clone()
			tc.mutate(&req, &pol)

			got, err := p.RequestCheckpoint(context.Background(), pol, "n1", listedWorkload(p1), req)
			if !errors.Is(err, errCheckpointRequestInvalid) {
				t.Fatalf("err = %v, want errCheckpointRequestInvalid", err)
			}
			if got != (checkpoint.Request{}) {
				t.Fatalf("request = %+v, want zero on refusal", got)
			}
			if *gets != 0 || *patches != 0 {
				t.Fatalf("gets = %d, patches = %d; an invalid request must touch nothing", *gets, *patches)
			}
			if _, stamped := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations); stamped {
				t.Fatal("the live pod must be untouched")
			}
		})
	}
}

// A window of exactly MaxWait is the policy's own bound and is stamped as is.
func TestRequestCheckpointAcceptsAWindowExactlyAtMaxWait(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)
	pol := checkpointPolicy()
	req := checkpointRequest("inc-1")
	req.DeadlineAt = req.RequestedAt.Add(pol.MaxWait)

	inForce, err := p.RequestCheckpoint(context.Background(), pol, "n1", listedWorkload(p1), req)
	if err != nil {
		t.Fatal(err)
	}
	if inForce != req || *patches != 1 {
		t.Fatalf("in-force = %+v, patches = %d; want the request stamped in one write", inForce, *patches)
	}
	stamped, ok := checkpoint.ParseRequest(livePod(t, client, "training", "trainer").Annotations)
	if !ok || !stamped.DeadlineAt.Equal(req.DeadlineAt) {
		t.Fatalf("live stamp = %+v (ok=%v), want the full maxWait window", stamped, ok)
	}
}

// The window bound is on the fresh request only. A same-incident stamp already
// on the object that has expired is still the deadline in force, so a restart
// finds the wait over rather than granting a new one.
func TestRequestCheckpointKeepsAnExpiredSameIncidentDeadline(t *testing.T) {
	p1 := trainingPod()
	original := checkpointRequest("inc-1")
	original.RequestedAt = checkpointNow.Add(-20 * time.Minute)
	original.DeadlineAt = checkpointNow.Add(-time.Minute) // expired at checkpointNow
	for k, v := range original.Annotations() {
		p1.Annotations[k] = v
	}
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), checkpointRequest("inc-1"))
	if err != nil {
		t.Fatal(err)
	}
	if inForce != original || *patches != 0 {
		t.Fatalf("in-force = %+v, patches = %d; want the expired original window kept without a write", inForce, *patches)
	}
	if got := checkpoint.Classify(checkpointNow, inForce, listedWorkload(p1).Checkpoint(), p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1))); got != checkpoint.OutcomeExpired {
		t.Fatalf("Classify = %q, want expired: the resumed window is already over", got)
	}
}

func TestObserveCheckpointMapsEachState(t *testing.T) {
	requested := listedWorkload(trainingPod())
	cases := []struct {
		name    string
		pod     *corev1.Pod
		node    string
		want    checkpoint.Outcome
		checkFn func(t *testing.T, obs checkpoint.Observation)
	}{
		{
			name: "acknowledged",
			pod: func() *corev1.Pod {
				// The live object carries inc-1's stamp, as it would after
				// RequestCheckpoint, and the workload answered it.
				p := trainingPod()
				for k, v := range checkpointRequest("inc-1").Annotations() {
					p.Annotations[k] = v
				}
				p.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-1")
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomeAcknowledged,
			checkFn: func(t *testing.T, obs checkpoint.Observation) {
				if !obs.Found || obs.Terminal || obs.UID != "uid-1" {
					t.Fatalf("obs = %+v", obs)
				}
			},
		},
		{
			name: "still running is pending",
			pod:  trainingPod(),
			node: "n1",
			want: checkpoint.OutcomePending,
		},
		{
			// A long-lived workload that acknowledged an EARLIER incident's
			// request still carries that value; it is not an answer to this one.
			name: "an older incident's acknowledgement is stale",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-0")
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomePending,
		},
		{
			name: "an unbound plain complete is not an acknowledgement",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.Annotations[checkpoint.AnnotationState] = "complete"
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomePending,
		},
		{
			// A bound acknowledgement with no request stamp on the live object
			// answers no question in force.
			name: "a bound acknowledgement without a live stamp is not one",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-1")
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomePending,
		},
		{
			// The object has since been stamped by inc-2; inc-1's retained
			// acknowledgement answers inc-1's question, which is gone.
			name: "a bound acknowledgement under another incident's live stamp is stale",
			pod: func() *corev1.Pod {
				p := trainingPod()
				for k, v := range checkpointRequest("inc-2").Annotations() {
					p.Annotations[k] = v
				}
				p.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-1")
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomePending,
		},
		{
			name: "in-progress is not an acknowledgement",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.Annotations[checkpoint.AnnotationState] = "in-progress"
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomePending,
		},
		{
			name: "succeeded is terminal",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.Status.Phase = corev1.PodSucceeded
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomeExited,
			checkFn: func(t *testing.T, obs checkpoint.Observation) {
				if !obs.Terminal {
					t.Fatalf("obs = %+v, want Terminal", obs)
				}
			},
		},
		{
			name: "failed is terminal",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.Status.Phase = corev1.PodFailed
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomeExited,
		},
		{
			name: "being deleted is terminal",
			pod: func() *corev1.Pod {
				p := trainingPod()
				ts := metav1.NewTime(checkpointNow)
				p.DeletionTimestamp = &ts
				p.Finalizers = []string{"example.com/keep"} // the fake needs one to keep a deleting object
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomeExited,
		},
		{
			name: "not found",
			pod:  nil,
			node: "n1",
			want: checkpoint.OutcomeExited,
			checkFn: func(t *testing.T, obs checkpoint.Observation) {
				if obs.Found || obs.Err != nil {
					t.Fatalf("obs = %+v, want a clean not-found", obs)
				}
			},
		},
		{
			name: "UID replacement carries no annotations",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.UID = k8stypes.UID("uid-2")
				p.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-1") // the replacement "acknowledges"
				return p
			}(),
			node: "n1",
			want: checkpoint.OutcomeExited,
			checkFn: func(t *testing.T, obs checkpoint.Observation) {
				if !obs.Found || obs.UID != "uid-2" || obs.Annotations != nil || obs.Err != nil {
					t.Fatalf("obs = %+v, want the replacement's UID and none of its annotations", obs)
				}
			},
		},
		{
			name: "node mismatch is a scope error",
			pod: func() *corev1.Pod {
				p := trainingPod()
				p.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-1")
				return p
			}(),
			node: "n2",
			want: checkpoint.OutcomeUnreachable,
			checkFn: func(t *testing.T, obs checkpoint.Observation) {
				if !errors.Is(obs.Err, platform.ErrCheckpointScope) || obs.Annotations != nil {
					t.Fatalf("obs = %+v, want ErrCheckpointScope and no annotations", obs)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var client *fake.Clientset
			if tc.pod == nil {
				client = fake.NewSimpleClientset()
			} else {
				client = fake.NewSimpleClientset(tc.pod)
			}
			p := &Platform{client: client}
			obs := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), tc.node, requested)
			if tc.checkFn != nil {
				tc.checkFn(t, obs)
			}
			got := checkpoint.Classify(checkpointNow, checkpointRequest("inc-1"), requested.Checkpoint(), obs)
			if got != tc.want {
				t.Fatalf("Classify = %q, want %q (obs %+v)", got, tc.want, obs)
			}
		})
	}
}

// An acknowledgement read from a namespace the policy does not allow is not an
// acknowledgement, and a disabled policy observes nothing.
func TestObserveCheckpointRefusesOutOfScope(t *testing.T) {
	p1 := trainingPod()
	p1.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-1")
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}

	pol := checkpointPolicy()
	pol.Namespaces = []string{"other"}
	obs := p.ObserveCheckpoint(context.Background(), pol, "n1", listedWorkload(p1))
	if !errors.Is(obs.Err, platform.ErrCheckpointScope) || obs.Annotations != nil {
		t.Fatalf("obs = %+v, want ErrCheckpointScope with no annotations", obs)
	}
	if got := checkpoint.Classify(checkpointNow, checkpointRequest("inc-1"), listedWorkload(p1).Checkpoint(), obs); got != checkpoint.OutcomeUnreachable {
		t.Fatalf("Classify = %q, want unreachable", got)
	}

	w := listedWorkload(p1)
	w.UID = ""
	if obs := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", w); !errors.Is(obs.Err, platform.ErrCheckpointScope) {
		t.Fatalf("obs = %+v, want a refusal for a workload with no UID", obs)
	}
}

func TestObserveCheckpointReturnsACopyOfTheAnnotations(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	obs := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1))
	if obs.Err != nil {
		t.Fatal(obs.Err)
	}
	obs.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor("inc-1")
	again := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1))
	if again.Annotations[checkpoint.AnnotationState] != "" {
		t.Fatal("an observation must not alias what the client holds")
	}
}

func TestObserveCheckpointSurfacesAReadFailure(t *testing.T) {
	client := fake.NewSimpleClientset(trainingPod())
	p := &Platform{client: client}
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver down")
	})
	obs := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(trainingPod()))
	if obs.Err == nil || obs.Found {
		t.Fatalf("obs = %+v, want an error and nothing else", obs)
	}
}

// The acknowledgement is bound to the incident named on the object. After a
// controller restart the same incident re-requests, resolves to the stamp
// already there, and must recognize the acknowledgement the workload wrote
// against that stamp; a different incident that later replaces an expired
// stamp must not, because the workload never answered it.
func TestAcknowledgementIsBoundToTheStampedIncidentAcrossARestart(t *testing.T) {
	p1 := trainingPod()
	original := checkpointRequest("inc-1")
	for k, v := range original.Annotations() {
		p1.Annotations[k] = v
	}
	// The workload read checkpoint-incident: inc-1 and answered it.
	p1.Annotations[checkpoint.AnnotationState] = checkpoint.StateCompleteFor(p1.Annotations[checkpoint.AnnotationIncident])
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)

	// The restarted controller re-issues the same incident's request.
	resumed := original
	resumed.RequestedAt = checkpointNow.Add(2 * time.Minute)
	resumed.DeadlineAt = checkpointDeadline.Add(2 * time.Minute)
	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if inForce != original || *patches != 0 {
		t.Fatalf("in-force = %+v, patches = %d; want the original stamp resumed without a write", inForce, *patches)
	}
	obs := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1))
	if got := checkpoint.Classify(resumed.RequestedAt, inForce, listedWorkload(p1).Checkpoint(), obs); got != checkpoint.OutcomeAcknowledged {
		t.Fatalf("Classify after restart = %q, want acknowledged: the bound answer survives the restart", got)
	}
	if _, present := livePod(t, client, "training", "trainer").Annotations[checkpoint.AnnotationState]; !present {
		t.Fatal("the workload-owned acknowledgement must never be deleted by the adapter")
	}

	// inc-1's window ends; inc-2 stamps its own request over the expired one.
	// The workload's old answer stays on the object and answers nothing.
	later := checkpointRequest("inc-2")
	later.RequestedAt = checkpointDeadline.Add(time.Minute)
	later.DeadlineAt = later.RequestedAt.Add(5 * time.Minute)
	inForce2, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(livePod(t, client, "training", "trainer")), later)
	if err != nil {
		t.Fatal(err)
	}
	if inForce2.IncidentID != "inc-2" {
		t.Fatalf("in-force = %+v, want inc-2's own request", inForce2)
	}
	live := livePod(t, client, "training", "trainer")
	if live.Annotations[checkpoint.AnnotationState] != checkpoint.StateCompleteFor("inc-1") {
		t.Fatalf("state = %q; the adapter must leave the workload-owned key exactly as written", live.Annotations[checkpoint.AnnotationState])
	}
	obs2 := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(live))
	if got := checkpoint.Classify(later.RequestedAt, inForce2, listedWorkload(live).Checkpoint(), obs2); got != checkpoint.OutcomePending {
		t.Fatalf("Classify for inc-2 = %q, want pending: inc-1's acknowledgement is stale for inc-2", got)
	}
}

// A sub-second window is a valid grant (checkpoint-max-wait: 500ms) and must
// be stamped as granted: the deadline read back off the object, and the one a
// restarted controller resumes, is the exact fractional time, not the whole
// second it would have been cut to.
func TestRequestCheckpointStampsASubSecondWindowExactly(t *testing.T) {
	p1 := trainingPod()
	client := fake.NewSimpleClientset(p1)
	p := &Platform{client: client}
	patches, _ := recordPatches(client)
	req := checkpointRequest("inc-1")
	req.RequestedAt = checkpointNow.Add(250 * time.Millisecond)
	req.DeadlineAt = req.RequestedAt.Add(500 * time.Millisecond)

	inForce, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(p1), req)
	if err != nil {
		t.Fatal(err)
	}
	if inForce != req || *patches != 1 {
		t.Fatalf("in-force = %+v, patches = %d; want the request stamped as given in one write", inForce, *patches)
	}
	live := livePod(t, client, "training", "trainer")
	if got := live.Annotations[checkpoint.AnnotationDeadlineAt]; got != "2026-08-05T11:02:13.75Z" {
		t.Fatalf("stamped deadline %q, want the fractional second kept", got)
	}
	stamped, ok := checkpoint.ParseRequest(live.Annotations)
	if !ok || !stamped.DeadlineAt.Equal(req.DeadlineAt) || !stamped.RequestedAt.Equal(req.RequestedAt) {
		t.Fatalf("live stamp = %+v (ok=%v), want the exact sub-second window", stamped, ok)
	}

	// The resume path: a restart proposes a later deadline and gets back the
	// exact stamped one, with nothing rewritten.
	resumed := req
	resumed.RequestedAt = req.RequestedAt.Add(100 * time.Millisecond)
	resumed.DeadlineAt = resumed.RequestedAt.Add(5 * time.Minute)
	again, err := p.RequestCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(live), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if !again.DeadlineAt.Equal(req.DeadlineAt) || !again.RequestedAt.Equal(req.RequestedAt) || *patches != 1 {
		t.Fatalf("resumed = %+v, patches = %d; want the exact stamped window and no second write", again, *patches)
	}
	// And the window is judged at the fractional deadline, not a second later.
	obs := p.ObserveCheckpoint(context.Background(), checkpointPolicy(), "n1", listedWorkload(live))
	if got := checkpoint.Classify(req.DeadlineAt.Add(-time.Millisecond), again, listedWorkload(live).Checkpoint(), obs); got != checkpoint.OutcomePending {
		t.Fatalf("Classify 1ms before the deadline = %q, want pending", got)
	}
	if got := checkpoint.Classify(req.DeadlineAt, again, listedWorkload(live).Checkpoint(), obs); got != checkpoint.OutcomeExpired {
		t.Fatalf("Classify at the deadline = %q, want expired", got)
	}
}

// NodeWorkloads publishes, per pod, the exclusion Drain itself applies, from
// the same function Drain uses, so the checkpoint pre-phase asks exactly the
// pods a drain will evict.
func TestNodeWorkloadsCarryTheDrainExclusionDrainApplies(t *testing.T) {
	pods := map[string]*corev1.Pod{
		"managed":   pod("managed", controllerRef("Job"), corev1.PodRunning, false),
		"succeeded": pod("succeeded", controllerRef("Job"), corev1.PodSucceeded, false),
		"failed":    pod("failed", controllerRef("Job"), corev1.PodFailed, false),
		"mirror":    pod("mirror", nil, corev1.PodRunning, true),
		"daemon":    pod("daemon", controllerRef("DaemonSet"), corev1.PodRunning, false),
		"bare":      pod("bare", nil, corev1.PodRunning, false),
	}
	want := map[string]platform.DrainExclusion{
		"managed":   platform.DrainEvictable,
		"succeeded": platform.DrainExclusionTerminal,
		"failed":    platform.DrainExclusionTerminal,
		"mirror":    platform.DrainExclusionInfrastructure,
		"daemon":    platform.DrainExclusionInfrastructure,
		"bare":      platform.DrainExclusionUnmanaged,
	}
	var objs []runtime.Object
	for _, p := range pods {
		objs = append(objs, p)
	}
	p := &Platform{client: fake.NewSimpleClientset(objs...)}
	workloads, err := p.NodeWorkloads(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(workloads) != len(pods) {
		t.Fatalf("listed %d workloads, want %d: the listing itself filters nothing", len(workloads), len(pods))
	}
	for _, w := range workloads {
		if w.DrainExclusion != want[w.Name] {
			t.Errorf("%s: DrainExclusion = %q, want %q", w.Name, w.DrainExclusion, want[w.Name])
		}
		for _, force := range []bool{false, true} {
			if got, drain := w.DrainEligible(force), !skipDuringDrain(pods[w.Name], force); got != drain {
				t.Errorf("%s force=%v: DrainEligible = %v but the drain would skip=%v", w.Name, force, got, !drain)
			}
		}
	}
}
