// Package impact renders the impact preview of an approval round
// (docs/workload-safe-recovery-design.md, S4): what a human is shown about
// the node, its workloads and the disruption downstream before they approve
// a destructive step.
//
// Nothing here talks to a cluster or a store. Compute takes a snapshot the
// controller gathered under its own budget and renders a durable, canonical
// impact-preview/v1 document with a digest over it, so what the human saw is
// exactly identifiable later. The preview is advisory: it cannot block an
// approve, and it never invents a number it did not count — in particular it
// never estimates lost training progress, which KubeNeuron does not know.
package impact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Version is the document version every preview carries.
const Version = "impact-preview/v1"

// Bounds. DefaultMaxItems and the ceiling mirror the CRD; MaxPayloadBytes is
// the store's bound on one record, above which the preview is unavailable
// with PayloadTooLarge rather than truncated silently.
const (
	DefaultMaxItems = 200
	MinMaxItems     = 10
	MaxMaxItems     = 1000
	DefaultBudget   = 15 * time.Second
	BudgetCeiling   = 60 * time.Second
	MaxPayloadBytes = 256 << 10
)

// Unavailable reasons. The set is closed; the string on a record is one of
// these.
const (
	UnavailableListingFailed   = "WorkloadListingFailed"
	UnavailableListingTimeout  = "WorkloadListingTimeout"
	UnavailableTopologyFailed  = "TopologyUnavailable"
	UnavailablePayloadTooLarge = "PayloadTooLarge"
	UnavailableStoreMissing    = "StoreUnavailable"
	UnavailableNoPendingStep   = "NoPendingStep"
)

// Warnings a preview may carry. Closed set.
const (
	WarningTimeSlicedAttribution = "TimeSlicedAttributionDeviceWide"
	WarningTopologyRefused       = "DeviceActionsRefused"
	WarningItemsTruncated        = "ItemsTruncated"
	WarningContextNotResolved    = "WorkloadContextNotResolved"
	WarningGroupTooLarge         = "GroupTooLarge"
	WarningGroupTotalUnknown     = "GroupTotalUnknown"
)

// Policy is the compiled spec.safety.impactPreview the renderer needs.
type Policy struct {
	Enabled            bool
	Budget             time.Duration
	MaxItems           int
	RequireForApproval bool
	// Context is which read-only resolvers to consult per itemised workload.
	Context platform.ContextSelection
}

// Input is the snapshot Compute renders. The controller fills it under the
// round's budget; Compute itself does no I/O.
type Input struct {
	IncidentID string
	Round      int
	ComputedAt time.Time
	Node       string
	// Topology is the node's classification, or nil when none could be built
	// (TopologyUnavailable is then a warning, not an unavailable preview:
	// the workloads are still worth showing).
	Topology *topology.Classification
	// PendingStep is the step waiting for approval; Downstream the
	// coordinating platform ops at or after it ("platform.drain", ...).
	PendingStep string
	Downstream  []string
	// Workloads is the node listing; Force the pending drain's force flag.
	Workloads []platform.Workload
	Force     bool
	// Op is the first downstream coordinating op ("drain" or
	// "evict_gpu_workload"), or "" when none.
	Op string
	// StepBudget is the time that disruption step would have for
	// coordination (its intended budget minus the reserve for the
	// disruption itself), so the effective wait shown per workload is the
	// one the step would actually grant. Zero or less means "not known":
	// only the policy bound applies, as in checkpoint.Policy.BoundedWait.
	StepBudget time.Duration
	// Checkpoint is the coordination policy; Class the incident's class.
	Checkpoint checkpoint.Policy
	Class      types.ProblemClass
	// Groups, when the group policy is on, bounds what the renderer says
	// about groups. Nil means groups are off.
	Groups *GroupPolicy
	// GroupTotals, keyed "namespace/group", is the live membership the
	// controller listed for each group seen on the node (bounded by the
	// policy); GroupTruncated marks the ones the listing could not bound.
	// Absent entries mean "not listed": the group's total is then UNKNOWN
	// (members_total_known false, GroupTotalUnknown warned), never the
	// on-node count dressed as a total.
	GroupTotals    map[string]int
	GroupTruncated map[string]bool
	// Contexts, per "namespace/name", is what the read-only resolvers found
	// (S5); nil entries are absent. Resolvers that are on but answered
	// not-observed add their name to ContextNotObserved.
	Contexts           map[string]json.RawMessage
	Owners             map[string][]string
	ContextNotObserved []string
	MaxItems           int
}

// GroupPolicy mirrors the S3 policy the renderer needs.
type GroupPolicy struct {
	MaxGroupSize int
	CrossNode    bool
}

