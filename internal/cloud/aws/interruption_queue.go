// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// OptionInterruptionQueue is the cloud.Config option naming the SQS queue
// that EventBridge delivers the account's EC2 interruption events to. Without
// it the provider reads no interruptions.
const OptionInterruptionQueue = "interruptionQueueURL"

// Sources of the notices read from the queue, shown with the incident.
const (
	SourceQueueSpot      = "aws-eventbridge:ec2/spot-instance-interruption-warning"
	SourceQueueRebalance = "aws-eventbridge:ec2/instance-rebalance-recommendation"
	SourceQueueHealth    = "aws-eventbridge:health/ec2-scheduled-change"
	SourceQueueState     = "aws-eventbridge:ec2/instance-state-change"
)

// spotNotice is how long after the event's time a spot instance is assumed to
// be stopped or terminated. The event documents no deadline: AWS sends it
// "two minutes prior" to the interruption.
const spotNotice = 2 * time.Minute

const (
	// queueWait is the long poll of one receive, the most SQS allows.
	queueWait = 20
	// queueBatch is the most messages one receive returns.
	queueBatch = 10
	// queueVisibility is how long a received message stays hidden. One not
	// deleted by then, because its notice could not be recorded, is
	// delivered again.
	queueVisibility = 300
)

// SQSAPI is the subset of the SQS client the interruption queue uses.
type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// interruptionQueue reads the EC2 interruption events EventBridge puts on a
// queue dedicated to one installation. A message is deleted once every notice
// it carries is acknowledged, or at once when it carries none KubeNeuron acts
// on. Only the fields below are read from a message; its body is never kept
// or logged.
type interruptionQueue struct {
	api SQSAPI
	url string

	mu sync.Mutex
	// pending are the notices received and not yet acknowledged, by key.
	pending map[string]*queuedNotice
	// messages are the received messages not yet deleted, by message ID.
	messages map[string]*queuedMessage
}

type queuedNotice struct {
	interruption cloud.Interruption
	messages     map[string]bool
}

type queuedMessage struct {
	receipt string
	// keys are the message's notices not yet acknowledged.
	keys map[string]bool
}

var _ cloud.InterruptionQueue = (*queueProvider)(nil)

// queueProvider is the AWS provider with an interruption queue configured.
type queueProvider struct {
	*managedProvider
	*interruptionQueue
}

func newInterruptionQueue(api SQSAPI, url string) *interruptionQueue {
	return &interruptionQueue{api: api, url: url, pending: map[string]*queuedNotice{}, messages: map[string]*queuedMessage{}}
}

// ReceiveInterruptions waits for the queue's next messages and keeps the
// notices they carry.
func (q *interruptionQueue) ReceiveInterruptions(ctx context.Context) (bool, error) {
	out, err := q.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: awssdk.String(q.url), MaxNumberOfMessages: queueBatch,
		WaitTimeSeconds: queueWait, VisibilityTimeout: queueVisibility,
	})
	if err != nil {
		return false, err
	}
	fresh := false
	var errs []error
	for _, m := range out.Messages {
		id, receipt := awssdk.ToString(m.MessageId), awssdk.ToString(m.ReceiptHandle)
		notices, outcome := parseInterruptionEvent(awssdk.ToString(m.Body))
		if len(notices) == 0 {
			if outcome != "" {
				metrics.CloudInterruptions.WithLabelValues(providerName, "unknown", outcome).Inc()
			}
			errs = append(errs, q.delete(ctx, receipt))
			continue
		}
		if q.keep(id, receipt, notices) {
			fresh = true
		}
	}
	return fresh, errors.Join(errs...)
}

