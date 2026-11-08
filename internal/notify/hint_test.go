package notify

import (
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The suggested approve carries the preview digest the notification showed,
// so running it approves exactly that preview and a recompute in between is
// refused; without a preview it pins the round alone.
func TestApproveHintCarriesTheDigestItShowed(t *testing.T) {
	inc := &types.Incident{ID: "inc-1", ApprovalEpoch: 3}
	if got := ApproveHint(inc, ApprovalContext{}); got != "approve with: kubeneuronctl approve inc-1 --round 3" {
		t.Fatalf("hint without a preview = %q", got)
	}
	if got := ApproveHint(inc, ApprovalContext{ImpactSummary: "2 workloads", ImpactDigest: "sha256:abc"}); got != "approve with: kubeneuronctl approve inc-1 --round 3 --impact-digest sha256:abc" {
		t.Fatalf("hint with a preview = %q", got)
	}
}
