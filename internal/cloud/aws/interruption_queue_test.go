// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/kubeneuron/kubeneuron/internal/cloud/cloudtest"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// fakeSQS is a queue: a received message is hidden until redeliver, and a
// delete needs the receipt handle of the message's latest delivery.
type fakeSQS struct {
	next      int
	messages  []*fakeMessage
	deleteErr error
	deleted   []string
	waits     []int32
}

type fakeMessage struct {
	id, body, receipt string
	hidden            bool
	gone              bool
	deliveries        int
}

func (f *fakeSQS) send(body string) {
	f.next++
	f.messages = append(f.messages, &fakeMessage{id: fmt.Sprintf("m-%d", f.next), body: body})
}

// redeliver makes every message not deleted visible again, as the
// visibility timeout does.
func (f *fakeSQS) redeliver() {
	for _, m := range f.messages {
		m.hidden = false
	}
}

func (f *fakeSQS) left() int {
	n := 0
	for _, m := range f.messages {
		if !m.gone {
			n++
		}
	}
	return n
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.waits = append(f.waits, in.WaitTimeSeconds)
	out := &sqs.ReceiveMessageOutput{}
	for _, m := range f.messages {
		if m.gone || m.hidden || len(out.Messages) == int(in.MaxNumberOfMessages) {
			continue
		}
		m.hidden = true
		m.deliveries++
		m.receipt = fmt.Sprintf("%s/%d", m.id, m.deliveries)
		out.Messages = append(out.Messages, sqstypes.Message{MessageId: awssdk.String(m.id), ReceiptHandle: awssdk.String(m.receipt), Body: awssdk.String(m.body)})
	}
	return out, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	for _, m := range f.messages {
		if m.receipt == awssdk.ToString(in.ReceiptHandle) && !m.gone {
			m.gone = true
			f.deleted = append(f.deleted, m.id)
			return &sqs.DeleteMessageOutput{}, nil
		}
	}
	return nil, errors.New("ReceiptHandleIsInvalid")
}

// Event bodies after the examples in the EC2 and AWS Health documentation.
func spotEvent(id, instance string) string {
	return fmt.Sprintf(`{"version":"0","id":%q,"detail-type":"EC2 Spot Instance Interruption Warning","source":"aws.ec2","account":"123456789012","time":"2026-11-18T10:00:00Z","region":"us-east-1","resources":["arn:aws:ec2:us-east-1:123456789012:instance/%s"],"detail":{"instance-id":%q,"instance-action":"terminate"}}`, id, instance, instance)
}

func rebalanceEvent(id, instance string) string {
	return fmt.Sprintf(`{"version":"0","id":%q,"detail-type":"EC2 Instance Rebalance Recommendation","source":"aws.ec2","account":"123456789012","time":"2026-11-18T10:00:00Z","region":"us-east-1","resources":[],"detail":{"instance-id":%q}}`, id, instance)
}

func stateEvent(id, instance, state string) string {
	return fmt.Sprintf(`{"version":"0","id":%q,"detail-type":"EC2 Instance State-change Notification","source":"aws.ec2","account":"123456789012","time":"2026-11-18T10:05:00Z","region":"us-east-1","resources":[],"detail":{"instance-id":%q,"state":%q}}`, id, instance, state)
}

func healthEvent(id, status string, entities ...string) string {
	list := ""
	for i, e := range entities {
		if i > 0 {
			list += ","
		}
		list += fmt.Sprintf(`{"entityValue":%q}`, e)
	}
	return fmt.Sprintf(`{"version":"0","id":%q,"detail-type":"AWS Health Event","source":"aws.health","account":"123456789012","time":"2026-11-18T10:00:00Z","region":"us-east-1","resources":[],"detail":{"eventArn":"arn:aws:health:us-east-1::event/EC2/AWS_EC2_INSTANCE_RETIREMENT_SCHEDULED/AWS_EC2_INSTANCE_RETIREMENT_SCHEDULED_1","service":"EC2","eventTypeCode":"AWS_EC2_INSTANCE_RETIREMENT_SCHEDULED","eventTypeCategory":"scheduledChange","startTime":"Wed, 25 Nov 2026 15:10:09 GMT","statusCode":%q,"affectedEntities":[%s],"eventDescription":[{"language":"en_US","latestDescription":"free text"}]}}`, id, status, list)
}

