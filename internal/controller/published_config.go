// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// runtimeConfigSuffix names the ConfigMap the operator publishes the
// compiled configuration in: <installation>-runtime.
const runtimeConfigSuffix = "-runtime"

// unloadedConfigReason holds an automatic step while the configuration the
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

// ConfigurePublishedConfig makes automatic steps wait until the loaded
// configuration is the one published for the installation. Without it (no
// operator: the configuration comes from files) there is nothing to wait for.
func (c *Controller) ConfigurePublishedConfig(maps typedcore.ConfigMapInterface, installation string) {
	c.published = publishedConfig{maps: maps, name: installation + runtimeConfigSuffix}
}

// unloadedConfig says why the loaded configuration cannot be taken as the
// published one, or "" when it is. It never says "" on a failed read.
func (c *Controller) unloadedConfig(ctx context.Context) string {
	p := c.published
	if p.maps == nil {
		return ""
	}
	cm, err := p.maps.Get(ctx, p.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf(unloadedConfigReason, "the published configuration cannot be read: "+err.Error(), p.name)
	}
	published := strings.TrimSpace(cm.Data["config-digest"])
	loaded := c.runtimeConfig(ctx).SourceDigest
	switch {
	case published == "":
		return fmt.Sprintf(unloadedConfigReason, "the published configuration has no digest", p.name)
	case loaded != published:
		return fmt.Sprintf(unloadedConfigReason, "the controller has not yet loaded the published configuration", p.name)
	}
	return ""
}
