// SPDX-License-Identifier: Apache-2.0

// Package releasepreflight checks that the pre-tag release workflow runs the
// release workflow's checks and builds, with the tag's version replaced by
// the version input, and that nothing in it can publish.
package releasepreflight

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Shared jobs are copied from release.yaml; their only difference is the
// version they read and steps whose id starts with "preflight-".
var shared = []string{"gates", "source-metadata", "manifest", "ctl", "upgrade-rehearsal"}

// The release's jobs that publish, sign or write a release. None of them has
// a counterpart in the preflight.
var publishing = []string{"release", "release-verify", "release-publish"}

// Text no line of the preflight may contain: each one logs in, signs,
// creates or edits a release, or reads a stored secret.
var forbidden = []string{"docker/login-action", "cosign", "gh release create", "gh release edit", "gh release upload", "gh release delete", "gh api", "action-gh-release", "secrets.", "id-token", ": write"}

var versionInput = strings.NewReplacer(
	"${GITHUB_REF_NAME}", "${RELEASE_VERSION}",
	"${{ github.ref_name }}", "${{ inputs.version }}",
)

// Check returns every way the preflight departs from the release workflow.
func Check(release, preflight []byte) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	var rel, pre map[string]any
	if err := yaml.Unmarshal(release, &rel); err != nil {
		return []string{"release.yaml: " + err.Error()}
	}
	if err := yaml.Unmarshal(preflight, &pre); err != nil {
		return []string{"release-preflight.yaml: " + err.Error()}
	}
	for i, line := range strings.Split(string(preflight), "\n") {
		for _, f := range forbidden {
			if strings.Contains(line, f) {
				fail("release-preflight.yaml:%d contains %q", i+1, f)
			}
		}
	}
	if on, _ := pre["on"].(map[string]any); len(on) != 1 || on["workflow_dispatch"] == nil {
		fail("release-preflight.yaml must run on workflow_dispatch only")
	}
	if p, ok := pre["permissions"].(map[string]any); !ok || len(p) != 0 {
		fail("release-preflight.yaml must default to no permissions")
	}
	wantEnv := map[string]any{"RELEASE_VERSION": "${{ inputs.version }}"}
	maps.Copy(wantEnv, mapOf(rel["env"]))
	if !reflect.DeepEqual(mapOf(pre["env"]), wantEnv) {
		fail("release-preflight.yaml env must be release.yaml env plus RELEASE_VERSION")
	}

	relJobs, preJobs := mapOf(rel["jobs"]), mapOf(pre["jobs"])
	known := append(append([]string{"images"}, shared...), publishing...)
	for _, name := range slices.Sorted(maps.Keys(relJobs)) {
		if !slices.Contains(known, name) {
			fail("release.yaml job %s has no preflight counterpart; add it to the preflight or list it as publishing", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(preJobs)) {
		if !slices.Contains(shared, name) && name != "images" {
			fail("release-preflight.yaml job %s is not a release job", name)
		}
		for scope, access := range mapOf(mapOf(preJobs[name])["permissions"]) {
			if access != "read" {
				fail("release-preflight.yaml job %s asks for %s: %v", name, scope, access)
			}
		}
	}
	for _, name := range append([]string{"images"}, shared...) {
		if relJobs[name] == nil {
			fail("release.yaml has no job %s; the preflight must follow the release", name)
		}
		if preJobs[name] == nil {
			fail("release-preflight.yaml has no job %s", name)
		}
	}
	for _, name := range shared {
		if relJobs[name] == nil || preJobs[name] == nil {
			continue
		}
		want := normalize(relJobs[name])
		got := withoutPreflightSteps(preJobs[name])
		if !reflect.DeepEqual(got, want) {
			w, _ := yaml.Marshal(want)
			g, _ := yaml.Marshal(got)
			fail("job %s differs from release.yaml\nwant:\n%s\ngot:\n%s", name, w, g)
		}
	}
	if relJobs["images"] != nil && preJobs["images"] != nil {
		ri, pi := mapOf(relJobs["images"]), mapOf(preJobs["images"])
		for _, key := range []string{"if", "needs", "outputs"} {
			if !reflect.DeepEqual(pi[key], ri[key]) {
				fail("job images: %s differs from release.yaml", key)
			}
		}
		if !reflect.DeepEqual(pi["permissions"], map[string]any{"contents": "read"}) {
			fail("job images must ask only for contents: read")
		}
		run := runText(pi)
		if !strings.Contains(run, `image="localhost:5000/kubeneuron/${name}"`) || strings.Contains(run, `image="${IMAGE_REPO}`) {
			fail("job images must push only to the registry on the runner")
		}
	}
	return problems
}

func normalize(v any) any {
	switch t := v.(type) {
	case string:
		return versionInput.Replace(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	}
	return v
}

func withoutPreflightSteps(job any) any {
	out := maps.Clone(mapOf(job))
	steps, ok := out["steps"].([]any)
	if out == nil || !ok {
		return out
	}
	kept := []any{}
	for _, s := range steps {
		if id, _ := mapOf(s)["id"].(string); strings.HasPrefix(id, "preflight-") {
			continue
		}
		kept = append(kept, s)
	}
	out["steps"] = kept
	return out
}

func runText(job map[string]any) string {
	var b strings.Builder
	steps, _ := job["steps"].([]any)
	for _, s := range steps {
		if run, ok := mapOf(s)["run"].(string); ok {
			b.WriteString(run)
		}
	}
	return b.String()
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