func TestParseInterruptionEvents(t *testing.T) {
	const arn = "aws-eventbridge:arn:aws:health:us-east-1::event/EC2/AWS_EC2_INSTANCE_RETIREMENT_SCHEDULED/AWS_EC2_INSTANCE_RETIREMENT_SCHEDULED_1/"
	start := time.Date(2026, 11, 25, 15, 10, 9, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		body    string
		want    []cloud.Interruption
		outcome string
	}{
		{"spot", spotEvent("e1", "i-0a"), []cloud.Interruption{{InstanceID: "i-0a", Kind: cloud.InterruptionSpotReclaim,
			Deadline: time.Date(2026, 11, 18, 10, 2, 0, 0, time.UTC), Source: SourceQueueSpot, Key: "aws-eventbridge:e1", Once: true}}, ""},
		{"rebalance", rebalanceEvent("e2", "i-0a"), []cloud.Interruption{{InstanceID: "i-0a", Kind: cloud.InterruptionRebalance,
			Source: SourceQueueRebalance, Key: "aws-eventbridge:e2", Once: true}}, ""},
		{"health by ARN and by ID", healthEvent("e3", "upcoming", "arn:aws:ec2:us-east-1:123456789012:instance/i-0a", "i-0b"), []cloud.Interruption{
			{InstanceID: "i-0a", Kind: cloud.InterruptionScheduledMaintenance, Deadline: start, Source: SourceQueueHealth, Key: arn + "i-0a", Once: true},
			{InstanceID: "i-0b", Kind: cloud.InterruptionScheduledMaintenance, Deadline: start, Source: SourceQueueHealth, Key: arn + "i-0b", Once: true}}, ""},
		{"health closed", healthEvent("e4", "closed", "i-0a"), []cloud.Interruption{
			{InstanceID: "i-0a", Kind: cloud.InterruptionScheduledMaintenance, Deadline: start, Source: SourceQueueHealth, Key: arn + "i-0a", Once: true, Ended: true}}, ""},
		{"running again", stateEvent("e5", "i-0a", "running"), []cloud.Interruption{
			{InstanceID: "i-0a", Source: SourceQueueState, Key: "aws-eventbridge:e5", Once: true, Ended: true}}, ""},
		{"stopping is the janitor's", stateEvent("e6", "i-0a", "stopping"), nil, ""},
		{"not JSON", "hello", nil, outcomeUnrecognised},
		{"another event", `{"id":"e7","source":"aws.autoscaling","detail-type":"EC2 Instance Launch Successful","detail":{}}`, nil, outcomeUnrecognised},
		{"spot without an instance", `{"id":"e8","source":"aws.ec2","detail-type":"EC2 Spot Instance Interruption Warning","time":"2026-11-18T10:00:00Z","detail":{}}`, nil, outcomeUnrecognised},
		{"health of another service", `{"id":"e9","source":"aws.health","detail-type":"AWS Health Event","detail":{"eventArn":"a","service":"RDS","eventTypeCategory":"scheduledChange","affectedEntities":[{"entityValue":"i-0a"}]}}`, nil, outcomeUnrecognised},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, outcome := parseInterruptionEvent(tc.body)
			if outcome != tc.outcome || fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("got %+v, %q; want %+v, %q", got, outcome, tc.want, tc.outcome)
			}
			for _, i := range got {
				if err := i.Validate(); err != nil {
					t.Fatalf("%+v: %v", i, err)
				}
			}
		})
	}
}

func queueNode(instance string) cloud.NodeRef {
	return cloud.NodeRef{Identity: cloud.NodeIdentity{Node: "node-" + instance, NodeUID: "u", ProviderID: "aws:///us-east-1a/" + instance, InstanceID: instance, BootID: "b"}}
}

type queueCloud struct {
	fake *fakeSQS
	n    int
}

func (c *queueCloud) Announce(kind cloud.InterruptionKind) {
	c.n++
	id := fmt.Sprintf("c%d", c.n)
	switch kind {
	case cloud.InterruptionRebalance:
		c.fake.send(rebalanceEvent(id, "i-0a"))
	default:
		c.fake.send(spotEvent(id, "i-0a"))
	}
}

func TestInterruptionQueueConformance(t *testing.T) {
	cloudtest.InterruptionConformance(t, cloudtest.InterruptionCase{
		New: func(t *testing.T) (cloud.InterruptionAware, cloudtest.InterruptionCloud) {
			fake := &fakeSQS{}
			return newInterruptionQueue(fake, "q"), &queueCloud{fake: fake}
		},
		Node: queueNode("i-0a"),
	})
}

