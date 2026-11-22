// SPDX-License-Identifier: Apache-2.0

package resizediff

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestAdded(t *testing.T) {
	cases := []struct {
		before, after []string
		replaced      string
		want          string
	}{
		{[]string{"a", "b", "c"}, []string{"a", "c", "d"}, "b", "d"},
		{[]string{"A", "b"}, []string{"a", "D"}, "B", "D"},     // case does not make a name new
		{[]string{"a", "b"}, []string{"a", "c", "d"}, "b", ""}, // two new
		{[]string{"a", "b"}, []string{"a"}, "b", ""},           // none new
		{[]string{"a", "b"}, []string{"a", "b"}, "b", ""},      // the replaced one is not new
		{nil, []string{"a"}, "b", "a"},
	}
	for _, tc := range cases {
		if got := Added(tc.before, tc.after, tc.replaced); got != tc.want {
			t.Errorf("Added(%q, %q, %q) = %q, want %q", tc.before, tc.after, tc.replaced, got, tc.want)
		}
	}
}

func TestFromPlanOnlyWhenDone(t *testing.T) {
	state := WithInstance(map[string]string{"size": "3"}, "gce://p/z/n")
	for phase, want := range map[cloud.RemoveFirstPhase]string{
		cloud.RemoveFirstDone:     "gce://p/z/n",
		cloud.RemoveFirstResizing: "",
		cloud.RemoveFirstDeleting: "",
		cloud.RemoveFirstPlanned:  "",
	} {
		if got := FromPlan(cloud.RemoveFirstPlan{Phase: phase, State: state}); got != want {
			t.Errorf("phase %s: %q, want %q", phase, got, want)
		}
	}
	if got := FromPlan(cloud.RemoveFirstPlan{Phase: cloud.RemoveFirstDone}); got != "" {
		t.Errorf("no state: %q", got)
	}
}
