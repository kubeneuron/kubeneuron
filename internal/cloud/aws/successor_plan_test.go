// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"errors"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	at "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

func TestPlanSuccessor(t *testing.T) {
	group := at.AutoScalingGroup{AutoScalingGroupName: sdk.String("workers"), AvailabilityZones: []string{"zone-a"}, DesiredCapacity: sdk.Int32(2), MaxSize: sdk.Int32(3)}
	for _, id := range []string{"i-old", "i-a"} {
		group.Instances = append(group.Instances, at.Instance{InstanceId: sdk.String(id), AvailabilityZone: sdk.String("zone-a"), LifecycleState: at.LifecycleStateInService})
	}
	s := cloud.Successor{Instance: "i-old", Operation: "op"}
	plan, err := planSuccessor(s, group)
	if err != nil || plan.State["members"] != "i-a,i-old" || plan.State["before"] != "2" || plan.Phase != cloud.SuccessorPlanned {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	group.AvailabilityZones = append(group.AvailabilityZones, "zone-b")
	if _, err := planSuccessor(s, group); !errors.Is(err, cloud.ErrAddFirstUnavailable) {
		t.Fatal(err)
	}
	group.AvailabilityZones = group.AvailabilityZones[:1]
	group.MaxSize = sdk.Int32(2)
	if _, err := planSuccessor(s, group); !errors.Is(err, cloud.ErrAddFirstUnavailable) {
		t.Fatal(err)
	}
}
