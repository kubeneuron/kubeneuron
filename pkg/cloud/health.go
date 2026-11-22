// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"time"
)

// HealthPoint is one sample of an instance's accelerator health as the
// cloud's monitoring reports it. Only these facts are kept.
type HealthPoint struct {
	// At is the end of the sample's interval.
	At time.Time `json:"at"`
	// Status is the health status the cloud reports, as it spells it.
	Status string `json:"status"`
	// Category is the cloud's cause of an unhealthy status, when it gives one.
	Category string `json:"category,omitempty"`
}

// Reasons of a HealthReadError.
const (
	HealthReadPermissionDenied = "permission-denied"
	HealthReadAPIError         = "api-error"
)

// HealthReadError is a failed or incomplete health read. Reason is one of
// the HealthRead constants.
type HealthReadError struct {
	Reason string
	Err    error
}

func (e *HealthReadError) Error() string { return e.Reason + ": " + e.Err.Error() }

func (e *HealthReadError) Unwrap() error { return e.Err }

// AcceleratorHealthReader is implemented by a provider that can read the
// accelerator health of instances from the cloud's monitoring, when the
// operator turned that read on.
type AcceleratorHealthReader interface {
	// AcceleratorHealthEnabled reports whether the read is turned on.
	AcceleratorHealthEnabled() bool
	// AcceleratorHealth returns the recent points of each instance, keyed by
	// instance ID as Provider.InstanceID returns it, oldest first. An
	// instance the cloud holds no point for is left out. A read that fails,
	// or returns only part of the answer, is a *HealthReadError and no
	// points: part of the answer is not the whole of it.
	AcceleratorHealth(ctx context.Context, instanceIDs []string) (map[string][]HealthPoint, error)
}
