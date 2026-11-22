// SPDX-License-Identifier: Apache-2.0

// Package resizediff carries the one instance a remove-first replacement
// found new in its pool: the pool's members were read before the first change
// in the cloud and again after the resize finished, and exactly one instance
// was new. It joins the providers that compute it and the controller that
// verifies the node behind it, without a change to pkg/cloud.
package resizediff

import (
	"context"
	"strings"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// MaxMembers is the largest group whose members a provider records for the
// comparison. A larger group is not compared; its replacement is found by
// the pool alone.
const MaxMembers = 100

// stateKey is the key of the new instance's providerID in a remove-first
// plan's State.
//
// The plan's State is the provider's own record; reading one key of it here
// is a convention of this repository. A Replacement field of
// cloud.RemoveFirstPlan would make it part of the contract, at the next tag
// of the pkg/cloud module.
const stateKey = "resize_diff"

// FromPlan is the providerID of the instance a finished plan found new in
// its pool, or "" when none was found.
func FromPlan(plan cloud.RemoveFirstPlan) string {
	if plan.Phase != cloud.RemoveFirstDone {
		return ""
	}
	return plan.State[stateKey]
}

// WithInstance returns state with providerID recorded as the new instance.
func WithInstance(state map[string]string, providerID string) map[string]string {
	out := make(map[string]string, len(state)+1)
	for k, v := range state {
		out[k] = v
	}
	out[stateKey] = providerID
	return out
}

// Added is the one name of after that is not in before and is not replaced,
// compared without regard to case; "" when there is none or more than one.
func Added(before, after []string, replaced string) string {
	var added string
	for _, name := range after {
		if strings.EqualFold(name, replaced) || containsFold(before, name) {
			continue
		}
		if added != "" {
			return ""
		}
		added = name
	}
	return added
}

func containsFold(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// Replacer is a provider whose Replace can say which instance joined the
// pool for the replacement. providerID is "" when the provider could not
// tell; the replacement itself is the same as Replace's.
type Replacer interface {
	ReplaceNamed(ctx context.Context, instanceID string) (providerID string, err error)
}
