// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	// draHealthTimeout bounds one list of the ResourceSlices.
	draHealthTimeout = 20 * time.Second
	// draHealthMaxDevices bounds the devices one read takes in.
	draHealthMaxDevices = 100000
	// draTaintValueMax is the longest taint value kept; the profile table
	// reads only short documented values.
	draTaintValueMax = 64
)

// DRAHealth reads, from the resource.k8s.io/v1 ResourceSlices, the devices
// each existing Node publishes and their taints. Only a slice with an
// explicit spec.nodeName naming a Node that exists is read; a slice scoped
// any other way, or malformed, is skipped. Nothing but the driver, pool,
// device name, uuid attribute and taints is taken from a slice. The caller
// turns the read on: the platform does not know the configuration.
func (p *Platform) DRAHealth(ctx context.Context) ([]types.NodeHealthSnapshot, error) {
	if p == nil || p.dynamic == nil {
		return nil, &platform.DRAHealthError{Reason: draReasonClientUnavailable, Err: errors.New("no dynamic client")}
	}
	ctx, cancel := context.WithTimeout(ctx, draHealthTimeout)
	defer cancel()
	list, err := p.dynamic.Resource(draResourceSlices).List(ctx, metav1.ListOptions{})
	if err != nil {
		reason := draReasonListFailed
		switch {
		case apierrors.IsForbidden(err):
			reason = draReasonForbidden
		case apierrors.IsNotFound(err):
			reason = draReasonAPIUnavailable
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || apierrors.IsTimeout(err):
			reason = draReasonTimeout
		}
		return nil, &platform.DRAHealthError{Reason: reason, Err: err}
	}
	nodes, err := p.allNodes(ctx)
	if err != nil {
		return nil, &platform.DRAHealthError{Reason: draReasonListFailed, Err: err}
	}
	readAt := time.Now()
	byNode := map[string]*types.NodeHealthSnapshot{}
	for _, n := range nodes {
		byNode[n.Name] = &types.NodeHealthSnapshot{Node: n.Name, UID: string(n.UID), Labels: n.Labels, ReadAt: readAt,
			ProviderID: n.Spec.ProviderID, CreatedAt: n.CreationTimestamp.Time}
	}
	total := 0
	for i := range list.Items {
		obj := list.Items[i].Object
		nodeName, ok, err := unstructured.NestedString(obj, "spec", "nodeName")
		if err != nil || !ok {
			continue
		}
		snap := byNode[nodeName]
		if snap == nil {
			continue
		}
		devices := sliceDevices(obj)
		total += len(devices)
		if total > draHealthMaxDevices {
			return nil, &platform.DRAHealthError{Reason: draReasonBoundsExceeded, Err: errors.New("too many devices")}
		}
		snap.DRADevices = append(snap.DRADevices, devices...)
	}
	var out []types.NodeHealthSnapshot
	for _, snap := range byNode {
		if len(snap.DRADevices) > 0 {
			out = append(out, *snap)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}

// sliceDevices are the devices of one v1 ResourceSlice with their taints. A
// field of the wrong type drops the device, or the slice.
func sliceDevices(obj map[string]any) []types.DRADevice {
	driver, _, err1 := unstructured.NestedString(obj, "spec", "driver")
	pool, _, err2 := unstructured.NestedString(obj, "spec", "pool", "name")
	raw, _, err3 := unstructured.NestedSlice(obj, "spec", "devices")
	if err1 != nil || err2 != nil || err3 != nil || driver == "" {
		return nil
	}
	var out []types.DRADevice
	for _, r := range raw {
		dev, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name, _, err := unstructured.NestedString(dev, "name")
		if err != nil || name == "" {
			continue
		}
		d := types.DRADevice{Driver: driver, Pool: pool, Device: name}
		// A v1 device attribute is a one-of: the uuid is read only as a
		// string.
		if uuid, ok, err := unstructured.NestedString(dev, "attributes", "uuid", "string"); err == nil && ok {
			d.UUID = uuid
		}
		taints, _, err := unstructured.NestedSlice(dev, "taints")
		if err != nil {
			continue
		}
		for _, t := range taints {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			key, _, _ := unstructured.NestedString(tm, "key")
			value, _, _ := unstructured.NestedString(tm, "value")
			effect, _, _ := unstructured.NestedString(tm, "effect")
			if key == "" {
				continue
			}
			if len(value) > draTaintValueMax {
				value = value[:draTaintValueMax]
			}
			taint := types.DRATaint{Key: key, Value: value, Effect: effect}
			if added, ok, _ := unstructured.NestedString(tm, "timeAdded"); ok {
				if at, err := time.Parse(time.RFC3339, added); err == nil {
					taint.TimeAdded = at
				}
			}
			d.Taints = append(d.Taints, taint)
		}
		out = append(out, d)
	}
	return out
}
