package checkpoint

import (
	"regexp"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The one eligibility function (docs/workload-safe-recovery-design.md, S6).
//
// The Phase 1 request, the advisory, the group stamp and the impact preview
// all decide "is this workload one we coordinate with" through Evaluate, so
// the four surfaces cannot disagree about who is eligible. The codes extend
// Ineligibility: the four Phase 1 codes keep their spelling (they label audit
// rows since v0.6.0), and the ones below are new.

const (
	// IneligibleDrainExcluded: the disruption would not touch the workload
	// (a finished, mirror, DaemonSet or, on a non-forced drain, unmanaged
	// Pod), so there is nothing to coordinate.
	IneligibleDrainExcluded Ineligibility = "drain-excluded"
	// IneligibleNotGPUHolder: an eviction of GPU workloads would not touch
	// the workload because it holds no GPU.
	IneligibleNotGPUHolder Ineligibility = "not-gpu-holder"
	// IneligibleGroupInvalid: the workload names a checkpoint group that is
	// not a DNS label; the group is ignored, the workload is still eligible
	// on its own (this code is informational on a preview, never a refusal).
	IneligibleGroupInvalid Ineligibility = "group-invalid"
	// IneligibleGroupTooLarge: the workload's group exceeds the policy's
	// maximum, so its peers were not stamped.
	IneligibleGroupTooLarge Ineligibility = "group-too-large"
	// IneligibleBudgetExhausted: the round's budget ran out before the
	// workload was reached.
	IneligibleBudgetExhausted Ineligibility = "budget-exhausted"
	// IneligiblePlatformUnsupported: the platform cannot stamp annotations.
	IneligiblePlatformUnsupported Ineligibility = "platform-unsupported"
	// IneligibleNoDisruption: no coordinating disruption (drain or GPU
	// workload eviction) lies downstream of the pending step, so there is
	// nothing to coordinate ahead of; a preview of such a round advertises
	// no eligible workload, whatever the policy would say.
	IneligibleNoDisruption Ineligibility = "no-disruption"
)

// IneligibilityCodes lists every code a preview may render, for the closed
// set the API documents. Order is stable.
func IneligibilityCodes() []Ineligibility {
	return []Ineligibility{
		IneligiblePolicyDisabled, IneligibleClassSkipped, IneligibleNamespace, IneligibleNotOptedIn,
		IneligibleDrainExcluded, IneligibleNotGPUHolder, IneligibleGroupInvalid, IneligibleGroupTooLarge,
		IneligibleBudgetExhausted, IneligiblePlatformUnsupported, IneligibleNoDisruption,
	}
}

// Candidate is the platform-agnostic view Evaluate decides about: the
// checkpoint view of the workload plus the two facts the disruption itself
// decides — whether a drain would evict it and whether it holds a GPU.
type Candidate struct {
	Workload Workload
	// DrainEligible is platform.Workload.DrainEligible(force) for the step.
	DrainEligible bool
	// UsesGPU is whether the workload holds an accelerator.
	UsesGPU bool
}

// Evaluate decides whether a candidate is coordinated with for an incident
// of the given class ahead of the given disruption ("drain" or
// "evict_gpu_workload"). Empty means eligible. The disruption's own reach is
// checked first — a workload the step would not touch is not asked, whatever
// the policy says — then the policy in its usual order. Any other op,
// including none, coordinates nothing: there is no disruption to be
// eligible for.
func Evaluate(c Candidate, policy Policy, class types.ProblemClass, op string) Ineligibility {
	switch op {
	case "drain":
		if !c.DrainEligible {
			return IneligibleDrainExcluded
		}
	case "evict_gpu_workload":
		if !c.UsesGPU {
			return IneligibleNotGPUHolder
		}
	default:
		return IneligibleNoDisruption
	}
	return policy.Eligibility(c.Workload, class)
}

// AnnotationGroup is the Pod annotation naming a checkpoint group (S3). It
// is read here because the group is part of eligibility rendering; the
// group semantics live in group.go.
const AnnotationGroup = "kubeneuron.io/checkpoint-group"

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Group returns the workload's declared checkpoint group when it is a valid
// DNS label (at most 63 characters), and whether the annotation is present
// but invalid. An absent annotation is ("", false).
func (w Workload) Group() (name string, invalid bool) {
	raw, ok := w.Annotations[AnnotationGroup]
	if !ok {
		return "", false
	}
	if len(raw) == 0 || len(raw) > 63 || !dnsLabel.MatchString(raw) {
		return "", true
	}
	return raw, false
}