// Preview is the impact-preview/v1 document.
type Preview struct {
	Version    string    `json:"version"`
	Incident   string    `json:"incident"`
	Round      int       `json:"round"`
	ComputedAt time.Time `json:"computed_at"`
	Digest     string    `json:"digest"`

	Node                  NodeView         `json:"node"`
	PendingStep           string           `json:"pending_step"`
	DownstreamDisruptions []string         `json:"downstream_disruptions"`
	Workloads             WorkloadsView    `json:"workloads"`
	Devices               DevicesView      `json:"devices"`
	CheckpointPolicy      CheckpointView   `json:"checkpoint_policy"`
	Limits                LimitsView       `json:"limits"`
	Warnings              []string         `json:"warnings"`
	Unavailable           *UnavailableView `json:"unavailable"`
}

// NodeView carries the node and its topology classification.
type NodeView struct {
	Name     string                   `json:"name"`
	Topology *topology.Classification `json:"topology,omitempty"`
}

// WorkloadsView is the counted and itemised listing.
type WorkloadsView struct {
	Total                 int            `json:"total"`
	GPUHolders            int            `json:"gpu_holders"`
	Namespaces            int            `json:"namespaces"`
	EligibleForCheckpoint int            `json:"eligible_for_checkpoint"`
	Ineligible            map[string]int `json:"ineligible"`
	Groups                []GroupView    `json:"groups"`
	Items                 []Item         `json:"items"`
}

// GroupView is one checkpoint group seen on the node.
type GroupView struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	MembersOnNode int    `json:"members_on_node"`
	// MembersTotal is the group's live membership across the cluster when
	// the controller could list it (MembersTotalKnown); otherwise it is the
	// on-node count, which is a lower bound and NOT the total — the preview
	// then carries WarningGroupTotalUnknown, because cross-node impact is
	// exactly what a group preview exists to show.
	MembersTotal       int  `json:"members_total"`
	MembersTotalKnown  bool `json:"members_total_known"`
	CrossNodeRequested bool `json:"cross_node_requested"`
	TooLarge           bool `json:"too_large,omitempty"`
}

