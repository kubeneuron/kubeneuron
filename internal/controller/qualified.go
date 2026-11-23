// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/evidence"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// unlocatedFault says why the recorded evidence does not let an automatic
// policy replace the node, or "": only a fault located on this node does. A
// shared or unknown location, or none recorded, waits for a person.
func (c *Controller) unlocatedFault(ctx context.Context, inc *types.Incident) string {
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return fmt.Sprintf("the incident's evidence cannot be read (%v)", err)
	}
	ev := evidence.Latest(trail)
	if ev == nil {
		return "automatic replacement needs the fault located on this node; no evidence is recorded"
	}
	if ev.Location.Location != types.LocationThisNode {
		return fmt.Sprintf("automatic replacement needs the fault located on this node; the fault location is %s (rule: %s)", ev.Location.Location, ev.Location.Rule)
	}
	return ""
}

// unqualifiedPair says why an automatic policy may not replace this
// incident's node on its own: its class, with the node's accelerator runtime profile, is not
// a pair the policy declares qualified. Empty means qualified. Only the
// policy's declaration qualifies a pair; the evidence block never does.
func (c *Controller) unqualifiedPair(ctx context.Context, inc *types.Incident, auto *recoverymode.Settings, labels map[string]string) string {
	const prefix = "automatic replacement is not allowed on this node — "
	const field = "spec.automatic.qualifiedProfiles"
	const manual = ", or approve this step manually"
	if len(auto.QualifiedProfiles) == 0 {
		return prefix + "the policy lists no profile in " + field + "; list a profile qualified for this class" + manual
	}
	profile, err := qualificationProfile(c.runtimeConfig(ctx).AcceleratorProfiles, labels, inc.Vendor)
	if err != nil {
		var overlap *config.OverlappingProfiles
		switch {
		case errors.Is(err, config.ErrNoAcceleratorRuntimeProfile):
			selector := "this node's labels"
			if inc.Vendor != "" {
				selector += " for vendor " + string(inc.Vendor)
			}
			return prefix + "no AcceleratorRuntimeProfile selects " + selector + "; create one whose nodeSelector matches the node and list it in the policy's " + field + " (see docs/playbook-authoring.md)" + manual
		case errors.As(err, &overlap):
			return fmt.Sprintf("%smore than one AcceleratorRuntimeProfile selects this node (%s and %s); leave exactly one%s", prefix, overlap.First, overlap.Second, manual)
		default:
			return fmt.Sprintf("%s%v; correct the invalid AcceleratorRuntimeProfile%s", prefix, err, manual)
		}
	}
	if !auto.Qualifies(profile.Name) {
		return fmt.Sprintf("%snode profile %s is not listed in the policy's %s for %s; add it there if you have qualified it%s", prefix, profile.Name, field, inc.Class, manual)
	}
	return ""
}

// qualificationProfile is the one profile selecting the node's labels, for the
// incident's vendor when it names one. An incident from an alert names none:
// then the labels alone must select exactly one profile.
func qualificationProfile(profiles []config.AcceleratorRuntimeProfile, labels map[string]string, vendor types.AcceleratorVendor) (*config.AcceleratorRuntimeProfile, error) {
	if vendor != "" {
		return (config.Config{AcceleratorProfiles: profiles}).ResolveAcceleratorRuntimeProfile(labels, vendor)
	}
	var matched *config.AcceleratorRuntimeProfile
	for i := range profiles {
		if !profiles[i].MatchesLabels(labels) {
			continue
		}
		if matched != nil {
			return nil, &config.OverlappingProfiles{First: matched.Name, Second: profiles[i].Name}
		}
		matched = &profiles[i]
	}
	if matched == nil {
		return nil, config.ErrNoAcceleratorRuntimeProfile
	}
	return matched, nil
}
