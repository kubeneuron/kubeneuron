// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"errors"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestSuccessorCandidate(t *testing.T) {
	s := cloud.Successor{Instance: "i-old", Pool: "workers", State: map[string]string{"zone": "zone-a", "members": "i-old,i-peer", "before": "2", "asg_arn": "asg-1"}}
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{"single addition", []string{"i-new", "i-peer", "i-old"}, "i-new"},
		{"pinned replaced", []string{"i-new", "i-peer", "i-other"}, ""},
		{"peer replaced", []string{"i-old", "i-new", "i-other"}, ""},
		{"duplicate old member", []string{"i-old", "i-old", "i-new"}, ""},
		{"missing launch", []string{"i-old", "i-peer"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := at.AutoScalingGroup{AutoScalingGroupName: sdk.String(s.Pool), AutoScalingGroupARN: sdk.String("asg-1"), DesiredCapacity: sdk.Int32(3), AvailabilityZones: []string{"zone-a"}}
			for _, id := range tc.ids {
				g.Instances = append(g.Instances, at.Instance{InstanceId: sdk.String(id), AvailabilityZone: sdk.String("zone-a"), LifecycleState: at.LifecycleStateInService})
			}
			got, err := successorCandidate(s, g)
			if got != tc.want || (tc.want == "" && !errors.Is(err, cloud.ErrNeedsOperator)) || (tc.want != "" && err != nil) {
				t.Fatalf("candidate=%q err=%v", got, err)
			}
		})
	}
}
