// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

const (
	defaultStoreSize      = "20Gi"
	defaultStoreRetention = "30d"
)

// observabilityMode is the mode of a collector or store block; a block left
// out is Disabled, so an upgrade never starts anything on its own.
func observabilityMode(spec *kubeneuronv1alpha1.OTLPEndpointSpec) kubeneuronv1alpha1.IntegrationMode {
	if spec == nil || spec.Mode == "" {
		return kubeneuronv1alpha1.IntegrationModeDisabled
	}
	return spec.Mode
}

func collectorSpec(installation *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.CollectorSpec {
	return installation.Spec.Observability.Collector
}

func collectorMode(installation *kubeneuronv1alpha1.KubeNeuron) kubeneuronv1alpha1.IntegrationMode {
	if c := collectorSpec(installation); c != nil {
		return observabilityMode(&c.OTLPEndpointSpec)
	}
	return kubeneuronv1alpha1.IntegrationModeDisabled
}

// effectiveStore is spec.observability.store. The deprecated
// victoriaMetrics block is a query address, not an OTLP destination, and is
// never written to.
func effectiveStore(installation *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.MetricsStoreSpec {
	return installation.Spec.Observability.Store
}

func storeMode(installation *kubeneuronv1alpha1.KubeNeuron) kubeneuronv1alpha1.IntegrationMode {
	if s := effectiveStore(installation); s != nil {
		return observabilityMode(&s.OTLPEndpointSpec)
	}
	return kubeneuronv1alpha1.IntegrationModeDisabled
}

func storeSize(store *kubeneuronv1alpha1.MetricsStoreSpec) (resource.Quantity, error) {
	size := defaultStoreSize
	if store.Managed != nil && store.Managed.Storage.Size != "" {
		size = store.Managed.Storage.Size
	}
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("spec.observability.store.managed.storage.size: %w", err)
	}
	if quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("spec.observability.store.managed.storage.size must be positive")
	}
	return quantity, nil
}

func storeRetention(store *kubeneuronv1alpha1.MetricsStoreSpec) string {
	if store.Managed != nil && store.Managed.Retention != "" {
		return store.Managed.Retention
	}
	return defaultStoreRetention
}

func storeEphemeral(store *kubeneuronv1alpha1.MetricsStoreSpec) bool {
	return store.Managed != nil && store.Managed.Storage.Ephemeral
}

