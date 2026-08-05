// Package providers blank-imports every cloud provider so their registrations
// run. A binary that builds cloud providers or gates on their capabilities
// imports this package for its side effects; adding a new cloud is a new
// provider package plus one import line here — never an edit to internal/cloud
// or any workflow/controller code.
package providers

import (
	_ "github.com/kubeneuron/kubeneuron/internal/cloud/aws"
)
