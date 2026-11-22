// SPDX-License-Identifier: Apache-2.0

// Package telemetry exports a component's Prometheus metrics over OTLP.
//
// There is one set of instruments: the Prometheus registry that already
// serves /metrics. The OpenTelemetry Prometheus bridge reads that registry at
// every export interval, so a series exported over OTLP is the same series a
// scrape returns, with the same name and labels; nothing is counted twice.
//
// Export is off unless an OTLP endpoint is configured through the standard
// OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
// variables, and OTEL_SDK_DISABLED=true or OTEL_METRICS_EXPORTER=none turn it
// off again. A component never waits on its telemetry: a misconfiguration is
// logged and the component runs without export; an export that fails is
// dropped (one batch at most is in flight, under a timeout) and counted in
// kubeneuron_telemetry_export_failures_total.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	prometheusbridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	// Protocol is the only OTLP transport the components speak.
	Protocol = "http/protobuf"
	// DefaultInterval applies when OTEL_METRIC_EXPORT_INTERVAL is unset.
	DefaultInterval = 30 * time.Second
	// DefaultTimeout applies when OTEL_METRIC_EXPORT_TIMEOUT is unset; it
	// bounds one export including the exporter's retries.
	DefaultTimeout = 10 * time.Second
	// ShutdownTimeout bounds the final flush when the caller's context has
	// no deadline of its own.
	ShutdownTimeout = 5 * time.Second
	// errorLogInterval rate-limits export error logs; the failure counter
	// still counts every failure.
	errorLogInterval = time.Minute
)

// Options configures Start.
type Options struct {
	// ServiceName is the default service.name resource attribute
	// (kubeneuron-controller, kubeneuron-agent, kubeneuron-operator);
	// OTEL_SERVICE_NAME overrides it.
	ServiceName string
	// Version becomes service.version.
	Version string
	// Gatherer is the registry /metrics serves.
	Gatherer prometheus.Gatherer
	// Registerer receives kubeneuron_telemetry_export_failures_total when
	// export is enabled. Usually the registry behind Gatherer.
	Registerer prometheus.Registerer
	// Log receives configuration warnings and rate-limited export errors.
	Log *slog.Logger
	// Getenv reads the environment; nil means os.Getenv. Tests set it.
	Getenv func(string) string
}

// Shutdown flushes the last batch and stops export. It is safe to call when
// export was never enabled.
type Shutdown func(context.Context) error

func noop(context.Context) error { return nil }

// Start begins OTLP export when the environment asks for it, and otherwise
// returns a no-op Shutdown. It never returns an error: a configuration that
// cannot be used is logged and export stays off, because a component must
// not fail to start over its telemetry.
func Start(ctx context.Context, o Options) Shutdown {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	enabled, err := configured(o.Getenv)
	if err != nil {
		o.Log.Warn("OTLP metrics export disabled: invalid configuration", "err", err)
		return noop
	}
	if !enabled {
		return noop
	}
	shutdown, err := start(ctx, o)
	if err != nil {
		o.Log.Warn("OTLP metrics export disabled: could not start", "err", err)
		return noop
	}
	o.Log.Info("OTLP metrics export enabled", "protocol", Protocol)
	return shutdown
}

// configured reports whether export is enabled.
func configured(getenv func(string) string) (bool, error) {
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true") {
		return false, nil
	}
	switch exporter := strings.TrimSpace(getenv("OTEL_METRICS_EXPORTER")); exporter {
	case "", "otlp":
	case "none":
		return false, nil
	default:
		return false, fmt.Errorf("OTEL_METRICS_EXPORTER=%q: only otlp and none are supported", exporter)
	}
	if strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")) == "" &&
		strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" {
		return false, nil
	}
	protocol := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"))
	if protocol == "" {
		protocol = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	// One transport is simpler to test and document, and the Collector we
	// deploy listens on it. Unset means ours; any other value is refused
	// rather than silently replaced.
	if protocol != "" && protocol != Protocol {
		return false, fmt.Errorf("OTLP protocol %q is not supported (supported: %s)", protocol, Protocol)
	}
	return true, nil
}

func start(ctx context.Context, o Options) (Shutdown, error) {
	if o.Gatherer == nil || o.Registerer == nil {
		return nil, errors.New("telemetry: Gatherer and Registerer are required")
	}
	res, err := resource.New(ctx,
		// Ours first: OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
		// (WithFromEnv) override them, which is how the operator adds the
		// cluster, namespace, pod and node attributes.
		resource.WithAttributes(
			attribute.String("service.name", o.ServiceName),
			attribute.String("service.version", o.Version),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}

	// The exporter reads the remaining OTEL_EXPORTER_OTLP_* variables itself:
	// endpoint, headers, certificates, insecure, timeout.
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("OTLP exporter: %w", err)
	}

	failures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubeneuron_telemetry_export_failures_total",
		Help: "OTLP metric exports that failed and were dropped.",
	})
	if err := o.Registerer.Register(failures); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			_ = exp.Shutdown(ctx)
			return nil, fmt.Errorf("register export failure counter: %w", err)
		}
		failures = already.ExistingCollector.(prometheus.Counter)
	}

	logger := &rateLimitedLog{log: o.Log, every: errorLogInterval}
	// The SDK reports a failed periodic export through the global error
	// handler as well; sharing the limiter keeps that to one line a minute.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.warn("OpenTelemetry error", err)
	}))

	readerOpts := []sdkmetric.PeriodicReaderOption{
		sdkmetric.WithProducer(prometheusbridge.NewMetricProducer(
			prometheusbridge.WithGatherer(o.Gatherer))),
	}
	// Explicit options override the environment in the SDK, so pass our
	// defaults only when the standard variables are unset.
	if o.Getenv("OTEL_METRIC_EXPORT_INTERVAL") == "" {
		readerOpts = append(readerOpts, sdkmetric.WithInterval(DefaultInterval))
	}
	if o.Getenv("OTEL_METRIC_EXPORT_TIMEOUT") == "" {
		readerOpts = append(readerOpts, sdkmetric.WithTimeout(DefaultTimeout))
	}
	reader := sdkmetric.NewPeriodicReader(
		&countingExporter{Exporter: exp, failures: failures, log: logger},
		readerOpts...)
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)

	var once sync.Once
	return func(ctx context.Context) error {
		var err error
		once.Do(func() {
			if _, ok := ctx.Deadline(); !ok {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, ShutdownTimeout)
				defer cancel()
			}
			err = provider.Shutdown(ctx)
		})
		return err
	}, nil
}

// countingExporter counts and logs failed exports. The periodic reader
// drops a failed batch; the next interval exports current values again,
// which loses nothing for cumulative series.
type countingExporter struct {
	sdkmetric.Exporter
	failures prometheus.Counter
	log      *rateLimitedLog
}

func (e *countingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err != nil {
		e.failures.Inc()
		e.log.warn("OTLP metrics export failed; batch dropped", err)
	}
	return err
}

type rateLimitedLog struct {
	log   *slog.Logger
	every time.Duration
	mu    sync.Mutex
	last  time.Time
}

func (l *rateLimitedLog) warn(msg string, err error) {
	l.mu.Lock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.mu.Unlock()
		return
	}
	l.last = now
	l.mu.Unlock()
	l.log.Warn(msg, "err", err)
}
