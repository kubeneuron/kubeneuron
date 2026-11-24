// SPDX-License-Identifier: Apache-2.0

package capacityreason

import (
	"errors"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// Failure retains provider diagnostics for logging. Its rendered error is safe
// for step results and the capacity sentinel remains usable with errors.Is.
type Failure struct {
	Reason Reason
	Cause  error
}

func (f *Failure) Error() string {
	return cloud.ErrReplacementCapacity.Error() + ": " + string(Known(string(f.Reason)))
}
func (f *Failure) Is(target error) bool { return target == cloud.ErrReplacementCapacity }
func (f *Failure) Unwrap() error        { return f.Cause }

func FromError(err error) Reason {
	var failure *Failure
	if errors.As(err, &failure) {
		return Known(string(failure.Reason))
	}
	return Unspecified
}

// Known admits only documented reasons, including when reading a stored record.
func Known(value string) Reason {
	switch r := Reason(value); r {
	case Unavailable, Quota, NetworkAddresses, NotAvailableToSubscription:
		return r
	default:
		return Unspecified
	}
}
