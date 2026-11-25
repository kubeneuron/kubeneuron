// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	listers "k8s.io/client-go/listers/core/v1"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/kubeneuron/kubeneuron/internal/httpapi"
)

const (
	testNamespace       = "kube-neuron-system"
	testInstallation    = "fleet"
	testInstallationUID = "installation-uid"
	testServiceAccount  = "fleet-agent"
	testDaemonSet       = "fleet-agent"
	testPod             = "fleet-agent-abcde"
	testNode            = "gpu-node-1"
	testAudience        = "kubeneuron-controller"
)

func TestAuthenticateAgentBindsTokenToLivePodAndNode(t *testing.T) {
	authenticator, request := newFixture(t)

	principal, err := authenticator.AuthenticateAgent(request)
	if err != nil {
		t.Fatalf("AuthenticateAgent() error = %v", err)
	}
	want := httpapi.AgentPrincipal{
		Namespace:      testNamespace,
		ServiceAccount: testServiceAccount,
		PodName:        testPod,
		PodUID:         "pod-uid",
		NodeName:       testNode,
		NodeUID:        "node-uid",
	}
	if principal != want {
		t.Fatalf("principal = %#v, want %#v", principal, want)
	}
}

func TestAuthenticateAgentRejectsInvalidIdentity(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testingFixture)
		wantStatus int
	}{
		{
			name: "no verified client certificate",
			mutate: func(f *testingFixture) {
				f.request.TLS = nil
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "certificate for another installation",
			mutate: func(f *testingFixture) {
				f.request.TLS.PeerCertificates[0].URIs = []*url.URL{{Scheme: "spiffe", Host: "kubeneuron.io", Path: "/installation/other/agent"}}
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "missing bearer token",
			mutate: func(f *testingFixture) {
				f.request.Header.Del("Authorization")
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "token not authenticated",
			mutate: func(f *testingFixture) {
				f.review.Status.Authenticated = false
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "wrong token audience",
			mutate: func(f *testingFixture) {
				f.review.Status.Audiences = []string{"other"}
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "wrong service account",
			mutate: func(f *testingFixture) {
				f.review.Status.User.Username = "system:serviceaccount:" + testNamespace + ":other"
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "missing Pod UID extra",
			mutate: func(f *testingFixture) {
				delete(f.review.Status.User.Extra, podUIDExtra)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "service account UID mismatch",
			mutate: func(f *testingFixture) {
				f.review.Status.User.UID = "old-service-account-uid"
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "Pod UID mismatch",
			mutate: func(f *testingFixture) {
				f.review.Status.User.Extra[podUIDExtra] = authenticationv1.ExtraValue{"other-pod-uid"}
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "Pod labels mismatch",
			mutate: func(f *testingFixture) {
				f.pod.Labels["app.kubernetes.io/instance"] = "other"
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "Pod owner mismatch",
			mutate: func(f *testingFixture) {
				f.pod.OwnerReferences[0].UID = "other-daemonset-uid"
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "Pod not running",
			mutate: func(f *testingFixture) {
				f.pod.Status.Phase = corev1.PodPending
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "DaemonSet terminating",
			mutate: func(f *testingFixture) {
				deletionTime := metav1.NewTime(time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC))
				f.daemonSet.DeletionTimestamp = &deletionTime
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "Node missing",
			mutate: func(f *testingFixture) {
				f.client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(corev1.Resource("nodes"), testNode)
				})
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "TokenReview API unavailable",
			mutate: func(f *testingFixture) {
				f.client.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("apiserver unavailable")
				})
			},
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTestingFixture(t)
			tt.mutate(fixture)
			if err := fixture.client.Tracker().Update(
				corev1.SchemeGroupVersion.WithResource("pods"),
				fixture.pod,
				fixture.pod.Namespace,
			); err != nil {
				t.Fatal(err)
			}
			if err := fixture.client.Tracker().Update(
				appsv1.SchemeGroupVersion.WithResource("daemonsets"),
				fixture.daemonSet,
				fixture.daemonSet.Namespace,
			); err != nil {
				t.Fatal(err)
			}
			_, err := fixture.authenticator.AuthenticateAgent(fixture.request)
			if err == nil {
				t.Fatal("AuthenticateAgent() error = nil")
			}
			var statusErr httpapi.HTTPStatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error %T does not expose an HTTP status: %v", err, err)
			}
			if got := statusErr.HTTPStatus(); got != tt.wantStatus {
				t.Fatalf("HTTP status = %d, want %d; error = %v", got, tt.wantStatus, err)
			}
		})
	}
}

type testingFixture struct {
	authenticator *Authenticator
	request       *http.Request
	client        *fake.Clientset
	review        *authenticationv1.TokenReview
	pod           *corev1.Pod
	daemonSet     *appsv1.DaemonSet
}

func newFixture(t *testing.T) (*Authenticator, *http.Request) {
	t.Helper()
	fixture := newTestingFixture(t)
	return fixture.authenticator, fixture.request
}

func newTestingFixture(t *testing.T) *testingFixture {
	t.Helper()
	controller := true
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: testServiceAccount, Namespace: testNamespace, UID: k8stypes.UID("service-account-uid"),
	}}
	daemonSet := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: testDaemonSet, Namespace: testNamespace, UID: k8stypes.UID("daemonset-uid"),
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPod,
			Namespace: testNamespace,
			UID:       k8stypes.UID("pod-uid"),
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kube-neuron",
				"app.kubernetes.io/instance":   testInstallation,
				"app.kubernetes.io/component":  "agent",
				"app.kubernetes.io/managed-by": "kubeneuron-operator",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: testDaemonSet,
				UID: daemonSet.UID, Controller: &controller,
			}},
		},
		Spec:   corev1.PodSpec{ServiceAccountName: testServiceAccount, NodeName: testNode},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNode, UID: k8stypes.UID("node-uid")}}
	client := fake.NewClientset(serviceAccount, daemonSet, pod, node)
	review := &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{testAudience},
		User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:" + testNamespace + ":" + testServiceAccount,
			UID:      "service-account-uid",
			Extra: map[string]authenticationv1.ExtraValue{
				podNameExtra: {testPod},
				podUIDExtra:  {"pod-uid"},
			},
		},
	}}
	client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		created := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if created.Spec.Token != "projected-token" {
			t.Errorf("TokenReview token = %q", created.Spec.Token)
		}
		if len(created.Spec.Audiences) != 1 || created.Spec.Audiences[0] != testAudience {
			t.Errorf("TokenReview audiences = %v", created.Spec.Audiences)
		}
		return true, review.DeepCopy(), nil
	})

	authenticator, err := New(client, Config{
		Audience:         testAudience,
		Namespace:        testNamespace,
		ServiceAccount:   testServiceAccount,
		AgentDaemonSets:  []string{testDaemonSet},
		InstallationName: testInstallation,
		InstallationUID:  testInstallationUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	authenticator.now = func() time.Time { return now }
	identityURI := &url.URL{Scheme: "spiffe", Host: "kubeneuron.io", Path: "/installation/" + testInstallationUID + "/agent"}
	leaf := &x509.Certificate{
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{identityURI},
	}
	request, err := http.NewRequest(http.MethodPost, "https://controller.example/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer projected-token")
	request.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
		VerifiedChains:   [][]*x509.Certificate{{leaf}},
	}
	return &testingFixture{
		authenticator: authenticator,
		request:       request,
		client:        client,
		review:        review,
		pod:           pod,
		daemonSet:     daemonSet,
	}
}