// keep records a message's notices; it reports whether any is new.
func (q *interruptionQueue) keep(id, receipt string, notices []cloud.Interruption) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	msg := q.messages[id]
	if msg == nil {
		msg = &queuedMessage{keys: map[string]bool{}}
		for _, n := range notices {
			msg.keys[n.Key] = true
		}
		q.messages[id] = msg
	}
	// A message delivered again has a new receipt handle; only the newest
	// deletes it.
	msg.receipt = receipt
	fresh := false
	for _, n := range notices {
		if !msg.keys[n.Key] {
			continue
		}
		p := q.pending[n.Key]
		if p == nil {
			p = &queuedNotice{interruption: n, messages: map[string]bool{}}
			q.pending[n.Key] = p
			fresh = true
		} else if p.interruption.Ended != n.Ended || !p.interruption.Deadline.Equal(n.Deadline) {
			// A later update of the same event, such as AWS Health
			// closing it.
			p.interruption = n
			fresh = true
		}
		p.messages[id] = true
	}
	return fresh
}

// Interruptions returns the received notices for the given nodes' instances.
// A notice for an instance that is not one of them is about another
// cluster's instance, or one this cluster no longer has: it is acknowledged
// and counted, and opens nothing. With no nodes given, nothing is judged
// foreign.
func (q *interruptionQueue) Interruptions(ctx context.Context, nodes []cloud.NodeRef) ([]cloud.Interruption, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	ours := map[string]bool{}
	for _, n := range nodes {
		ours[n.Identity.InstanceID] = true
	}
	var out []cloud.Interruption
	var foreign []cloud.Interruption
	q.mu.Lock()
	for _, p := range q.pending {
		if ours[p.interruption.InstanceID] {
			out = append(out, p.interruption)
		} else {
			foreign = append(foreign, p.interruption)
		}
	}
	q.mu.Unlock()
	var errs []error
	for _, f := range foreign {
		if !f.Ended {
			metrics.CloudInterruptions.WithLabelValues(providerName, string(f.Kind), "foreign").Inc()
		}
		errs = append(errs, q.AckInterruption(ctx, f.Key))
	}
	return out, errors.Join(errs...)
}

// AckInterruption forgets the notice and deletes each of its messages that
// carries no other notice still pending. When a delete fails the notice stays
// pending, so a later acknowledgement deletes again.
func (q *interruptionQueue) AckInterruption(ctx context.Context, key string) error {
	q.mu.Lock()
	p := q.pending[key]
	if p == nil {
		q.mu.Unlock()
		return nil
	}
	receipts := map[string]string{}
	for id := range p.messages {
		if msg := q.messages[id]; msg != nil && len(msg.keys) == 1 && msg.keys[key] {
			receipts[id] = msg.receipt
		}
	}
	q.mu.Unlock()
	var errs []error
	deleted := map[string]bool{}
	for id, receipt := range receipts {
		if err := q.delete(ctx, receipt); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted[id] = true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for id := range deleted {
		delete(q.messages, id)
		delete(p.messages, id)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	for id := range p.messages {
		if msg := q.messages[id]; msg != nil {
			delete(msg.keys, key)
		}
	}
	delete(q.pending, key)
	return nil
}

func (q *interruptionQueue) delete(ctx context.Context, receipt string) error {
	_, err := q.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: awssdk.String(q.url), ReceiptHandle: awssdk.String(receipt)})
	return err
}

// interruptionEvent is the part of an EventBridge event the queue reads.
type interruptionEvent struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	DetailType string          `json:"detail-type"`
	Time       time.Time       `json:"time"`
	Detail     json.RawMessage `json:"detail"`
}

type ec2EventDetail struct {
	InstanceID string `json:"instance-id"`
	State      string `json:"state"`
}

type healthEventDetail struct {
	EventArn          string `json:"eventArn"`
	Service           string `json:"service"`
	EventTypeCategory string `json:"eventTypeCategory"`
	StartTime         string `json:"startTime"`
	StatusCode        string `json:"statusCode"`
	AffectedEntities  []struct {
		EntityValue string `json:"entityValue"`
	} `json:"affectedEntities"`
}

// Message outcomes counted when a message carries no notice.
const (
	outcomeUnrecognised = "unrecognised"
)

