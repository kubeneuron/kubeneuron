// SPDX-License-Identifier: Apache-2.0

package cloudtest

import (
	"context"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// announcer is the smallest provider that keeps the contract: it reports
// every announcement until told otherwise, each under its own key.
type announcer struct {
	instance string
	notices  []cloud.Interruption
}

func (a *announcer) Announce(kind cloud.InterruptionKind) {
	a.notices = append(a.notices, cloud.Interruption{
		InstanceID: a.instance, Kind: kind, Source: "fake", Key: "fake/" + string(kind),
	})
}

func (a *announcer) Interruptions(_ context.Context, nodes []cloud.NodeRef) ([]cloud.Interruption, error) {
	var out []cloud.Interruption
	for _, n := range nodes {
		for _, i := range a.notices {
			if i.InstanceID == n.Identity.InstanceID {
				out = append(out, i)
			}
		}
	}
	return out, nil
}

func TestInterruptionConformanceAcceptsAKeepingProvider(t *testing.T) {
	InterruptionConformance(t, InterruptionCase{
		New: func(*testing.T) (cloud.InterruptionAware, InterruptionCloud) {
			a := &announcer{instance: "i-1"}
			return a, a
		},
		Node: cloud.NodeRef{Identity: cloud.NodeIdentity{Node: "n1", InstanceID: "i-1"}},
	})
}