// N4: a Pod owned by the detection-only companion DaemonSet (which exists
// when Enabled arming narrows the primary) authenticates exactly like a
// primary agent Pod — and a DaemonSet missing from the allow-list admits
// nothing even when every label matches.
func TestAuthenticateAgentAcceptsDetectionCompanionDaemonSet(t *testing.T) {
	fixture := newTestingFixture(t)
	controller := true
	detect := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: "fleet-agent-detect", Namespace: testNamespace, UID: k8stypes.UID("detect-daemonset-uid"),
	}}
	if err := fixture.client.Tracker().Add(detect); err != nil {
		t.Fatal(err)
	}
	fixture.pod.Labels["app.kubernetes.io/component"] = "agent-detect"
	fixture.pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "DaemonSet", Name: detect.Name,
		UID: detect.UID, Controller: &controller,
	}}
	if _, err := fixture.client.CoreV1().Pods(testNamespace).Update(context.Background(), fixture.pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Not allow-listed: perfect labels and a live DaemonSet admit nothing.
	if _, err := fixture.authenticator.AuthenticateAgent(fixture.request); err == nil {
		t.Fatal("a DaemonSet outside the allow-list must not own authenticated agents")
	}

	fixture.authenticator.cfg.AgentDaemonSets = []string{testDaemonSet, detect.Name}
	principal, err := fixture.authenticator.AuthenticateAgent(fixture.request)
	if err != nil {
		t.Fatalf("detection-companion Pod failed to authenticate: %v", err)
	}
	if principal.NodeName != testNode {
		t.Fatalf("principal = %#v, want node %s", principal, testNode)
	}
}

