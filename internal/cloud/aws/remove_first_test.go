// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/kubeneuron/kubeneuron/internal/cloud/removefirst"
)

func TestObserveRemoveFirstDelete(t *testing.T) {
	f := &fakeEC2{instanceID: "i-old", states: []ec2types.InstanceStateName{"running", "shutting-down", "terminated"}}
	r := NewWithAPI(f)
	for _, want := range []removefirst.Observation{removefirst.SameMachine, removefirst.Deleting, removefirst.Deleted} {
		if got, err := r.ObserveRemoveFirstDelete(t.Context(), "i-old"); got != want || err != nil {
			t.Fatalf("observation %q, %v; want %q", got, err, want)
		}
	}
	f.instanceID = "i-other"
	if got, err := r.ObserveRemoveFirstDelete(t.Context(), "i-old"); got != removefirst.Unknown || err != nil {
		t.Fatalf("unmatched response: %q, %v", got, err)
	}
	if f.terminateCalls != 0 {
		t.Fatalf("observation terminated %d instances", f.terminateCalls)
	}
}
