// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
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
	if len(auto.QualifiedProfiles) == 0 {
		return fmt.Sprintf("the policy qualifies no node profile for automatic recovery of %s", inc.Class)
	}
	profile, err := qualificationProfile(c.runtimeConfig(ctx).AcceleratorProfiles, labels, inc.Vendor)
	if err != nil {
		return fmt.Sprintf("the node's accelerator runtime profile cannot be resolved (%v), so no qualified pair applies", err)
	}
	if !auto.Qualifies(profile.Name) {
		return fmt.Sprintf("%s on node profile %s is not a qualified pair of the policy", inc.Class, profile.Name)
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
			return nil, fmt.Errorf("%w: %q and %q", config.ErrAmbiguousAcceleratorRuntimeProfile, matched.Name, profiles[i].Name)
		}
		matched = &profiles[i]
	}
	if matched == nil {
		return nil, config.ErrNoAcceleratorRuntimeProfile
	}
	return matched, nil
}