// Each of the four agent DaemonSets owns Pods that authenticate, once its
// name is allow-listed; a component outside the four does not.
func TestAuthenticateAgentAcceptsEveryAgentDaemonSet(t *testing.T) {
	for _, component := range []string{"agent", "agent-detect", "agent-detect-journal-var", "agent-detect-journal-run"} {
		t.Run(component, func(t *testing.T) {
			fixture := newTestingFixture(t)
			controller := true
			owner := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
				Name: "fleet-" + component, Namespace: testNamespace, UID: k8stypes.UID(component + "-uid"),
			}}
			if component != "agent" {
				if err := fixture.client.Tracker().Add(owner); err != nil {
					t.Fatal(err)
				}
			} else {
				owner = fixture.daemonSet
			}
			fixture.pod.Labels["app.kubernetes.io/component"] = component
			fixture.pod.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: owner.Name, UID: owner.UID, Controller: &controller,
			}}
			if _, err := fixture.client.CoreV1().Pods(testNamespace).Update(context.Background(), fixture.pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			fixture.authenticator.cfg.AgentDaemonSets = []string{
				testDaemonSet, "fleet-agent-detect", "fleet-agent-detect-journal-var", "fleet-agent-detect-journal-run",
			}
			if principal, err := fixture.authenticator.AuthenticateAgent(fixture.request); err != nil || principal.NodeName != testNode {
				t.Fatalf("principal = %#v, err = %v", principal, err)
			}
			fixture.pod.Labels["app.kubernetes.io/component"] = component + "-other"
			if _, err := fixture.client.CoreV1().Pods(testNamespace).Update(context.Background(), fixture.pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.authenticator.AuthenticateAgent(fixture.request); err == nil {
				t.Fatal("an unknown component authenticated")
			}
		})
	}
}

func TestAuthenticateAgentObjectCaches(t *testing.T) {
	a, req := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartObjectCaches(ctx, testNodeLister(t, a), slog.Default())
	deadline, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if !cache.WaitForCacheSync(deadline.Done(), a.objects.pods.synced, a.objects.nodes.synced, a.objects.daemonSets.synced, a.objects.serviceAccounts.synced) {
		t.Fatal("identity caches did not sync")
	}
	client := a.client.(*fake.Clientset)
	for _, miss := range []bool{false, true} {
		if miss {
			a.objects.pods.get = func(name string) (*corev1.Pod, error) {
				return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
			}
		}
		client.ClearActions()
		if _, err := a.AuthenticateAgent(req); err != nil {
			t.Fatal(err)
		}
		var gets, reviews int
		for _, action := range client.Actions() {
			if action.GetVerb() == "get" {
				gets++
				if action.GetResource().Resource != "pods" {
					t.Fatalf("unexpected GET %v", action.GetResource())
				}
			}
			if action.GetResource().Resource == "tokenreviews" {
				reviews++
			}
		}
		want := 0
		if miss {
			want = 1
		}
		if gets != want || reviews != 1 {
			t.Fatalf("miss=%v: %d GETs, %d reviews", miss, gets, reviews)
		}
	}
}

