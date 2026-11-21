// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

type permissionEC2 struct {
	fakeEC2
	t        *testing.T
	response error
	calls    int
}

func (f *permissionEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.t.Helper()
	if !sdk.ToBool(in.DryRun) || !slices.Equal(in.InstanceIds, []string{"i-pinned"}) {
		f.t.Fatal("expected dry run for the single pinned instance")
	}
	f.calls++
	return nil, f.response
}
func TestReplacePermission(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		response error
		allowed  bool
		calls    int
	}{
		{"authorized", "i-pinned", fmt.Errorf("request: %w", &smithy.GenericAPIError{Code: "DryRunOperation"}), true, 1},
		{"denied", "i-pinned", &smithy.GenericAPIError{Code: "UnauthorizedOperation"}, false, 1},
		{"throttled", "i-pinned", &smithy.GenericAPIError{Code: "RequestLimitExceeded"}, false, 1},
		{"ambiguous success", "i-pinned", nil, false, 1},
		{"unstructured error", "i-pinned", errors.New("DryRunOperation"), false, 1},
		{"missing pin", "", nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &permissionEC2{t: t, response: tc.response}
			err := NewWithAPI(f).CheckReplacePermission(t.Context(), tc.id)
			var apiErr smithy.APIError
			if !tc.allowed && errors.As(tc.response, &apiErr) {
				want := "EC2 TerminateInstances DryRun failed"
				if apiErr.ErrorCode() == "UnauthorizedOperation" {
					want = "ec2:TerminateInstances denied"
				}
				if !errors.Is(err, tc.response) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), apiErr.ErrorCode()) {
					t.Fatalf("expected diagnostic %q with AWS cause, got %v", want, err)
				}
			}
			var hold *cloud.RecoveryHold
			if !tc.allowed && tc.response != nil && (!errors.As(err, &hold) || strings.Contains(hold.Message(), "i-pinned") || strings.Contains(hold.Message(), "DryRun failed")) {
				t.Fatalf("expected a hold without the raw error, got %v", err)
			}
			if tc.name == "denied" && hold.Message() != "The controller's IAM role is not allowed to call ec2:TerminateInstances on the node's instance. Allow ec2:TerminateInstances in the controller's IAM role (see docs/operations.md). KubeNeuron checks again every minute." {
				t.Fatalf("denied message %q", hold.Message())
			}
			if (err == nil) != tc.allowed || f.calls != tc.calls {
				t.Fatalf("permission err=%v calls=%d", err, f.calls)
			}
		})
	}
}
