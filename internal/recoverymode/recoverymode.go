// SPDX-License-Identifier: Apache-2.0

// Package recoverymode defines who authorises a recovery step for the
// incidents a policy matches: nobody (dry-run), a named person (approval), or
// the policy itself, approved in advance by the person who enabled it
// (automatic). It is plain data shared by the configuration, the playbook
// engine and the controller.
package recoverymode

import (
	"fmt"
	"strings"
	"time"
)

// Mode is a policy's recovery mode.
type Mode string

const (
	// Unset is the mode of a policy that names none: Approval.
	Unset Mode = ""
	// DryRun simulates every step of the incidents the policy matches.
	DryRun Mode = "dry-run"
	// Approval parks every step that stops work for a named person.
	Approval Mode = "approval"
	// Automatic runs those steps under the policy's limits, without a person.
	Automatic Mode = "automatic"
)

// Effective resolves Unset to Approval, today's behaviour.
func (m Mode) Effective() Mode {
	if m == Unset {
		return Approval
	}
	return m
}

// Acknowledgement is the fixed sentence a policy must carry to enable
// automatic mode. It is short on purpose: the full warning (Warning) lives in
// the field description and the docs, so improving its wording never
// invalidates a policy a user already wrote.
const Acknowledgement = "I understand that automatic mode drains and replaces nodes without asking"

// Warning is the full text shown beside the setting (CRD description, Helm
// values and docs carry the same words).
const Warning = "Automatic mode acts without asking. For the node pools and fault classes you list here, KubeNeuron drains the node and restarts or replaces it through your cloud provider's API as soon as the conditions of your policy are met. Workloads on that node are stopped. A replacement instance may not be available, and the cloud provider bills instances according to its own terms. When replacement adds a node first, two instances run and are billed until the old one is removed, and your quota must allow the extra instance. You are responsible for the policy you set here and for its effect on your workloads. Start in dry-run, then in approval mode, before enabling automatic mode."

// RecycleWarning describes the optional restart through the cloud API.
const RecycleWarning = "A restart through the cloud API drains the node before it is tried, so it can interrupt the workload before a prepared replacement is ready; the add-first guarantee does not cover it."

// Strategy is how a node is replaced.
type Strategy string

const (
	// AddFirst brings up the new node, waits until it is ready with its
	// accelerators, and only then drains and removes the old one. Default.
	AddFirst Strategy = "add-first"
	// RemoveFirst removes the old node and waits for the pool to replace it.
	// Only when named explicitly.
	RemoveFirst Strategy = "remove-first"
)

// Effective resolves an empty strategy to AddFirst.
func (s Strategy) Effective() Strategy {
	if s == "" {
		return AddFirst
	}
	return s
}

// ValidateStrategy rejects an unknown strategy.
func ValidateStrategy(s Strategy) error {
	switch s {
	case "", AddFirst, RemoveFirst:
		return nil
	}
	return fmt.Errorf("unknown replacement strategy %q (want %q or %q)", s, AddFirst, RemoveFirst)
}

// DefaultMinUnhealthyNodes is the circuit breaker's absolute floor: it never
// trips on fewer unhealthy nodes than this, so the first fault in a two-node
// pool does not lock the pool.
const DefaultMinUnhealthyNodes = 2

// Settings is the advance approval an automatic policy carries: where it may
// act, how much at once, and who switched it on.
type Settings struct {
	// NodeSelector names the node pools by label. It must not be empty: an
	// absent selector is never read as "every node".
	NodeSelector map[string]string `yaml:"node_selector" json:"nodeSelector"`
	// PoolLabel is the node label that identifies a pool, for the per-pool
	// limit and the circuit breaker. Empty treats all selected nodes as one
	// pool.
	PoolLabel string `yaml:"pool_label,omitempty" json:"poolLabel,omitempty"`
	// MaxConcurrentNodes bounds the nodes this policy recovers at once.
	MaxConcurrentNodes int `yaml:"max_concurrent_nodes" json:"maxConcurrentNodes"`
	// MaxConcurrentNodesPerPool bounds them per pool; zero means the total
	// bound only.
	MaxConcurrentNodesPerPool int `yaml:"max_concurrent_nodes_per_pool,omitempty" json:"maxConcurrentNodesPerPool,omitempty"`
	// CheckpointWait is how long a step that stops work waits for the job's
	// checkpoint (through checkpoint coordination) before it proceeds.
	CheckpointWait time.Duration `yaml:"checkpoint_wait,omitempty" json:"checkpointWait,omitempty"`
	// NodeCooldown is the minimum time between two automatic recoveries of
	// one node, so a flapping node is not replaced again and again.
	NodeCooldown time.Duration `yaml:"node_cooldown" json:"nodeCooldown"`
	// MaxUnhealthyPercent is the circuit breaker: when more than this share
	// of a pool's nodes have an open incident or are not Ready, the policy
	// falls back to approval, and stays there until the policy is changed.
	MaxUnhealthyPercent int `yaml:"max_unhealthy_percent" json:"maxUnhealthyPercent"`
	// MinUnhealthyNodes is the breaker's absolute floor (zero means
	// DefaultMinUnhealthyNodes).
	MinUnhealthyNodes int `yaml:"min_unhealthy_nodes,omitempty" json:"minUnhealthyNodes,omitempty"`
	// EnabledBy and EnabledAt are what the policy declares about who switched
	// automatic mode on and when. KubeNeuron cannot verify them; the
	// Kubernetes API audit log records who actually changed the object.
	EnabledBy string    `yaml:"enabled_by" json:"enabledBy"`
	EnabledAt time.Time `yaml:"enabled_at" json:"enabledAt"`
	// Generation is the policy object's generation when compiled (zero for a
	// file configuration). A tripped circuit breaker holds for one
	// generation: changing the policy is how a person returns it to
	// automatic.
	Generation int64 `yaml:"generation,omitempty" json:"generation,omitempty"`
	// Acknowledgement must read exactly Acknowledgement.
	Acknowledgement string `yaml:"acknowledgement" json:"acknowledgement"`
}

