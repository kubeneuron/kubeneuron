// Package aws recycles or replaces the EC2 instance behind a GPU node.
//
// It exists because a hardware GPU reset is impossible on a virtualized EC2
// instance: the hypervisor withholds the PCI reset from the guest (measured on
// g4dn). The cloud-native equivalent is to reinitialize the instance itself —
// stop/start tears down and re-establishes the GPU passthrough, and terminate
// hands the node back to the autoscaler for a clean replacement.
//
// This is deliberately a controller-side concern, never an agent action. The
// agent dies the moment its instance stops, so it could never issue the Start
// that follows. Only a process that outlives the node — the controller — can
// drive stop/start.
package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/kubeneuron/kubeneuron/internal/cloud"
)

// EC2API is the subset of the EC2 client this package uses. It is an interface
// so tests substitute a fake and no test ever reaches AWS.
type EC2API interface {
	StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// Recycler drives EC2 stop/start and terminate against a bounded EC2 client.
type Recycler struct {
	api EC2API
	// pollInterval paces the wait for an instance to reach a target state.
	pollInterval time.Duration
}

// New builds a Recycler from the ambient AWS configuration (IRSA in EKS). The
// controller's ServiceAccount must carry an IAM role scoped to
// ec2:StopInstances/StartInstances/TerminateInstances/DescribeInstances on the
// cluster's own instances.
func New(ctx context.Context, region string) (*Recycler, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: loading configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("aws: no region configured; set AWS_REGION or the node's region label")
	}
	return &Recycler{api: ec2.NewFromConfig(cfg), pollInterval: 10 * time.Second}, nil
}

// NewWithAPI builds a Recycler over an explicit client, for tests.
func NewWithAPI(api EC2API) *Recycler {
	return &Recycler{api: api, pollInterval: time.Millisecond}
}

// autoscalingGroupTag is the tag EC2 stamps on every autoscaling-group member.
const autoscalingGroupTag = "aws:autoscaling:groupName"

// CheckRecycle reports whether stop/start can work for this exact instance.
//
// The capability is declared provider-wide, but viability is per-instance: a
// managed-node-group (or any ASG) member that stops fails its group's health
// check, and the group terminates and replaces it in the middle of the
// "recycle" — which the controller previously discovered only when the node
// never rejoined and the step timed out. The ASG membership tag is the
// authoritative, credential-cheap signal, so the verdict is available at
// admission time, before a human is asked to approve a step that cannot work.
func (r *Recycler) CheckRecycle(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("aws: instance ID is required")
	}
	out, err := r.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("aws: describing %s: %w", instanceID, err)
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if awssdk.ToString(instance.InstanceId) != instanceID {
				continue
			}
			for _, tag := range instance.Tags {
				if awssdk.ToString(tag.Key) == autoscalingGroupTag {
					return fmt.Errorf("%w: instance %s belongs to autoscaling group %q, whose health check terminates a stopped member mid-recycle; use ReplaceNode",
						cloud.ErrRecycleNotViable, instanceID, awssdk.ToString(tag.Value))
				}
			}
			return nil
		}
	}
	return fmt.Errorf("aws: instance %s no longer exists", instanceID)
}

