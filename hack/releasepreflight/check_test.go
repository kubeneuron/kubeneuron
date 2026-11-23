// SPDX-License-Identifier: Apache-2.0

package releasepreflight

import (
	"os"
	"strings"
	"testing"
)

func workflows(t *testing.T) (string, string) {
	t.Helper()
	release, err := os.ReadFile("../../.github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := os.ReadFile("../../.github/workflows/release-preflight.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(release), string(preflight)
}

func TestPreflightFollowsTheRelease(t *testing.T) {
	release, preflight := workflows(t)
	for _, p := range Check([]byte(release), []byte(preflight)) {
		t.Error(p)
	}
}

func TestCheckReportsDrift(t *testing.T) {
	release, preflight := workflows(t)
	cases := []struct {
		name, release, preflight, want string
	}{
		{"renamed release job", strings.Replace(release, "\n  ctl:\n", "\n  cli:\n", 1), preflight, "release.yaml has no job ctl"},
		{"new release job", release + "  notarize:\n    runs-on: ubuntu-latest\n    steps: []\n", preflight, "job notarize has no preflight counterpart"},
		{"changed release step", strings.Replace(release, "go build -trimpath", "go build -trimpath -a", 1), preflight, "job ctl differs"},
		{"changed baseline", strings.Replace(release, "BASELINE: v1.0.0-rc.1", "BASELINE: v1.0.0-rc.2", 1), preflight, "job upgrade-rehearsal differs"},
		{"tag push trigger", release, strings.Replace(preflight, "on:\n  workflow_dispatch:", "on:\n  push:\n    tags: [\"v*\"]\n  workflow_dispatch:", 1), "workflow_dispatch only"},
		{"write permission", release, strings.Replace(preflight, "    outputs:\n      digests:", "      packages: write\n    outputs:\n      digests:", 1), `contains ": write"`},
		{"registry login", release, strings.Replace(preflight, "      - uses: anchore/sbom-action", "      - uses: docker/login-action@0000000000000000000000000000000000000000\n      - uses: anchore/sbom-action", 1), `contains "docker/login-action"`},
		{"push to GHCR", release, strings.Replace(preflight, `image="localhost:5000/kubeneuron/${name}"`, `image="${IMAGE_REPO}/${name}"`, 1), "push only to the registry on the runner"},
		{"tag version in a copied job", release, strings.Replace(preflight, `version="${RELEASE_VERSION}"`+"\n          mkdir -p dist\n          for platform", `version="${GITHUB_REF_NAME}"`+"\n          mkdir -p dist\n          for platform", 1), "job ctl differs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.release == release && c.preflight == preflight {
				t.Fatal("the mutation did not apply")
			}
			problems := strings.Join(Check([]byte(c.release), []byte(c.preflight)), "\n")
			if !strings.Contains(problems, c.want) {
				t.Fatalf("want a problem containing %q, got:\n%s", c.want, problems)
			}
		})
	}
}