// validateObservability repeats the schema's rules (an object can reach
// the operator without admission, in tests and on old API servers) and adds
// the ones CEL cannot express.
func validateObservability(s *kubeneuronv1alpha1.KubeNeuron) error {
	obs := s.Spec.Observability
	if obs.VictoriaMetrics != nil {
		if obs.Store != nil {
			return fmt.Errorf("spec.observability.victoriaMetrics is deprecated and replaced by store; set only store")
		}
		if err := validateDependency("victoriaMetrics", *obs.VictoriaMetrics); err != nil {
			return err
		}
	}
	if obs.Collector != nil {
		if err := validateOTLPEndpoint("spec.observability.collector", s.Spec.Namespace, obs.Collector.OTLPEndpointSpec); err != nil {
			return err
		}
		if obs.Collector.Resources != nil && obs.Collector.Mode != kubeneuronv1alpha1.IntegrationModeManaged {
			return fmt.Errorf("spec.observability.collector.resources apply only to a Managed collector")
		}
		if obs.Collector.DCGMExporter != nil && obs.Collector.Mode != kubeneuronv1alpha1.IntegrationModeManaged {
			return fmt.Errorf("spec.observability.collector.dcgmExporter applies only to a Managed collector")
		}
	}
	if obs.Store != nil {
		if err := validateOTLPEndpoint("spec.observability.store", s.Spec.Namespace, obs.Store.OTLPEndpointSpec); err != nil {
			return err
		}
		if obs.Store.Managed != nil && obs.Store.Mode != kubeneuronv1alpha1.IntegrationModeManaged {
			return fmt.Errorf("spec.observability.store.managed applies only to a Managed store")
		}
		if obs.Store.Mode == kubeneuronv1alpha1.IntegrationModeManaged {
			if _, err := storeSize(obs.Store); err != nil {
				return err
			}
		}
	}
	if collectorMode(s) == kubeneuronv1alpha1.IntegrationModeExternal && storeMode(s) == kubeneuronv1alpha1.IntegrationModeManaged {
		return fmt.Errorf("spec.observability: an external collector routes metrics itself, so the store cannot be Managed")
	}
	if g := obs.Grafana; g != nil {
		switch g.Mode {
		case kubeneuronv1alpha1.IntegrationModeDisabled, kubeneuronv1alpha1.IntegrationModeExternal, kubeneuronv1alpha1.IntegrationModeManaged:
		default:
			return fmt.Errorf("spec.observability.grafana has unsupported mode %q", g.Mode)
		}
		if g.Mode != kubeneuronv1alpha1.IntegrationModeManaged && (g.Datasource != nil || g.Resources != nil) {
			return fmt.Errorf("spec.observability.grafana.datasource and resources apply only to a Managed grafana")
		}
		if g.Mode == kubeneuronv1alpha1.IntegrationModeManaged {
			switch storeMode(s) {
			case kubeneuronv1alpha1.IntegrationModeManaged:
				if g.Datasource != nil {
					return fmt.Errorf("spec.observability.grafana.datasource applies only to an External store; a Managed store is the datasource itself")
				}
			case kubeneuronv1alpha1.IntegrationModeExternal:
				if g.Datasource == nil {
					return fmt.Errorf("spec.observability.grafana: a Managed grafana with an External store requires datasource.url")
				}
				if err := validateOTLPEndpoint("spec.observability.grafana.datasource", s.Spec.Namespace, kubeneuronv1alpha1.OTLPEndpointSpec{
					Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: g.Datasource.URL, AuthorizationSecretRef: g.Datasource.AuthorizationSecretRef,
				}); err != nil {
					return errors.New(strings.Replace(err.Error(), "datasource.endpoint", "datasource.url", 1))
				}
			default:
				return fmt.Errorf("spec.observability.grafana: a Managed grafana needs a Managed or External store to read")
			}
		}
	}
	return nil
}

func validateOTLPEndpoint(field, namespace string, spec kubeneuronv1alpha1.OTLPEndpointSpec) error {
	switch spec.Mode {
	case kubeneuronv1alpha1.IntegrationModeManaged, kubeneuronv1alpha1.IntegrationModeDisabled:
		if spec.Endpoint != "" || spec.AuthorizationSecretRef != nil {
			return fmt.Errorf("%s: endpoint and authorizationSecretRef apply only to External", field)
		}
		return nil
	case kubeneuronv1alpha1.IntegrationModeExternal:
	default:
		return fmt.Errorf("%s has unsupported mode %q", field, spec.Mode)
	}
	endpoint, err := url.Parse(strings.TrimSpace(spec.Endpoint))
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return fmt.Errorf("%s.endpoint must be an http or https URL", field)
	}
	if endpoint.User != nil {
		return fmt.Errorf("%s.endpoint must not carry credentials; use authorizationSecretRef", field)
	}
	if _, err := endpointPort(spec.Endpoint); err != nil {
		return fmt.Errorf("%s.endpoint: %w", field, err)
	}
	if ref := spec.AuthorizationSecretRef; ref != nil {
		if ref.Key == "" {
			return fmt.Errorf("%s.authorizationSecretRef.key is required", field)
		}
		if ref.Namespace != "" && ref.Namespace != namespace {
			return fmt.Errorf("%s.authorizationSecretRef must be in the installation namespace %q", field, namespace)
		}
	}
	return nil
}

// endpointPort is the TCP port an endpoint URL connects to.
func endpointPort(endpoint string) (int32, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return 0, err
	}
	if p := parsed.Port(); p != "" {
		port, err := strconv.ParseUint(p, 10, 16)
		if err != nil || port == 0 {
			return 0, fmt.Errorf("invalid port %q", p)
		}
		return int32(port), nil
	}
	if parsed.Scheme == "https" {
		return 443, nil
	}
	return 80, nil
}
