// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
)

// firstAlertRecord is where the controller notes the first metric alert it
// accepts: the installation's inventory summary, which the operator reads
// to report that alerts from an External store reach the controller.
type firstAlertRecord struct {
	maps         typedcore.ConfigMapInterface
	installation string
	uid          string
	done         atomic.Bool
}

// ConfigureFirstAlertRecord binds the first-alert record to the
// installation's inventory summary. Call before serving requests.
func (c *Controller) ConfigureFirstAlertRecord(maps typedcore.ConfigMapInterface, installation, uid string) {
	c.firstAlert.maps, c.firstAlert.installation, c.firstAlert.uid = maps, installation, uid
}

// RecordAlertAccepted sets firstAlertAcceptedAt in the inventory summary if
// it is not set yet. The webhook calls it only after an alert's signal was
// committed. Once the key exists it neither reads nor writes again.
func (c *Controller) RecordAlertAccepted(ctx context.Context) error {
	r := &c.firstAlert
	if r.maps == nil || r.done.Load() {
		return nil
	}
	name := r.installation + inventory.Suffix
	for attempt := 0; ; attempt++ {
		cm, err := r.maps.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		owner := metav1.GetControllerOf(cm)
		if r.uid == "" || owner == nil || string(owner.UID) != r.uid || owner.Name != r.installation || owner.Kind != "KubeNeuron" {
			return errors.New("inventory ConfigMap belongs to another installation")
		}
		if _, ok := inventory.FirstAlert(cm.Data); ok {
			r.done.Store(true)
			return nil
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[inventory.FirstAlertAcceptedAt] = time.Now().UTC().Format(time.RFC3339Nano)
		// Update carries the resourceVersion of the Get: a concurrent
		// inventory publish makes it conflict, and the next attempt rereads.
		_, err = r.maps.Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			r.done.Store(true)
			return nil
		}
		if !apierrors.IsConflict(err) || attempt == 4 {
			return err
		}
	}
}
