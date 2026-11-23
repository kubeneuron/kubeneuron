// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// runtimeConfigSuffix names the ConfigMap the operator publishes the
// compiled configuration in: <installation>-runtime.
const runtimeConfigSuffix = "-runtime"

// unloadedConfigReason holds a step while the configuration the
// controller acts on is not the one the operator published.
const unloadedConfigReason = "%s; the step is retried when it is loaded. To check, compare source_digest in /api/v1/runtime-config with config-digest in ConfigMap %s"

// publishedConfig reads the digest of the configuration the operator has
// published. The controller loads its configuration from the mounted copy of
// that ConfigMap, which the kubelet refreshes a minute or more after the
// operator writes it; the API has the published one at once.
type publishedConfig struct {
	maps typedcore.ConfigMapInterface
	name string
}

// ConfigurePublishedConfig makes steps wait until the loaded
// configuration is the one published for the installation. Without it (no
// operator: the configuration comes from files) there is nothing to wait for.
func (c *Controller) ConfigurePublishedConfig(maps typedcore.ConfigMapInterface, installation string) {
	c.published = publishedConfig{maps: maps, name: installation + runtimeConfigSuffix}
}

// PublishedConfig reads the digest the operator published and when it wrote
// it, for /api/v1/runtime-config. configured is false without an operator:
// nothing is published then.
func (c *Controller) PublishedConfig(ctx context.Context) (digest string, at time.Time, configured bool, err error) {
	p := c.published
	if p.maps == nil {
		return "", time.Time{}, false, nil
	}
	cm, err := p.maps.Get(ctx, p.name, metav1.GetOptions{})
	if err != nil {
		return "", time.Time{}, true, err
	}
	// The last write to the ConfigMap, by whichever manager made it.
	at = cm.CreationTimestamp.Time
	for _, f := range cm.ManagedFields {
		if f.Time != nil && f.Time.After(at) {
			at = f.Time.Time
		}
	}
	return strings.TrimSpace(cm.Data["config-digest"]), at, true, nil
}

// unloadedConfig says why the loaded configuration cannot be taken as the
// published one, or "" when it is. It never says "" on a failed read. The
// digests it compared go to the audit row of a held step.
func (c *Controller) unloadedConfig(ctx context.Context) (string, map[string]string) {
	p := c.published
	if p.maps == nil {
		return "", nil
	}
	loaded := c.runtimeConfig(ctx).SourceDigest
	digests := map[string]string{"loaded_config_digest": loaded}
	cm, err := p.maps.Get(ctx, p.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf(unloadedConfigReason, "the published configuration cannot be read: "+err.Error(), p.name), digests
	}
	published := strings.TrimSpace(cm.Data["config-digest"])
	digests["published_config_digest"] = published
	switch {
	case published == "":
		return fmt.Sprintf(unloadedConfigReason, "the published configuration has no digest", p.name), digests
	case loaded != published:
		return fmt.Sprintf(unloadedConfigReason, "the controller has not yet loaded the published configuration", p.name), digests
	}
	return "", nil
}

// holdUnloadedConfig holds a step, before anything decides whether it may
// run, while the loaded configuration is not the published one: a policy,
// an approval or the execution mode the operator has replaced could
// otherwise still start it. The decision is taken again under the new
// configuration once it is loaded; an approval stays recorded. A
// notification step acts on no node and is not held.
func (c *Controller) holdUnloadedConfig(ctx context.Context, inc *types.Incident, step *playbook.Step) bool {
	if strings.HasPrefix(step.Action, "notify.") {
		return false
	}
	reason, digests := c.unloadedConfig(ctx)
	if reason == "" {
		return false
	}
	c.deferStep(inc, step, metrics.DeferConfigNotLoaded)
	held := step.Name + " held: " + reason
	if pol, ok := c.automaticPolicyFor(ctx, inc); ok {
		held = step.Name + " held by policy " + pol.Name + ": " + reason
	}
	if !c.logOnce.first("config-held/" + inc.ID + "/" + step.Name + "/" + held) {
		return true
	}
	c.log.Info("holding the step until the published configuration is loaded", "incident", inc.ID, "step", step.Name, "reason", reason)
	if err := c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), FromState: inc.State, ToState: inc.State,
		Actor: "system", Action: "recovery-held", Params: digests, Result: held, DryRun: inc.DryRun,
	}); err != nil {
		c.log.Error("recovery-held audit append failed", "incident", inc.ID, "err", err)
	}
	return true
}
