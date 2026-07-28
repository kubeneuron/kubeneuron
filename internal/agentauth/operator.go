package agentauth

import (
	"fmt"
	"net/http"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubeneuron/kubeneuron/internal/httpapi"
)

// OperatorAuthenticator authenticates operator API callers with their own
// Kubernetes credential instead of the shared static token. TokenReview
// establishes who is calling; SubjectAccessReview then requires that this
// principal may get/update the root KubeNeuron object — so operator-API
// rights are granted and revoked with ordinary Kubernetes RBAC on the CR,
// and every audit row carries a verifiable identity.
type OperatorAuthenticator struct {
	client       kubernetes.Interface
	installation string
}

// NewOperator builds the Kubernetes-credential authenticator for the
// operator API of the named installation.
func NewOperator(client kubernetes.Interface, installation string) (*OperatorAuthenticator, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}
	if strings.TrimSpace(installation) == "" {
		return nil, fmt.Errorf("installation name is required")
	}
	return &OperatorAuthenticator{client: client, installation: installation}, nil
}

// AuthenticateOperator implements httpapi.OperatorAuthenticator.
func (a *OperatorAuthenticator) AuthenticateOperator(r *http.Request, verb string) (httpapi.OperatorIdentity, error) {
	token, err := bearerToken(r)
	if err != nil {
		return httpapi.OperatorIdentity{}, err
	}
	// No audience restriction: human kubectl tokens and OIDC tokens carry
	// the API server audience, not an installation-specific one. Identity
	// is still fully verified by the API server.
	review, err := a.client.AuthenticationV1().TokenReviews().Create(r.Context(), &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return httpapi.OperatorIdentity{}, unavailable("TokenReview failed", err)
	}
	if !review.Status.Authenticated || review.Status.User.Username == "" {
		return httpapi.OperatorIdentity{}, unauthorized("token is not authenticated")
	}
	user := review.Status.User

	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}
	access, err := a.client.AuthorizationV1().SubjectAccessReviews().Create(r.Context(), &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user.Username,
			Groups: user.Groups,
			UID:    user.UID,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:    "kubeneuron.io",
				Resource: "kubeneurons",
				Name:     a.installation,
				Verb:     verb,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return httpapi.OperatorIdentity{}, unavailable("SubjectAccessReview failed", err)
	}
	if !access.Status.Allowed {
		return httpapi.OperatorIdentity{}, forbidden(fmt.Sprintf(
			"%s may not %s kubeneurons/%s", user.Username, verb, a.installation))
	}
	return httpapi.OperatorIdentity{Actor: user.Username, Method: "kubernetes"}, nil
}

var _ httpapi.OperatorAuthenticator = (*OperatorAuthenticator)(nil)
