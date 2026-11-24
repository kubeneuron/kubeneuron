// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	listers "k8s.io/client-go/listers/core/v1"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type objectReader[T any] struct {
	synced cache.InformerSynced
	get    func(string) (T, error)
}

func (reader objectReader[T]) lookup(ctx context.Context, resource, name string, live func(context.Context, string, metav1.GetOptions) (T, error)) (T, error) {
	if reader.synced != nil && reader.synced() {
		if obj, err := reader.get(name); err == nil {
			return obj, nil
		}
	}
	metrics.AgentAuthLiveGets.WithLabelValues(resource).Inc()
	return live(ctx, name, metav1.GetOptions{})
}

type identityObjects struct {
	pods            objectReader[*corev1.Pod]
	nodes           objectReader[*corev1.Node]
	serviceAccounts objectReader[*corev1.ServiceAccount]
	daemonSets      objectReader[*appsv1.DaemonSet]
}

// StartObjectCaches must be called before serving requests. Each reader falls
// back independently until its initial list has completed.
func (a *Authenticator) StartObjectCaches(ctx context.Context, nodeLister func() listers.NodeLister, log *slog.Logger) {
	ns := a.cfg.Namespace
	factory := informers.NewSharedInformerFactoryWithOptions(a.client, 0, informers.WithNamespace(ns))
	pods := factory.Core().V1().Pods()

	accounts := factory.Core().V1().ServiceAccounts()
	sets := factory.Apps().V1().DaemonSets()
	a.objects = identityObjects{
		pods:            objectReader[*corev1.Pod]{pods.Informer().HasSynced, pods.Lister().Pods(ns).Get},
		nodes:           objectReader[*corev1.Node]{func() bool { return nodeLister() != nil }, func(name string) (*corev1.Node, error) { return nodeLister().Get(name) }},
		serviceAccounts: objectReader[*corev1.ServiceAccount]{accounts.Informer().HasSynced, accounts.Lister().ServiceAccounts(ns).Get},
		daemonSets:      objectReader[*appsv1.DaemonSet]{sets.Informer().HasSynced, sets.Lister().DaemonSets(ns).Get},
	}
	factory.Start(ctx.Done())
	checks := map[string]cache.InformerSynced{"pods": a.objects.pods.synced, "nodes": a.objects.nodes.synced, "serviceaccounts": a.objects.serviceAccounts.synced, "daemonsets": a.objects.daemonSets.synced}
	go warnUnsynced(ctx, checks, log, 30*time.Second)
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cache.WaitForCacheSync(wait.Done(), a.objects.pods.synced, a.objects.nodes.synced, a.objects.serviceAccounts.synced, a.objects.daemonSets.synced)
}

func warnUnsynced(ctx context.Context, checks map[string]cache.InformerSynced, log *slog.Logger, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	for resource, synced := range checks {
		if !synced() {
			log.Warn("Agent authentication cache has not synchronized; using live reads", "resource", resource)
		}
	}
}