// A message is deleted only when its notice is acknowledged; one whose
// notice was never acknowledged comes back and is the same notice.
func TestInterruptionQueueDeletesOnlyAfterAck(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSQS{}
	q := newInterruptionQueue(fake, "q")
	nodes := []cloud.NodeRef{queueNode("i-0a")}
	fake.send(spotEvent("e1", "i-0a"))
	if fresh, err := q.ReceiveInterruptions(ctx); err != nil || !fresh {
		t.Fatalf("receive: %v, %v", fresh, err)
	}
	if fake.waits[0] != queueWait {
		t.Fatalf("receive waited %d s, want a %d s long poll", fake.waits[0], queueWait)
	}
	got, err := q.Interruptions(ctx, nodes)
	if err != nil || len(got) != 1 {
		t.Fatalf("interruptions: %+v, %v", got, err)
	}
	if fake.left() != 1 {
		t.Fatal("a notice not acknowledged was deleted")
	}

	// The store write failed: no ack. The process restarts with nothing in
	// memory and the message is delivered again.
	fake.redeliver()
	q = newInterruptionQueue(fake, "q")
	if fresh, err := q.ReceiveInterruptions(ctx); err != nil || !fresh {
		t.Fatalf("receive again: %v, %v", fresh, err)
	}
	again, _ := q.Interruptions(ctx, nodes)
	if len(again) != 1 || again[0].Key != got[0].Key {
		t.Fatalf("redelivered %+v, want %+v", again, got)
	}

	// A delete that fails keeps the notice for the next ack.
	fake.deleteErr = errors.New("throttled")
	if err := q.AckInterruption(ctx, got[0].Key); err == nil {
		t.Fatal("ack with a failing delete reported no error")
	}
	if left, _ := q.Interruptions(ctx, nodes); len(left) != 1 {
		t.Fatalf("after a failed delete: %+v", left)
	}
	fake.deleteErr = nil
	if err := q.AckInterruption(ctx, got[0].Key); err != nil {
		t.Fatal(err)
	}
	if fake.left() != 0 {
		t.Fatal("acknowledged message not deleted")
	}
	if left, _ := q.Interruptions(ctx, nodes); len(left) != 0 {
		t.Fatalf("acknowledged notice still reported: %+v", left)
	}
}

// A second delivery of the same event is the same notice, and one ack
// deletes both copies.
func TestInterruptionQueueDuplicateEvent(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSQS{}
	q := newInterruptionQueue(fake, "q")
	fake.send(spotEvent("e1", "i-0a"))
	fake.send(spotEvent("e1", "i-0a"))
	if _, err := q.ReceiveInterruptions(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := q.Interruptions(ctx, []cloud.NodeRef{queueNode("i-0a")})
	if len(got) != 1 {
		t.Fatalf("duplicate event: %+v, want one notice", got)
	}
	if err := q.AckInterruption(ctx, got[0].Key); err != nil {
		t.Fatal(err)
	}
	if fake.left() != 0 {
		t.Fatalf("%d copies left after the ack", fake.left())
	}
}

// A message KubeNeuron does not act on is deleted at once; a notice for an
// instance that is not a cluster node is deleted when read, and opens
// nothing; a message for two instances is deleted once both are done.
func TestInterruptionQueueDeletesWhatItDoesNotKeep(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSQS{}
	q := newInterruptionQueue(fake, "q")
	fake.send("not an event")
	fake.send(stateEvent("e2", "i-0a", "terminated"))
	fake.send(spotEvent("e3", "i-0other"))
	fake.send(healthEvent("e4", "upcoming", "i-0a", "i-0b"))
	if _, err := q.ReceiveInterruptions(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.left() != 2 {
		t.Fatalf("%d messages left, want the spot and health ones", fake.left())
	}
	// Before the node list is known nothing is judged foreign.
	if got, _ := q.Interruptions(ctx, nil); got != nil || fake.left() != 2 {
		t.Fatalf("without nodes: %+v, %d left", got, fake.left())
	}
	got, err := q.Interruptions(ctx, []cloud.NodeRef{queueNode("i-0a"), queueNode("i-0b")})
	if err != nil || len(got) != 2 {
		t.Fatalf("interruptions: %+v, %v", got, err)
	}
	if fake.left() != 1 {
		t.Fatal("the foreign spot notice was not deleted")
	}
	if err := q.AckInterruption(ctx, got[0].Key); err != nil {
		t.Fatal(err)
	}
	if fake.left() != 1 {
		t.Fatal("the health message was deleted with one of its two instances pending")
	}
	if err := q.AckInterruption(ctx, got[1].Key); err != nil {
		t.Fatal(err)
	}
	if fake.left() != 0 {
		t.Fatal("the health message was not deleted once both instances were done")
	}
}

// AWS Health's later update of an event closes the same notice.
func TestInterruptionQueueHealthClosed(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSQS{}
	q := newInterruptionQueue(fake, "q")
	nodes := []cloud.NodeRef{queueNode("i-0a")}
	fake.send(healthEvent("e1", "upcoming", "i-0a"))
	if _, err := q.ReceiveInterruptions(ctx); err != nil {
		t.Fatal(err)
	}
	open, _ := q.Interruptions(ctx, nodes)
	fake.send(healthEvent("e2", "closed", "i-0a"))
	if fresh, err := q.ReceiveInterruptions(ctx); err != nil || !fresh {
		t.Fatalf("closing update: %v, %v", fresh, err)
	}
	closed, _ := q.Interruptions(ctx, nodes)
	if len(closed) != 1 || !closed[0].Ended || closed[0].Key != open[0].Key {
		t.Fatalf("closed %+v, open %+v", closed, open)
	}
	if err := q.AckInterruption(ctx, closed[0].Key); err != nil {
		t.Fatal(err)
	}
	if fake.left() != 0 {
		t.Fatalf("%d messages left after the closed event's ack", fake.left())
	}
}