func TestRepeatedReviewedToken(t *testing.T) {
	for _, scenario := range []string{"repeat", "missing TLS", "deleted Pod", "replacement Pod", "expired review", "changed audience"} {
		t.Run(scenario, func(t *testing.T) {
			f := newTestingFixture(t)
			a := f.authenticator
			now := a.now()
			a.now = func() time.Time { return now }
			claims := fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
			f.request.Header.Set("Authorization", "Bearer "+token)
			if scenario == "repeat" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				a.StartObjectCaches(ctx, testNodeLister(t, a), slog.Default())
				timeout, stop := context.WithTimeout(ctx, 5*time.Second)
				defer stop()
				if !cache.WaitForCacheSync(timeout.Done(), a.objects.pods.synced, a.objects.nodes.synced, a.objects.serviceAccounts.synced, a.objects.daemonSets.synced) {
					t.Fatal("cache sync timed out")
				}
				f.client.ClearActions()
			}
			reviews := 0
			f.client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
				reviews++
				res := f.review.DeepCopy()
				if reviews > 1 {
					res.Status.Authenticated = false
				}
				return true, res, nil
			})
			if _, err := a.AuthenticateAgent(f.request); err != nil {
				t.Fatal(err)
			}
			if scenario == "repeat" {
				actions := f.client.Actions()
				if len(actions) != 1 || actions[0].GetResource().Resource != "tokenreviews" {
					t.Fatalf("unexpected cold review actions: %v", actions)
				}
				f.client.ClearActions()
			}
			pods := f.client.CoreV1().Pods(testNamespace)
			switch scenario {
			case "missing TLS":
				f.request.TLS = nil
			case "deleted Pod", "replacement Pod":
				if err := pods.Delete(context.Background(), testPod, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				if scenario == "replacement Pod" {
					p := f.pod.DeepCopy()
					p.UID = "new"
					if _, err := pods.Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			case "expired review":
				// Model a disconnected watch retaining the Pod after deletion.
				if err := pods.Delete(context.Background(), testPod, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				a.objects.pods = objectReader[*corev1.Pod]{synced: func() bool { return true }, get: func(string) (*corev1.Pod, error) { return f.pod, nil }}
				if _, err := a.AuthenticateAgent(f.request); err != nil {
					t.Fatalf("stale view before expiry: %v", err)
				}
				now = now.Add(positiveReviewTTL)
			case "changed audience":
				a.cfg.Audience = "other"
			}
			_, err := a.AuthenticateAgent(f.request)
			if (err == nil) != (scenario == "repeat") {
				t.Fatalf("unexpected authentication result: %v", err)
			}
			if scenario == "repeat" && len(f.client.Actions()) != 0 {
				t.Fatalf("cache hit made API calls: %v", f.client.Actions())
			}
			// A token whose Pod is gone is invalid, not merely unauthorized:
			// the cached review is dropped and the API answers again.
			var statusErr httpapi.HTTPStatusError
			if scenario != "repeat" && (!errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusUnauthorized) {
				t.Fatalf("got %v, want 401", err)
			}
			want := 1
			if scenario != "repeat" && scenario != "missing TLS" {
				want = 2
			}
			if reviews != want {
				t.Fatalf("got %d TokenReviews, want %d", reviews, want)
			}
		})
	}
}

func testNodeLister(t *testing.T, a *Authenticator) func() listers.NodeLister {
	t.Helper()
	n, err := a.client.CoreV1().Nodes().Get(context.Background(), testNode, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	index := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	if err := index.Add(n); err != nil {
		t.Fatal(err)
	}
	l := listers.NewNodeLister(index)
	return func() listers.NodeLister { return l }
}

func TestUnsyncedCacheDiagnostics(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&out, nil))
	warnUnsynced(context.Background(), map[string]cache.InformerSynced{"pods": func() bool { return false }, "nodes": func() bool { return true }}, log, 0)
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `"resource":"pods"`) {
		t.Fatalf("unexpected warnings: %s", out.String())
	}
	metric := metrics.AgentAuthLiveGets.WithLabelValues("pods")
	before := testutil.ToFloat64(metric)
	f := newTestingFixture(t)
	if _, err := f.authenticator.AuthenticateAgent(f.request); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metric) - before; got != 1 {
		t.Fatalf("fallback count delta = %v", got)
	}
}