// Item is one workload on the node.
type Item struct {
	Namespace  string          `json:"namespace"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	UID        string          `json:"uid"`
	UsesGPU    bool            `json:"uses_gpu"`
	OwnerChain []string        `json:"owner_chain,omitempty"`
	Context    json.RawMessage `json:"context,omitempty"`
	Checkpoint CheckpointItem  `json:"checkpoint"`
}

// CheckpointItem is the coordination verdict for one workload.
type CheckpointItem struct {
	OptedIn       bool   `json:"opted_in"`
	Eligible      bool   `json:"eligible"`
	Ineligibility string `json:"ineligibility,omitempty"`
	EffectiveWait string `json:"effective_wait,omitempty"`
	Group         string `json:"group,omitempty"`
}

// DevicesView summarises the device layout from the classification.
type DevicesView struct {
	Whole              int    `json:"whole"`
	MIGInstances       int    `json:"mig_instances"`
	TimeSlicedReplicas int    `json:"time_sliced_replicas"`
	Attribution        string `json:"attribution"`
}

// CheckpointView is the effective coordination policy.
type CheckpointView struct {
	Enabled     bool   `json:"enabled"`
	DefaultWait string `json:"default_wait,omitempty"`
	MaxWait     string `json:"max_wait,omitempty"`
	Advisory    bool   `json:"advisory"`
	Groups      bool   `json:"groups"`
}

// LimitsView says how the listing was bounded.
type LimitsView struct {
	ItemsTruncated bool `json:"items_truncated"`
	MaxItems       int  `json:"max_items"`
}

// UnavailableView is set when the preview could not be computed.
type UnavailableView struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Compute renders the preview from the snapshot and seals it with a digest.
func Compute(in Input) Preview {
	maxItems := in.MaxItems
	if maxItems < MinMaxItems || maxItems > MaxMaxItems {
		maxItems = DefaultMaxItems
	}
	p := Preview{
		Version: Version, Incident: in.IncidentID, Round: in.Round, ComputedAt: in.ComputedAt.UTC(),
		Node:                  NodeView{Name: in.Node, Topology: in.Topology},
		PendingStep:           in.PendingStep,
		DownstreamDisruptions: append([]string{}, in.Downstream...),
		Workloads:             WorkloadsView{Ineligible: map[string]int{}, Groups: []GroupView{}, Items: []Item{}},
		Devices:               DevicesView{Attribution: string(topology.AttributionUnknown)},
		CheckpointPolicy: CheckpointView{
			Enabled: in.Checkpoint.Enabled, Advisory: in.Checkpoint.Advisory.Enabled, Groups: in.Groups != nil,
		},
		Limits:   LimitsView{MaxItems: maxItems},
		Warnings: []string{},
	}
	if in.Checkpoint.Enabled {
		p.CheckpointPolicy.DefaultWait = in.Checkpoint.DefaultWait.String()
		p.CheckpointPolicy.MaxWait = in.Checkpoint.MaxWait.String()
	}
	if in.Topology != nil {
		p.Devices = devices(*in.Topology)
		if in.Topology.Topology == topology.TopologyTimeSliced {
			p.Warnings = append(p.Warnings, WarningTimeSlicedAttribution)
		}
		if !in.Topology.Eligible() {
			p.Warnings = append(p.Warnings, WarningTopologyRefused)
		}
	}

	namespaces := map[string]struct{}{}
	groups := map[string]*GroupView{}
	sorted := append([]platform.Workload(nil), in.Workloads...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Name < sorted[j].Name
	})
	for _, w := range sorted {
		p.Workloads.Total++
		namespaces[w.Namespace] = struct{}{}
		if w.UsesGPU {
			p.Workloads.GPUHolders++
		}
		cw := w.Checkpoint()
		code := checkpoint.Evaluate(checkpoint.Candidate{
			Workload: cw, DrainEligible: w.DrainEligible(in.Force), UsesGPU: w.UsesGPU,
		}, in.Checkpoint, in.Class, in.Op)
		item := Item{
			Namespace: w.Namespace, Name: w.Name, Kind: w.Kind, UID: w.UID, UsesGPU: w.UsesGPU,
			Checkpoint: CheckpointItem{OptedIn: cw.OptedIn(), Eligible: code == "", Ineligibility: string(code)},
		}
		if code == "" {
			p.Workloads.EligibleForCheckpoint++
			item.Checkpoint.EffectiveWait = in.Checkpoint.BoundedWait(cw, in.StepBudget).String()
		} else {
			p.Workloads.Ineligible[string(code)]++
		}
		if in.Groups != nil {
			if name, invalid := cw.Group(); invalid {
				p.Workloads.Ineligible[string(checkpoint.IneligibleGroupInvalid)]++
			} else if name != "" {
				item.Checkpoint.Group = name
				key := w.Namespace + "/" + name
				g, ok := groups[key]
				if !ok {
					g = &GroupView{Name: name, Namespace: w.Namespace, CrossNodeRequested: in.Groups.CrossNode}
					groups[key] = g
				}
				g.MembersOnNode++
			}
		}
		if ctx, ok := in.Contexts[w.Namespace+"/"+w.Name]; ok && len(ctx) > 0 {
			item.Context = ctx
		}
		if owners, ok := in.Owners[w.Namespace+"/"+w.Name]; ok && len(owners) > 0 {
			item.OwnerChain = append([]string(nil), owners...)
		}
		if len(p.Workloads.Items) < maxItems {
			p.Workloads.Items = append(p.Workloads.Items, item)
		} else {
			p.Limits.ItemsTruncated = true
		}
	}
	p.Workloads.Namespaces = len(namespaces)
	if p.Limits.ItemsTruncated {
		p.Warnings = append(p.Warnings, WarningItemsTruncated)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		// The on-node count is what this listing proved; the total comes from
		// the group listing when the controller ran one. Without one the
		// total is unknown and said to be: the on-node count stands in as a
		// lower bound, flagged, never as the answer.
		if total, ok := in.GroupTotals[k]; ok {
			g.MembersTotal = total
			g.MembersTotalKnown = true
		}
		if g.MembersTotal < g.MembersOnNode {
			g.MembersTotal = g.MembersOnNode
		}
		if !g.MembersTotalKnown && !contains(p.Warnings, WarningGroupTotalUnknown) {
			p.Warnings = append(p.Warnings, WarningGroupTotalUnknown)
		}
		if in.GroupTruncated[k] {
			g.TooLarge = true
			if !contains(p.Warnings, WarningGroupTooLarge) {
				p.Warnings = append(p.Warnings, WarningGroupTooLarge)
			}
		}
		if in.Groups != nil && in.Groups.MaxGroupSize > 0 && g.MembersTotal > in.Groups.MaxGroupSize {
			g.TooLarge = true
			if !contains(p.Warnings, WarningGroupTooLarge) {
				p.Warnings = append(p.Warnings, WarningGroupTooLarge)
			}
		}
		p.Workloads.Groups = append(p.Workloads.Groups, *g)
	}
	if len(in.ContextNotObserved) > 0 && !contains(p.Warnings, WarningContextNotResolved) {
		p.Warnings = append(p.Warnings, WarningContextNotResolved)
	}
	sort.Strings(p.Warnings)
	return seal(p)
}

// Unavailable renders a preview that could not be computed: the identity and
// the reason, nothing counted, sealed like any other so the record is
// identifiable.
func Unavailable(in Input, reason, detail string) Preview {
	p := Preview{
		Version: Version, Incident: in.IncidentID, Round: in.Round, ComputedAt: in.ComputedAt.UTC(),
		Node: NodeView{Name: in.Node}, PendingStep: in.PendingStep,
		DownstreamDisruptions: append([]string{}, in.Downstream...),
		Workloads:             WorkloadsView{Ineligible: map[string]int{}, Groups: []GroupView{}, Items: []Item{}},
		Devices:               DevicesView{Attribution: string(topology.AttributionUnknown)},
		Limits:                LimitsView{MaxItems: in.MaxItems},
		Warnings:              []string{},
		Unavailable:           &UnavailableView{Reason: reason, Detail: strings.TrimSpace(detail)},
	}
	return seal(p)
}

// seal computes the digest over the canonical JSON of the document with an
// empty digest field, then stores it. Two renderings of the same snapshot
// share a digest; any change — including the computation time, so a
// recompute is a new record the human must see again — changes it.
func seal(p Preview) Preview {
	p.Digest = ""
	canonical, err := json.Marshal(p)
	if err != nil {
		// Marshal of these plain structs cannot fail; a digest over the
		// error text keeps the record identifiable if it ever does.
		canonical = []byte("impact-preview marshal error: " + err.Error())
	}
	sum := sha256.Sum256(canonical)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p
}

// Record renders the preview as the store record, enforcing the payload
// bound: a document above MaxPayloadBytes is stored as unavailable with
// PayloadTooLarge rather than truncated.
func Record(p Preview) (*types.ImpactPreview, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxPayloadBytes && p.Unavailable == nil {
		p = Unavailable(Input{IncidentID: p.Incident, Round: p.Round, ComputedAt: p.ComputedAt, Node: p.Node.Name,
			PendingStep: p.PendingStep, Downstream: p.DownstreamDisruptions, MaxItems: p.Limits.MaxItems},
			UnavailablePayloadTooLarge, "rendered document exceeds the record bound")
		payload, err = json.Marshal(p)
		if err != nil {
			return nil, err
		}
	}
	rec := &types.ImpactPreview{
		IncidentID: p.Incident, Epoch: p.Round, ComputedAt: p.ComputedAt, Version: p.Version, Digest: p.Digest,
		Payload: payload,
	}
	if p.Unavailable != nil {
		rec.UnavailableReason = p.Unavailable.Reason
	}
	return rec, nil
}

// Parse reads a stored record back into its document.
func Parse(rec *types.ImpactPreview) (Preview, error) {
	var p Preview
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		return Preview{}, err
	}
	return p, nil
}

// Summary renders the three-to-four-line digest a notification carries.
func Summary(p Preview) string {
	if p.Unavailable != nil {
		return "impact preview unavailable: " + p.Unavailable.Reason
	}
	topo := "unknown topology"
	if p.Node.Topology != nil {
		topo = string(p.Node.Topology.Topology) + " / device actions " + string(p.Node.Topology.DeviceActions)
	}
	lines := []string{
		"impact: node " + p.Node.Name + " (" + topo + ")",
		"workloads: " + itoa(p.Workloads.Total) + " total, " + itoa(p.Workloads.GPUHolders) + " GPU holders, " +
			itoa(p.Workloads.Namespaces) + " namespaces, " + itoa(p.Workloads.EligibleForCheckpoint) + " eligible for checkpoint",
	}
	if len(p.Workloads.Groups) > 0 {
		lines = append(lines, "groups: "+itoa(len(p.Workloads.Groups)))
	}
	if len(p.Warnings) > 0 {
		lines = append(lines, "warnings: "+strings.Join(p.Warnings, ", "))
	}
	return strings.Join(lines, "\n")
}

func devices(c topology.Classification) DevicesView {
	return DevicesView{
		Whole:              int(c.Evidence.WholeDevices),
		MIGInstances:       int(c.Evidence.MIGInstances),
		TimeSlicedReplicas: int(c.Evidence.SharedReplicas),
		Attribution:        string(c.Attribution),
	}
}

func contains(list []string, s string) bool {
	for _, have := range list {
		if have == s {
			return true
		}
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
