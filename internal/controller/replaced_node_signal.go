// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// A node whose machine an incident has removed goes on reporting for minutes:
// its agent stops answering and its devices go, and the cloud deletes the
// Node object only later. What it reports then is the removal, not a new
// fault, so the signal is recorded on the incident that removed the machine
// instead of opening an incident of its own. The incident's own fault,
// reported again from there, is not that fault coming back on the node that
// took the machine's place either: it is recorded the same way and does not
// fail the verification. Once that incident waits for a person or has ended,
// the signal is taken as before.
const replacedNodeSignalAction = "replaced-node-signal"

// replacingStates are the states of an incident automation is still working
// on. Only then does an incident that removed a machine take the signals of
// its node.
var replacingStates = []types.IncidentState{
	types.StateOpen, types.StateObserving, types.StateEvaluating,
	types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
}

// removedMachine is the machine inc's replacement has removed, or nil while
// it has removed none: until then the node is in service and what it reports
// may be a fault of its own. An add-first successor is recorded as replaced
// once the cloud accepted the delete of the pinned instance; a targeted
// remove-first delete is confirmed; a GKE plan resizes the pool, and sets it
// back, only once the instance is gone.
func removedMachine(inc *types.Incident) *cloud.NodeIdentity {
	if s := inc.Successor; s != nil && s.Outcome == types.SuccessorReplaced {
		return inc.CloudIdentity
	}
	r := inc.RemoveFirst
	switch {
	case r == nil:
		return nil
	case r.Version == 1 && r.Outcome == types.RemoveFirstDeleteConfirmed:
		return r.DeleteTarget
	case r.Version == 0 && (r.Outcome == types.RemoveFirstRestored ||
		r.Open() && (r.Phase == cloud.RemoveFirstResizing || r.Phase == cloud.RemoveFirstDone)):
		if pin := inc.CloudIdentity; pin != nil && pin.InstanceID == r.Instance {
			return pin
		}
	}
	return nil
}

// fromRemovedMachine reports whether inc, still worked on, removed the
// machine that sent sig: identity, read for sig before the transaction, is
// still that machine's Node. A signal a person raised, or one for a node that
// could not be read, never is.
func fromRemovedMachine(inc *types.Incident, sig types.Signal, identity *cloud.NodeIdentity) bool {
	if sig.Source == types.SourceManual || identity == nil || identity.NodeUID == "" || !slices.Contains(replacingStates, inc.State) {
		return false
	}
	removed := removedMachine(inc)
	return removed != nil && removed.Node == identity.Node && removed.NodeUID == identity.NodeUID
}

// replacingIncidentTx is the incident that removed the machine behind sig's
// node, for a signal that names no incident of its own.
func replacingIncidentTx(ctx context.Context, tx store.Tx, sig types.Signal, identity *cloud.NodeIdentity) (*types.Incident, error) {
	if sig.Source == types.SourceManual || identity == nil || identity.NodeUID == "" {
		return nil, nil
	}
	open, err := tx.ListIncidents(ctx, store.IncidentFilter{Node: sig.Target.Node, States: replacingStates})
	if err != nil {
		return nil, err
	}
	for _, inc := range open {
		if fromRemovedMachine(inc, sig, identity) {
			return inc, nil
		}
	}
	return nil, nil
}

// recordReplacedNodeSignal records sig on inc once for each class and device:
// the sources repeat a signal while it holds (vmalert on every evaluation,
// node health on every read), and a repeat adds nothing. Only the audit trail
// changes: the incident's signal count, its last signal and its timestamps
// stay as they are, so the signal neither delays nor fails its verification.
// The signal_ params name the signal beside its evidence, whose own keys
// (node health's gpu_uuid, for one) describe what the source observed.
// recurrence is whether sig is inc's own fault, which would otherwise have
// counted as the fault coming back.
func recordReplacedNodeSignal(ctx context.Context, tx store.Tx, inc *types.Incident, sig types.Signal, identity *cloud.NodeIdentity, recurrence bool) error {
	trail, err := tx.AuditTrail(ctx, inc.ID)
	if err != nil {
		return err
	}
	for _, entry := range trail {
		if entry.Action == replacedNodeSignalAction && entry.Params["signal_class"] == string(sig.Class) &&
			entry.Params["signal_gpu_uuid"] == sig.Target.GPUUUID && entry.Params["signal_pci_addr"] == sig.Target.PCIAddr {
			return nil
		}
	}
	params := map[string]string{}
	for k, v := range sig.Evidence {
		params[k] = v
	}
	params["signal_class"] = string(sig.Class)
	params["signal_source"] = string(sig.Source)
	params["signal_severity"] = string(sig.Severity)
	params["signal_node_uid"] = identity.NodeUID
	if sig.Target.GPUUUID != "" {
		params["signal_gpu_uuid"] = sig.Target.GPUUUID
	}
	if sig.Target.PCIAddr != "" {
		params["signal_pci_addr"] = sig.Target.PCIAddr
	}
	result := fmt.Sprintf("%s was reported for %s after this incident removed its machine. It is recorded here and no separate incident was opened", sig.Class, sig.Target.Node)
	if recurrence {
		result = fmt.Sprintf("%s was reported again for %s after this incident removed its machine. It is recorded here and does not count as a recurrence", sig.Class, sig.Target.Node)
	}
	return tx.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), FromState: inc.State, ToState: inc.State,
		Actor: "system", Action: replacedNodeSignalAction, Params: params, DryRun: inc.DryRun,
		Result: result,
	})
}
