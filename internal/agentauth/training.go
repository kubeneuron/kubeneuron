// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"net/http"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TrainingReportPod authenticates the sender; accelerator allocation, monitored
// node membership and the live owner chain must be verified before accepting data.
func TrainingReportPod(r *http.Request, client kubernetes.Interface, installationUID string) (*corev1.Pod, error) {
	if installationUID == "" || r.TLS == nil {
		return nil, unauthorized("training endpoint identity unavailable")
	}
	token, err := bearerToken(r)
	if err != nil {
		return nil, err
	}
	audience := "kubeneuron.io/training-report/" + installationUID
	review, err := client.AuthenticationV1().TokenReviews().Create(r.Context(), &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{audience}}}, metav1.CreateOptions{})
	if err != nil {
		return nil, forbidden("training token verification unavailable")
	}
	if !review.Status.Authenticated || review.Status.Error != "" || !contains(review.Status.Audiences, audience) {
		return nil, unauthorized("invalid training token")
	}
	user := review.Status.User
	account, ok := strings.CutPrefix(user.Username, "system:serviceaccount:")
	namespace, sa, split := strings.Cut(account, ":")
	name, named := singleExtra(user.Extra, podNameExtra)
	uid, bound := singleExtra(user.Extra, podUIDExtra)
	if !ok || !split || namespace == "" || sa == "" || !named || !bound {
		return nil, forbidden("training pod binding required")
	}
	pod, err := client.CoreV1().Pods(namespace).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil || string(pod.UID) != uid || pod.Spec.ServiceAccountName != sa || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
		return nil, forbidden("training pod binding invalid")
	}
	return pod, nil
}