// MinUnhealthy is the effective breaker floor.
func (a *Settings) MinUnhealthy() int {
	if a.MinUnhealthyNodes > 0 {
		return a.MinUnhealthyNodes
	}
	return DefaultMinUnhealthyNodes
}

// TripKey identifies the policy revision a breaker trip belongs to.
func (a *Settings) TripKey() string {
	return fmt.Sprintf("generation=%d enabled_at=%s", a.Generation, a.EnabledAt.UTC().Format(time.RFC3339))
}

// Validate checks a policy's mode and its automatic block together.
func Validate(mode Mode, auto *Settings) error {
	switch mode {
	case Unset, DryRun, Approval:
		if auto != nil {
			return fmt.Errorf("automatic settings are only allowed with mode %q", Automatic)
		}
		return nil
	case Automatic:
	default:
		return fmt.Errorf("unknown recovery mode %q (want %q, %q or %q)", mode, DryRun, Approval, Automatic)
	}
	if auto == nil {
		return fmt.Errorf("mode %q requires the automatic settings", Automatic)
	}
	if len(auto.NodeSelector) == 0 {
		return fmt.Errorf("automatic: node_selector must name the node pools; an empty selector is not \"every node\"")
	}
	for k, v := range auto.NodeSelector {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("automatic: node_selector keys and values must not be empty")
		}
	}
	if auto.MaxConcurrentNodes < 1 {
		return fmt.Errorf("automatic: max_concurrent_nodes must be at least 1")
	}
	if auto.MaxConcurrentNodesPerPool < 0 || auto.MaxConcurrentNodesPerPool > auto.MaxConcurrentNodes {
		return fmt.Errorf("automatic: max_concurrent_nodes_per_pool must be between 0 and max_concurrent_nodes")
	}
	if auto.CheckpointWait < 0 {
		return fmt.Errorf("automatic: checkpoint_wait must not be negative")
	}
	if auto.NodeCooldown <= 0 {
		return fmt.Errorf("automatic: node_cooldown must be positive")
	}
	if auto.MaxUnhealthyPercent < 1 || auto.MaxUnhealthyPercent > 100 {
		return fmt.Errorf("automatic: max_unhealthy_percent must be between 1 and 100")
	}
	if auto.MinUnhealthyNodes < 0 {
		return fmt.Errorf("automatic: min_unhealthy_nodes must not be negative")
	}
	if strings.TrimSpace(auto.EnabledBy) == "" {
		return fmt.Errorf("automatic: enabled_by must name who enabled automatic mode")
	}
	if auto.EnabledAt.IsZero() {
		return fmt.Errorf("automatic: enabled_at is required")
	}
	if auto.Acknowledgement != Acknowledgement {
		return fmt.Errorf("automatic: acknowledgement must read exactly %q", Acknowledgement)
	}
	return nil
}

// SelectsNode reports whether labels match every selector entry.
func (a *Settings) SelectsNode(labels map[string]string) bool {
	if a == nil || len(a.NodeSelector) == 0 || labels == nil {
		return false
	}
	for k, v := range a.NodeSelector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Pool returns the pool a node belongs to under this policy.
func (a *Settings) Pool(labels map[string]string) string {
	if a == nil || a.PoolLabel == "" {
		return ""
	}
	return labels[a.PoolLabel]
}