// parseInterruptionEvent reads the notices one message carries, following
// the event formats in the EC2 and AWS Health documentation (2026-11-18). It
// returns none, with an outcome to count, for a message it cannot read or one
// of an event KubeNeuron does not act on; and none and no outcome for an
// instance state change other than running, which the vanished-node janitor
// covers.
func parseInterruptionEvent(body string) ([]cloud.Interruption, string) {
	var ev interruptionEvent
	if err := json.Unmarshal([]byte(body), &ev); err != nil || ev.ID == "" {
		return nil, outcomeUnrecognised
	}
	key := "aws-eventbridge:" + ev.ID
	switch {
	case ev.Source == "aws.ec2" && ev.DetailType == "EC2 Spot Instance Interruption Warning":
		d, ok := ec2Detail(ev)
		if !ok || ev.Time.IsZero() {
			return nil, outcomeUnrecognised
		}
		return []cloud.Interruption{{InstanceID: d.InstanceID, Kind: cloud.InterruptionSpotReclaim,
			Deadline: ev.Time.Add(spotNotice).UTC(), Source: SourceQueueSpot, Key: key, At: ev.Time.UTC(), Once: true}}, ""
	case ev.Source == "aws.ec2" && ev.DetailType == "EC2 Instance Rebalance Recommendation":
		d, ok := ec2Detail(ev)
		if !ok {
			return nil, outcomeUnrecognised
		}
		return []cloud.Interruption{{InstanceID: d.InstanceID, Kind: cloud.InterruptionRebalance,
			Source: SourceQueueRebalance, Key: key, At: ev.Time.UTC(), Once: true}}, ""
	case ev.Source == "aws.ec2" && ev.DetailType == "EC2 Instance State-change Notification":
		d, ok := ec2Detail(ev)
		if !ok || d.State == "" {
			return nil, outcomeUnrecognised
		}
		if d.State != "running" {
			return nil, ""
		}
		return []cloud.Interruption{{InstanceID: d.InstanceID, Source: SourceQueueState, Key: key, At: ev.Time.UTC(), Once: true, Ended: true}}, ""
	case ev.Source == "aws.health" && ev.DetailType == "AWS Health Event":
		return healthNotices(ev)
	}
	return nil, outcomeUnrecognised
}

func ec2Detail(ev interruptionEvent) (ec2EventDetail, bool) {
	var d ec2EventDetail
	if err := json.Unmarshal(ev.Detail, &d); err != nil || !strings.HasPrefix(d.InstanceID, "i-") {
		return d, false
	}
	return d, true
}

// healthNotices are the notices of an AWS Health scheduled change to EC2
// instances, one per affected instance, keyed by the event and the instance
// so the event's later updates carry the same keys.
func healthNotices(ev interruptionEvent) ([]cloud.Interruption, string) {
	var d healthEventDetail
	if err := json.Unmarshal(ev.Detail, &d); err != nil || d.EventArn == "" {
		return nil, outcomeUnrecognised
	}
	if d.Service != "EC2" || d.EventTypeCategory != "scheduledChange" {
		return nil, outcomeUnrecognised
	}
	var deadline time.Time
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339} {
		if t, err := time.Parse(layout, d.StartTime); err == nil {
			deadline = t.UTC()
			break
		}
	}
	ended := d.StatusCode == "closed"
	var out []cloud.Interruption
	for _, e := range d.AffectedEntities {
		id := e.EntityValue
		if i := strings.LastIndex(id, "instance/"); i >= 0 {
			id = id[i+len("instance/"):]
		}
		if !strings.HasPrefix(id, "i-") {
			continue
		}
		out = append(out, cloud.Interruption{InstanceID: id, Kind: cloud.InterruptionScheduledMaintenance,
			Deadline: deadline, Source: SourceQueueHealth, Key: "aws-eventbridge:" + d.EventArn + "/" + id,
			At: ev.Time.UTC(), Once: true, Ended: ended})
	}
	if len(out) == 0 {
		return nil, outcomeUnrecognised
	}
	return out, ""
}
