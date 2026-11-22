// SPDX-License-Identifier: Apache-2.0

// Command kubeneuron-operator runs the Kubernetes control plane for
// KubeNeuron. It reconciles CRD configuration into runtime resources and
// delegates storage-heavy dependencies to their dedicated operators.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	// Blank import registers every cloud provider so the operator can gate a
	// playbook against a provider's declared capabilities at compile time.
	_ "github.com/kubeneuron/kubeneuron/internal/cloud/providers"
	"github.com/kubeneuron/kubeneuron/internal/operator"
	"github.com/kubeneuron/kubeneuron/internal/telemetry"
)

var version = "dev"

func main() {
	var (
		metricsAddr           = flag.String("metrics-bind-address", ":8080", "address for Prometheus metrics")
		probeAddr             = flag.String("health-probe-bind-address", ":8081", "address for health probes")
		leaderElect           = flag.Bool("leader-elect", true, "enable Kubernetes Lease leader election")
		leaderNamespace       = flag.String("leader-election-namespace", "", "namespace for the leader-election Lease (default: pod namespace)")
		showVersion           = flag.Bool("version", false, "print version and exit")
		allowNodeLevelActions = flag.Bool("allow-node-level-actions", false, "permit installations to opt into alpha node-level actions")
		defaultAdapters       = flag.String("default-agent-adapters", "",
			"JSON list of spec.agent.adapters entries added to every installation, e.g. "+
				`'[{"vendor":"amd","options":{"tool":"amd-smi"}}]'; an installation entry replaces a default for the same vendor`)
		collectorImage = flag.String("otel-collector-image", operator.DefaultCollectorImage,
			"image of a Managed spec.observability.collector; pin by digest, e.g. a mirror of the default")
		metricsStoreImage = flag.String("metrics-store-image", operator.DefaultMetricsStoreImage,
			"image of a Managed spec.observability.store; pin by digest, e.g. a mirror of the default")
		metricsRulesImage = flag.String("metrics-rules-image", operator.DefaultMetricsRulesImage,
			"image of the recording rules evaluator (vmalert) that comes with a Managed store; pin by digest")
		grafanaImage = flag.String("grafana-image", operator.DefaultGrafanaImage,
			"image of a Managed spec.observability.grafana: the official open-source Grafana, unmodified; pin by digest, e.g. a mirror of the default")
		// Production logging by default (JSON, info, sampled); development
		// output remains reachable with --zap-devel.
		logOptions = zap.Options{}
	)
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	if *showVersion {
		fmt.Println("kubeneuron-operator", version)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))
	scheme := runtime.NewScheme()
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(policyv1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
	utilruntime.Must(networkingv1.AddToScheme(scheme))
	utilruntime.Must(kubeneuronv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          *leaderElect,
		LeaderElectionID:        "kubeneuron-operator.kubeneuron.io",
		LeaderElectionNamespace: *leaderNamespace,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress:  *probeAddr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating manager:", err)
		os.Exit(1)
	}

	agentAdapters, err := operator.ParseDefaultAgentAdapters(*defaultAdapters)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing -default-agent-adapters:", err)
		os.Exit(1)
	}
	if err := (&operator.KubeNeuronReconciler{
		Client:                mgr.GetClient(),
		APIReader:             mgr.GetAPIReader(),
		Scheme:                mgr.GetScheme(),
		Recorder:              mgr.GetEventRecorderFor("kubeneuron-operator"),
		AllowNodeLevelActions: *allowNodeLevelActions,
		DefaultAgentAdapters:  agentAdapters,
		CollectorImage:        *collectorImage,
		MetricsStoreImage:     *metricsStoreImage,
		MetricsRulesImage:     *metricsRulesImage,
		GrafanaImage:          *grafanaImage,
		// The chart sets POD_NAMESPACE; the collector admits the operator's
		// own telemetry from that namespace only.
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintln(os.Stderr, "registering reconciler:", err)
		os.Exit(1)
	}
	if err := (&operator.GPUAutonomyPlanReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintln(os.Stderr, "registering GPUAutonomyPlan reconciler:", err)
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		fmt.Fprintln(os.Stderr, "adding health check:", err)
		os.Exit(1)
	}
	// Readiness reflects the informer caches: an operator that has not yet
	// synced must not be routed traffic or counted available in a rollout.
	if err := mgr.AddReadyzCheck("cache-sync", func(req *http.Request) error {
		// Bounded wait: a synced cache answers immediately; an unsynced one
		// must fail the probe quickly, not hang until the kubelet timeout.
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return fmt.Errorf("informer caches are not synced")
		}
		return nil
	}); err != nil {
		fmt.Fprintln(os.Stderr, "adding readiness check:", err)
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	stopTelemetry := telemetry.Start(ctx, telemetry.Options{
		ServiceName: "kubeneuron-operator",
		Version:     version,
		Gatherer:    ctrlmetrics.Registry,
		Registerer:  ctrlmetrics.Registry,
		Log:         slog.New(logr.ToSlogHandler(ctrl.Log.WithName("telemetry"))),
	})
	err = mgr.Start(ctx)
	_ = stopTelemetry(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "running manager:", err)
		os.Exit(1)
	}
}
