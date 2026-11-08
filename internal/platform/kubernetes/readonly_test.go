package kubernetes

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// v0.8 non-goal, pinned at the source: the dynamic client exists to READ
// foreign objects (ResourceSlices, Jobs, Kueue Workloads, RayClusters,
// RayJobs) for the topology classifier and the context resolvers. No code
// path in those files may create, update, patch, delete or apply anything
// through it. A reviewer reading the RBAC tests sees the grant is read-only;
// this test makes sure the code could not use more even if the grant grew.
func TestDynamicClientOnlyReadsForeignObjects(t *testing.T) {
	files := []string{"context.go", "topology.go", "group.go"}
	write := regexp.MustCompile(`\.(Create|Update|UpdateStatus|Patch|Delete|DeleteCollection|Apply|ApplyStatus)\(`)
	dynamicCall := regexp.MustCompile(`p\.dynamic\.Resource\([^)]*\)(\.Namespace\([^)]*\))?\.([A-Za-z]+)\(`)
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		if m := write.FindString(text); m != "" {
			t.Errorf("%s: contains a write call %q; the classifier and the resolvers are read-only by design", name, m)
		}
		for _, m := range dynamicCall.FindAllStringSubmatch(text, -1) {
			switch m[2] {
			case "Get", "List":
			default:
				t.Errorf("%s: dynamic client call %q is not Get or List", name, m[0])
			}
		}
	}
	// The one file that writes to Pods writes annotations only, through the
	// guarded JSON Patch, and never touches any other resource.
	src, err := os.ReadFile("advisory.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "p.dynamic.") {
		t.Errorf("advisory.go: must not use the dynamic client; its only write is the Pod annotation patch")
	}
}
