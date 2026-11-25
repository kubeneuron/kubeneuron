// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// CloudNode takes all fields from one live Node read, not an inventory cache.
func (p *Platform) CloudNode(ctx context.Context, name string) (cloud.NodeRef, error) {
	if p.recycler == nil {
		return cloud.NodeRef{}, fmt.Errorf("no cloud provider is configured")
	}
	n, err := p.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return cloud.NodeRef{}, nodeNotFound{err}
	}
	if err != nil {
		return cloud.NodeRef{}, err
	}
	identity, err := p.nodeIdentity(n)
	if err != nil {
		return cloud.NodeRef{}, err
	}
	ref := cloud.NodeRef{Identity: identity, Labels: maps.Clone(n.Labels), Annotations: maps.Clone(n.Annotations)}
	for _, condition := range n.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			ref.Ready = condition.Status == corev1.ConditionTrue
		}
	}
	if aware, ok := p.recycler.(cloud.NodeEventsAware); ok {
		ref.Events, ref.EventsKnown = p.nodeEvents(ctx, n.Name, aware.NodeEventSources())
	}
	return ref, nil
}

// nodeNotFound is the Kubernetes API's NotFound for a live Node read. It keeps
// the API's message and error, and adds platform.ErrNodeNotFound to the chain.
type nodeNotFound struct{ err error }

func (e nodeNotFound) Error() string   { return e.err.Error() }
func (e nodeNotFound) Unwrap() []error { return []error{platform.ErrNodeNotFound, e.err} }

// MatchCloudNodeUID reads only the Node. Compare its UID before parsing the
// provider ID: a newly created Node may not have a provider ID yet.
func (p *Platform) MatchCloudNodeUID(ctx context.Context, name, uid string) (cloud.NodeIdentity, error) {
	if uid == "" || p.recycler == nil {
		return cloud.NodeIdentity{}, fmt.Errorf("matching requires an opening node UID and a configured cloud provider")
	}
	n, err := p.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cloud.NodeIdentity{}, err
	}
	if n.Name != name || string(n.UID) != uid {
		return cloud.NodeIdentity{}, cloud.ErrIdentityChanged
	}
	return p.nodeIdentity(n)
}

func (p *Platform) nodeIdentity(n *corev1.Node) (cloud.NodeIdentity, error) {
	id, err := p.recycler.InstanceID(n.Spec.ProviderID)
	if err != nil {
		return cloud.NodeIdentity{}, err
	}
	pin := cloud.NodeIdentity{
		Node: n.Name, NodeUID: string(n.UID), ProviderID: n.Spec.ProviderID,
		InstanceID: id, BootID: n.Status.NodeInfo.BootID,
	}
	if err := pin.Validate(); err != nil {
		return cloud.NodeIdentity{}, err
	}
	return pin, nil
}

// nodeEventLimit bounds one read of a node's events; a node with more is a
// truncated read, so its events are not known.
const nodeEventLimit = 500

// nodeEventNamespace is where Kubernetes records events about nodes.
const nodeEventNamespace = "default"

// nodeEvents reads a node's recent events from the named sources. Any failure
// or a truncated list makes them unknown, never empty-and-known.
func (p *Platform) nodeEvents(ctx context.Context, node string, sources []string) ([]cloud.NodeEvent, bool) {
	if len(sources) == 0 {
		return nil, false
	}
	selector := fields.Set{"involvedObject.kind": "Node", "involvedObject.name": node}.AsSelector().String()
	list, err := p.client.CoreV1().Events(nodeEventNamespace).List(ctx, metav1.ListOptions{FieldSelector: selector, Limit: nodeEventLimit})
	if err != nil || list.Continue != "" {
		return nil, false
	}
	since := time.Now().Add(-cloud.NodeEventWindow)
	var out []cloud.NodeEvent
	for _, e := range list.Items {
		source := e.Source.Component
		if !slices.Contains(sources, source) {
			source = e.ReportingController
		}
		if !slices.Contains(sources, source) {
			continue
		}
		// An event with no time at all is kept: it may be recent.
		if at := eventTime(e); at.IsZero() || at.After(since) {
			out = append(out, cloud.NodeEvent{Source: source, Reason: e.Reason, Time: at})
		}
	}
	return out, true
}

// eventTime is when an event last happened, from whichever of its times the
// recorder filled.
func eventTime(e corev1.Event) time.Time {
	at := e.LastTimestamp.Time
	if e.Series != nil && e.Series.LastObservedTime.After(at) {
		at = e.Series.LastObservedTime.Time
	}
	if e.EventTime.After(at) {
		at = e.EventTime.Time
	}
	if e.FirstTimestamp.After(at) {
		at = e.FirstTimestamp.Time
	}
	return at
}
