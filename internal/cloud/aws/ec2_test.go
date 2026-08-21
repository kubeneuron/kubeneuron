package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/kubeneuron/kubeneuron/internal/cloud"
)

// fakeEC2 scripts a sequence of DescribeInstances states so a recycle can be
// driven to completion without touching AWS.
type fakeEC2 struct {
	stopCalls, startCalls, terminateCalls int
	describeCalls                         int
	states                                []ec2types.InstanceStateName
	instanceID                            string
	missing                               bool
	stopErr                               error
	asgName                               string
	// describeErr fails every DescribeInstances; describeErrFirst fails only
	// the first N, modelling EC2 throttling that clears.
	describeErr      error
	describeErrFirst int
}

func (f *fakeEC2) StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	f.stopCalls++
	return &ec2.StopInstancesOutput{}, f.stopErr
}
func (f *fakeEC2) StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.startCalls++
	return &ec2.StartInstancesOutput{}, nil
}
func (f *fakeEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.terminateCalls++
	return &ec2.TerminateInstancesOutput{}, nil
}
func (f *fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	i := f.describeCalls
	f.describeCalls++
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if i < f.describeErrFirst {
		return nil, errors.New("RequestLimitExceeded")
	}
	if f.missing {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	state := f.states[len(f.states)-1]
	if i < len(f.states) {
		state = f.states[i]
	}
	instance := ec2types.Instance{
		InstanceId: aws.String(f.instanceID),
		State:      &ec2types.InstanceState{Name: state},
	}
	if f.asgName != "" {
		instance.Tags = []ec2types.Tag{{Key: aws.String("aws:autoscaling:groupName"), Value: aws.String(f.asgName)}}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{
		Instances: []ec2types.Instance{instance},
	}}}, nil
}

// Recycle is a full stop -> wait stopped -> start -> wait running cycle. That
// sequence, not a reboot, is what tears down and re-establishes the GPU
// passthrough on a virtualized instance.
func TestRecycleStopsThenStarts(t *testing.T) {
	f := &fakeEC2{
		instanceID: "i-abc",
		states: []ec2types.InstanceStateName{
			ec2types.InstanceStateNameStopping,
			ec2types.InstanceStateNameStopped, // stop completes
			ec2types.InstanceStateNamePending,
			ec2types.InstanceStateNameRunning, // start completes
		},
	}
	if err := NewWithAPI(f).Recycle(context.Background(), "i-abc"); err != nil {
		t.Fatal(err)
	}
	if f.stopCalls != 1 || f.startCalls != 1 {
		t.Fatalf("stop=%d start=%d, want one each", f.stopCalls, f.startCalls)
	}
}

func TestReplaceTerminates(t *testing.T) {
	f := &fakeEC2{instanceID: "i-abc"}
	if err := NewWithAPI(f).Replace(context.Background(), "i-abc"); err != nil {
		t.Fatal(err)
	}
	if f.terminateCalls != 1 {
		t.Fatalf("terminate=%d, want 1", f.terminateCalls)
	}
}

func TestRecycleFailsWhenStopIsRejected(t *testing.T) {
	f := &fakeEC2{instanceID: "i-abc", stopErr: errors.New("AccessDenied")}
	if err := NewWithAPI(f).Recycle(context.Background(), "i-abc"); err == nil {
		t.Fatal("a rejected StopInstances must fail the recycle")
	}
	if f.startCalls != 0 {
		t.Fatal("must not start an instance that never stopped")
	}
}

func TestRecycleFailsIfInstanceVanishes(t *testing.T) {
	f := &fakeEC2{instanceID: "i-abc", missing: true}
	if err := NewWithAPI(f).Recycle(context.Background(), "i-abc"); err == nil {
		t.Fatal("an instance that no longer exists must not be reported recycled")
	}
}

func TestRecycleRequiresInstanceID(t *testing.T) {
	if err := NewWithAPI(&fakeEC2{}).Recycle(context.Background(), ""); err == nil {
		t.Fatal("empty instance ID must be rejected")
	}
}

// N3: capabilities are provider-scoped but viability is instance-scoped. An
// autoscaling-group member cannot be stop/started — the group's health check
// terminates a stopped member mid-recycle — and that verdict must be
// available BEFORE any stop is issued, as a typed ErrRecycleNotViable.
func TestCheckRecycleRefusesAutoscalingGroupMember(t *testing.T) {
	api := &fakeEC2{instanceID: "i-1", states: []ec2types.InstanceStateName{ec2types.InstanceStateNameRunning}, asgName: "gpu-mng"}
	r := NewWithAPI(api)

	err := r.CheckRecycle(context.Background(), "i-1")
	if !errors.Is(err, cloud.ErrRecycleNotViable) {
		t.Fatalf("CheckRecycle on an ASG member = %v, want ErrRecycleNotViable", err)
	}
	if api.stopCalls != 0 {
		t.Fatal("a viability check must never stop anything")
	}
}

func TestCheckRecycleAllowsPlainInstance(t *testing.T) {
	api := &fakeEC2{instanceID: "i-1", states: []ec2types.InstanceStateName{ec2types.InstanceStateNameRunning}}
	if err := NewWithAPI(api).CheckRecycle(context.Background(), "i-1"); err != nil {
		t.Fatalf("CheckRecycle on a plain instance = %v, want nil", err)
	}
}

func TestCheckRecycleMissingInstanceIsNotAViabilityVerdict(t *testing.T) {
	api := &fakeEC2{instanceID: "i-1", missing: true}
	err := NewWithAPI(api).CheckRecycle(context.Background(), "i-1")
	if err == nil || errors.Is(err, cloud.ErrRecycleNotViable) {
		t.Fatalf("missing instance = %v, want a plain error, not a definitive non-viability verdict", err)
	}
}

// TestRecycleAlwaysAttemptsTheStart covers the worst outcome this action has:
// a machine the operator approved "stop and start" for, left stopped.
//
// StartInstances is issued from exactly one place in this program, so returning
// between the stop and it means nothing will ever bring that instance back. The
// two ways to get there are ordinary: EC2 throttling one DescribeInstances, or
// the step deadline expiring during the stop-wait.
func TestRecycleAlwaysAttemptsTheStart(t *testing.T) {
	t.Run("the stop-wait never succeeds", func(t *testing.T) {
		f := &fakeEC2{instanceID: "i-1", describeErr: errors.New("RequestLimitExceeded")}
		r := &Recycler{api: f, pollInterval: time.Millisecond}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		if err := r.Recycle(ctx, "i-1"); err == nil {
			t.Fatal("a failed stop-wait reported success")
		}
		if f.startCalls == 0 {
			t.Fatal("the instance was stopped and no start was ever issued; nothing else in this " +
				"program starts one, so the machine stays powered off indefinitely")
		}
	})

	t.Run("a transient throttle does not strand it", func(t *testing.T) {
		f := &fakeEC2{
			instanceID:       "i-2",
			describeErrFirst: 2,
			// The first two describes error, and describeCalls advances on
			// those too, so the state list is offset by two.
			states: []ec2types.InstanceStateName{
				ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopping,
				ec2types.InstanceStateNameStopped, ec2types.InstanceStateNameRunning,
			},
		}
		r := &Recycler{api: f, pollInterval: time.Millisecond}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := r.Recycle(ctx, "i-2"); err != nil {
			t.Fatalf("two throttled describes failed the whole recycle: %v", err)
		}
		if f.startCalls == 0 {
			t.Fatal("no start was issued")
		}
	})
}
