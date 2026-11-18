// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"net/http/httptest"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestTrainingReportPodBinding(t *testing.T) {
	const audience = "kubeneuron.io/training-report/install"
	for _, tc := range []struct {
		name, aud, uid string
		allow          bool
	}{
		{"current", audience, "uid", true}, {"replacement", audience, "old", false}, {"different installation", "kubeneuron.io/training-report/other", "uid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work", UID: "uid"}, Spec: corev1.PodSpec{ServiceAccountName: "trainer", NodeName: "worker"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
			client.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
				review := a.(ktesting.CreateAction).GetObject().(*authv1.TokenReview)
				if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != audience {
					t.Fatal("unexpected audience")
				}
				review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{tc.aud}, User: authv1.UserInfo{Username: "system:serviceaccount:work:trainer", Extra: map[string]authv1.ExtraValue{podNameExtra: {"pod"}, podUIDExtra: {tc.uid}}}}
				return true, review, nil
			})
			req := httptest.NewRequest("POST", "https://controller/", nil)
			req.Header.Set("Authorization", "Bearer fixture")
			pod, err := TrainingReportPod(req, client, "install")
			if (err == nil) != tc.allow || (pod != nil) != tc.allow {
				t.Fatal("unexpected authentication result", err)
			}
		})
	}
}