// Recycle stops the instance and starts it again, waiting for each transition.
//
// Stop/start is not a reboot: it detaches the instance from its physical host
// and reattaches it, so the GPU passthrough is torn down and re-established
// from scratch. The instance ID, EBS volumes, and private IP survive; the GPU
// comes up clean. The context deadline bounds the whole operation, so a step
// timeout applies end to end.
func (r *Recycler) Recycle(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("aws: instance ID is required")
	}
	if _, err := r.api.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return fmt.Errorf("aws: stopping %s: %w", instanceID, err)
	}
	// From here the instance is stopping or stopped, and the ONLY code in this
	// program that starts one again is four lines below. Returning between the
	// two leaves a machine powered off with nothing that will ever bring it
	// back — and the operator approved "stop and start", not "stop".
	//
	// Two ordinary paths reached that: EC2 throttling a single DescribeInstances
	// (waitForState used to return on the first error), and the step deadline
	// expiring during the stop-wait. Neither is exotic, and the error the human
	// eventually read said "waiting for i-… to stop failed" without mentioning
	// that the machine was off.
	//
	// So the start is attempted whatever the stop-wait concluded, on a deadline
	// detached from the caller's — the caller's may be the thing that just
	// expired — and both errors are reported together.
	stopErr := r.waitForState(ctx, instanceID, ec2types.InstanceStateNameStopped)
	if stopErr != nil {
		stopErr = fmt.Errorf("aws: waiting for %s to stop: %w", instanceID, stopErr)
	}
	startCtx := ctx
	if stopErr != nil {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), recoveryStartTimeout)
		defer cancel()
	}
	if _, err := r.api.StartInstances(startCtx, &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		err = fmt.Errorf("aws: INSTANCE %s IS STOPPED AND WAS NOT RESTARTED: starting it failed: %w", instanceID, err)
		if stopErr != nil {
			return errors.Join(stopErr, err)
		}
		return err
	}
	if stopErr != nil {
		// The start was issued, so the machine is coming back; the recycle
		// still failed and the caller must not treat it as done.
		return errors.Join(stopErr, fmt.Errorf("aws: a start was issued for %s to avoid leaving it stopped", instanceID))
	}
	if err := r.waitForState(ctx, instanceID, ec2types.InstanceStateNameRunning); err != nil {
		return fmt.Errorf("aws: waiting for %s to run: %w", instanceID, err)
	}
	return nil
}

// Replace terminates the instance. The node group's autoscaler (managed node
// group ASG or Karpenter) then provisions a fresh node. This does not wait for
// the replacement: the terminated node's incident is closed as replaced once
// the node object disappears, which the controller already handles.
func (r *Recycler) Replace(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("aws: instance ID is required")
	}
	if _, err := r.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return fmt.Errorf("aws: terminating %s: %w", instanceID, err)
	}
	return nil
}

// recoveryStartTimeout bounds the start issued after a failed stop-wait. It is
// deliberately short: the call either reaches EC2 or it does not, and the
// caller is already past its own deadline.
const recoveryStartTimeout = 30 * time.Second

// describeErrorBudget is how many consecutive DescribeInstances failures a wait
// tolerates before giving up. EC2 throttles — RequestLimitExceeded is routine —
// and returning on the first error meant one throttled call could strand a
// stopped instance.
const describeErrorBudget = 5

// waitForState polls until the instance reaches the target state, the context
// is done, or the instance vanishes (which only a target of terminated treats
// as success).
func (r *Recycler) waitForState(ctx context.Context, instanceID string, target ec2types.InstanceStateName) error {
	describeErrors := 0
	for {
		state, found, err := r.instanceState(ctx, instanceID)
		if err != nil {
			describeErrors++
			if describeErrors >= describeErrorBudget {
				return fmt.Errorf("describing %s failed %d times in a row: %w", instanceID, describeErrors, err)
			}
			timer := time.NewTimer(r.pollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("instance %s: %w (last describe error: %v)", instanceID, ctx.Err(), err)
			case <-timer.C:
			}
			continue
		}
		describeErrors = 0
		if !found {
			return fmt.Errorf("instance %s no longer exists", instanceID)
		}
		if state == target {
			return nil
		}
		timer := time.NewTimer(r.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("instance %s did not reach %q (last state %q): %w", instanceID, target, state, ctx.Err())
		case <-timer.C:
		}
	}
}

func (r *Recycler) instanceState(ctx context.Context, instanceID string) (ec2types.InstanceStateName, bool, error) {
	out, err := r.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", false, fmt.Errorf("aws: describing %s: %w", instanceID, err)
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if awssdk.ToString(instance.InstanceId) != instanceID {
				continue
			}
			if instance.State == nil {
				return "", true, fmt.Errorf("aws: %s has no state", instanceID)
			}
			return instance.State.Name, true, nil
		}
	}
	return "", false, nil
}
