// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	kubeplatform "github.com/kubeneuron/kubeneuron/internal/platform/kubernetes"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/postgres"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// benchNodeAPI connects the production platform adapter to client-go's fake
// tracker, preserving the adapter's API calls and client-side rate limiter.
func benchNodeAPI(b *testing.B, objects []runtime.Object) (*kubeplatform.Platform, *fake.Clientset) {
	b.Helper()
	client := fake.NewSimpleClientset(objects...)
	nodes := client.CoreV1().Nodes()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes" && !strings.HasPrefix(r.URL.Path, "/api/v1/nodes/") {
			http.NotFound(w, r)
			b.Errorf("unexpected API path %s", r.URL.Path)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
		var result runtime.Object
		var err error
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var list *corev1.NodeList
			list, err = nodes.List(r.Context(), metav1.ListOptions{})
			if err == nil {
				list.TypeMeta = metav1.TypeMeta{Kind: "NodeList", APIVersion: "v1"}
				result = list
			}
		case r.Method == http.MethodGet:
			result, err = nodes.Get(r.Context(), name, metav1.GetOptions{})
		case r.Method == http.MethodPatch:
			var body []byte
			body, err = io.ReadAll(r.Body)
			if err == nil {
				result, err = nodes.Patch(r.Context(), name, ktypes.PatchType(r.Header.Get("Content-Type")), body, metav1.PatchOptions{})
			}
		default:
			err = fmt.Errorf("unexpected API method %s", r.Method)
		}
		if err != nil {
			b.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if node, ok := result.(*corev1.Node); ok {
			node.TypeMeta = metav1.TypeMeta{Kind: "Node", APIVersion: "v1"}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			b.Error(err)
		}
	}))
	b.Cleanup(server.Close)
	path := filepath.Join(b.TempDir(), "kubeconfig")
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: bench\n  cluster:\n    server: %s\ncontexts:\n- name: bench\n  context:\n    cluster: bench\ncurrent-context: bench\n", server.URL)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		b.Fatal(err)
	}
	b.Setenv("KUBERNETES_SERVICE_HOST", "")
	p, err := kubeplatform.New(path)
	if err != nil {
		b.Fatal(err)
	}
	return p, client
}

// Embedding an unset Store makes an unexpected uncounted workflow call fail
// the benchmark instead of silently undercounting it.
type benchWorkflowStore struct {
	store.Store
	db    store.Store
	calls int
}

func (s *benchWorkflowStore) GetIncident(ctx context.Context, id string) (*types.Incident, error) {
	s.calls++
	return s.db.GetIncident(ctx, id)
}
func (s *benchWorkflowStore) ListIncidents(ctx context.Context, f store.IncidentFilter) ([]*types.Incident, error) {
	s.calls++
	return s.db.ListIncidents(ctx, f)
}
func (s *benchWorkflowStore) FirstApprovalDecision(ctx context.Context, id string, epoch int) (*types.Approval, error) {
	s.calls++
	return s.db.FirstApprovalDecision(ctx, id, epoch)
}

// A fresh schema keeps each PostgreSQL fixture independent of other runs.
func benchFleetStore(b *testing.B) store.Store {
	b.Helper()
	dsn := os.Getenv("KUBENEURON_BENCH_POSTGRES_DSN")
	if dsn == "" {
		db, err := sqlite.Open(filepath.Join(b.TempDir(), "workflow.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if err := db.Close(); err != nil {
				b.Error(err)
			}
		})
		return db
	}
	u, err := url.Parse(dsn)
	if err != nil {
		b.Fatal("invalid benchmark PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := admin.Close(); err != nil {
			b.Error(err)
		}
	})
	schema := fmt.Sprintf("fleet_bench_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			b.Error(err)
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := postgres.Open(b.Context(), u.String())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := db.Close(); err != nil {
			b.Error(err)
		}
	})
	return db
}

func BenchmarkControllerFleetPass(b *testing.B) {
	for _, scenario := range []struct {
		phase string
		held  int
	}{
		{"Janitors", 0}, {"Janitors", 50}, {"Janitors", 200},
		{"JanitorsOpen", 50}, {"JanitorsOpen", 200},
		{"JanitorsSecond", 50}, {"JanitorsSecond", 200},
		{"Reconcile", 50}, {"Reconcile", 200},
	} {
		b.Run(fmt.Sprintf("%s/held=%d", scenario.phase, scenario.held), func(b *testing.B) {
			db := benchFleetStore(b)
			ctx := b.Context()
			var objects []runtime.Object
			for i := 0; i < 500; i++ {
				name := fmt.Sprintf("worker-%03d", i)
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"}}
				objects = append(objects, node)
				if i >= scenario.held {
					continue
				}
				id := "incident-" + name
				owners := fmt.Sprintf("[%q]", id)
				node.Spec.Unschedulable = true
				node.Spec.Taints = []corev1.Taint{{Key: kubeplatform.DegradedTaintKey, Effect: corev1.TaintEffectNoSchedule, Value: "fell-off-bus"}}
				node.Annotations = map[string]string{
					"kubeneuron.io/cordon-owners":   owners,
					"kubeneuron.io/degraded-owners": owners,
					"kubeneuron.io/cordon-reason":   "kubeneuron: fell-off-bus (" + id + ")",
					"kubeneuron.io/cordon-restore":  "",
					"karpenter.sh/do-not-disrupt":   "true",
				}
				state := types.StateAwaitingApproval
				if scenario.phase == "Janitors" || scenario.phase == "JanitorsSecond" {
					switch i % 3 {
					case 1:
						state = types.StateResolved
					case 2:
						continue
					}
				}
				now := time.Now()
				inc := &types.Incident{ID: id, State: state, Target: types.Target{Node: name}, Class: "fell-off-bus", ApprovalEpoch: 1, OpenedAt: now, StateChangedAt: now, UpdatedAt: now}
				if err := db.CreateIncident(ctx, inc); err != nil {
					b.Fatal(err)
				}
			}
			p, client := benchNodeAPI(b, objects)
			s := &benchWorkflowStore{db: db}
			engine, err := playbook.NewEngine(nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			c := New(s, nil, engine, nil, nil, p, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			c.SetDegradedTaintPolicy(DegradedTaintPolicy{Enabled: true})
			runJanitors := func() {
				if cordons, taints := c.reconcileNodeHolds(ctx); !cordons || !taints {
					b.Fatal("janitor listing failed")
				}
			}
			verbs := map[string]int{"list": 0, "get": 0, "patch": 0}
			var apiCalls, storeCalls int
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				b.StopTimer()
				for _, obj := range objects[:scenario.held] {
					if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("nodes"), obj, ""); err != nil {
						b.Fatal(err)
					}
				}
				if scenario.phase == "JanitorsSecond" {
					runJanitors()
				}
				client.ClearActions()
				before := s.calls
				b.StartTimer()
				if scenario.phase != "Reconcile" {
					runJanitors()
				} else {
					c.reconcile(ctx)
				}
				b.StopTimer()
				storeCalls += s.calls - before
				actions := client.Actions()
				apiCalls += len(actions)
				for _, a := range actions {
					verbs[a.GetVerb()]++
				}
			}
			for verb, count := range verbs {
				b.ReportMetric(float64(count)/float64(b.N), "kube-"+verb+"/op")
			}
			b.ReportMetric(float64(apiCalls)/float64(b.N), "kube-calls/op")
			b.ReportMetric(float64(storeCalls)/float64(b.N), "store-calls/op")
		})
	}
}
