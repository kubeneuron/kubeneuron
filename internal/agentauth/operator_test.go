package agentauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/kubeneuron/kubeneuron/internal/httpapi"
)

func operatorAuthRequest(token string) *http.Request {
	req := httptest.NewRequest("POST", "/api/v1/incidents/inc-1/approve", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func operatorAuthFixture(t *testing.T, authenticated, allowed bool) (*OperatorAuthenticator, *[]authorizationv1.SubjectAccessReviewSpec) {
	t.Helper()
	client := fake.NewClientset()
	client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		review.Status = authenticationv1.TokenReviewStatus{
			Authenticated: authenticated,
			User: authenticationv1.UserInfo{
				Username: "system:serviceaccount:ops:sre-bot",
				UID:      "sa-uid",
				Groups:   []string{"system:serviceaccounts"},
			},
		}
		return true, review, nil
	})
	var sars []authorizationv1.SubjectAccessReviewSpec
	client.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		sar := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		sars = append(sars, sar.Spec)
		sar.Status.Allowed = allowed
		return true, sar, nil
	})
	authenticator, err := NewOperator(client, "fleet")
	if err != nil {
		t.Fatal(err)
	}
	return authenticator, &sars
}

func TestOperatorAuthenticatorAllowsAuthorizedPrincipal(t *testing.T) {
	authenticator, sars := operatorAuthFixture(t, true, true)
	identity, err := authenticator.AuthenticateOperator(operatorAuthRequest("caller-token"), "update")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Actor != "system:serviceaccount:ops:sre-bot" || identity.Method != "kubernetes" {
		t.Fatalf("identity = %+v", identity)
	}
	if len(*sars) != 1 {
		t.Fatalf("SubjectAccessReviews = %d, want 1", len(*sars))
	}
	spec := (*sars)[0]
	attrs := spec.ResourceAttributes
	if spec.User != "system:serviceaccount:ops:sre-bot" || attrs == nil ||
		attrs.Group != "kubeneuron.io" || attrs.Resource != "kubeneurons" ||
		attrs.Name != "fleet" || attrs.Verb != "update" {
		t.Fatalf("SubjectAccessReview spec = %+v", spec)
	}
}

func TestOperatorAuthenticatorRejectsUnauthorizedPrincipal(t *testing.T) {
	authenticator, _ := operatorAuthFixture(t, true, false)
	_, err := authenticator.AuthenticateOperator(operatorAuthRequest("caller-token"), "update")
	assertHTTPStatus(t, err, http.StatusForbidden)
}

func TestOperatorAuthenticatorRejectsUnauthenticatedToken(t *testing.T) {
	authenticator, sars := operatorAuthFixture(t, false, true)
	_, err := authenticator.AuthenticateOperator(operatorAuthRequest("caller-token"), "get")
	assertHTTPStatus(t, err, http.StatusUnauthorized)
	if len(*sars) != 0 {
		t.Fatal("SubjectAccessReview must not run for an unauthenticated token")
	}

	_, err = authenticator.AuthenticateOperator(operatorAuthRequest(""), "get")
	assertHTTPStatus(t, err, http.StatusUnauthorized)
}

func TestOperatorAuthenticatorReportsAPIServerOutage(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver down")
	})
	authenticator, err := NewOperator(client, "fleet")
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.AuthenticateOperator(operatorAuthRequest("caller-token"), "get")
	assertHTTPStatus(t, err, http.StatusServiceUnavailable)
}

func assertHTTPStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var statusErr httpapi.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != want {
		t.Fatalf("error = %v, want HTTP status %d", err, want)
	}
}
