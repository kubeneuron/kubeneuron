package operator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// TestEveryCompiledChildKindGetsAStatus is the guard against the defect class
// itself rather than against the two kinds that happened to be missing.
//
// CompileSnapshot took six child kinds; the status pass enumerated four of
// them, inline, at a call site nobody revisited when the other two were added.
// The two it skipped were the two safety brakes — GPUMaintenanceWindow, which
// pauses automation while a technician works a row, and GPUNodeConfig, whose
// paused flag is the per-node version of the same thing. Both can fail the
// whole installation compile, and neither could ever say so on itself.
//
// The kind list here is derived from CompileSnapshot's SIGNATURE, not written
// out again. A seventh compiled kind therefore fails this test the moment it is
// added, which is the only thing that stops round 27 finding it.
func TestEveryCompiledChildKindGetsAStatus(t *testing.T) {
	compiled := compiledChildKinds(t)
	if len(compiled) < 6 {
		t.Fatalf("derived only %v from CompileSnapshot's signature; the reflection below has "+
			"stopped tracking the real parameter list and this test now proves nothing", compiled)
	}

	scheme := runtime.NewScheme()
	if err := kubeneuronv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	installation := testKubeNeuron()
	objects := []client.Object{installation}
	made := map[string]client.Object{}
	for _, kind := range compiled {
		obj := newChildOfKind(t, scheme, kind, installation.Name)
		made[kind] = obj
		objects = append(objects, obj)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(objects...).
		Build()
	r := &KubeNeuronReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()
	policies, playbooks, mappings, maintenance, nodeConfigs, profiles, err := r.configuration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A compile failure is the interesting case: it is when an operator most
	// needs each object to say whether it is the culprit or a bystander.
	if err := r.updateChildConfigurationStatuses(ctx, installation,
		policies, playbooks, mappings, profiles, maintenance, nodeConfigs,
		nil, errCompileForTest); err != nil {
		t.Fatalf("status pass returned an error: %v", err)
	}

	for kind, obj := range made {
		fetched := obj.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), fetched); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		conditions := conditionsOf(t, fetched)
		if meta.FindStatusCondition(conditions, "Ready") == nil {
			t.Errorf("%s compiles into the snapshot and can fail the whole installation, but "+
				"carries no Ready condition; an operator looking at this object cannot tell "+
				"whether it landed, and if it is the object that broke the compile its "+
				"innocent siblings are the ones showing red", kind)
		}
	}
}

// errCompileForTest stands in for a real compile failure — the state in which
// every child object must be able to say whether it is the cause.
var errCompileForTest = errors.New("maintenance window \"dc-power-work\": " +
	"nodeSelector.matchExpressions is not supported")

// compiledChildKinds reads the child kinds off CompileSnapshot's parameter
// list. Variadic parameters count too — AcceleratorRuntimeProfile arrives that
// way.
func compiledChildKinds(t *testing.T) []string {
	t.Helper()
	fn := reflect.TypeOf(CompileSnapshot)
	var kinds []string
	for i := 0; i < fn.NumIn(); i++ {
		param := fn.In(i)
		for param.Kind() == reflect.Slice {
			param = param.Elem()
		}
		if param.Kind() != reflect.Struct {
			continue
		}
		// The installation itself is the parent, not a child.
		if param.Name() == "KubeNeuron" {
			continue
		}
		if _, hasSpec := param.FieldByName("Spec"); !hasSpec {
			continue
		}
		if _, hasStatus := param.FieldByName("Status"); !hasStatus {
			continue
		}
		kinds = append(kinds, param.Name())
	}
	return kinds
}

// newChildOfKind builds a minimal object of the named kind, bound to the
// installation. Deliberately minimal: an object that cannot compile is exactly
// the case this test is about.
func newChildOfKind(t *testing.T, scheme *runtime.Scheme, kind, ref string) client.Object {
	t.Helper()
	for gvk := range scheme.AllKnownTypes() {
		if gvk.Kind != kind {
			continue
		}
		obj, err := scheme.New(gvk)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		child, ok := obj.(client.Object)
		if !ok {
			t.Fatalf("%s is not a client.Object", kind)
		}
		child.SetName(strings.ToLower(kind) + "-under-test")
		child.SetGeneration(1)
		spec := reflect.ValueOf(child).Elem().FieldByName("Spec")
		if field := spec.FieldByName("KubeNeuronRef"); field.IsValid() && field.CanSet() {
			field.SetString(ref)
		}
		return child
	}
	t.Fatalf("kind %s is not registered in the scheme", kind)
	return nil
}

func conditionsOf(t *testing.T, obj client.Object) []metav1.Condition {
	t.Helper()
	status := reflect.ValueOf(obj).Elem().FieldByName("Status")
	conditions := status.FieldByName("Conditions")
	if !conditions.IsValid() {
		t.Fatalf("%T has no Status.Conditions", obj)
	}
	out, _ := conditions.Interface().([]metav1.Condition)
	return out
}
