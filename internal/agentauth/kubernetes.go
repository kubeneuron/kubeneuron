// Package agentauth authenticates controller requests made by Kubernetes node
// agents. A fleet mTLS certificate proves installation membership; a
// Pod-bound ServiceAccount token supplies the exact Pod and node identity.
package agentauth

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubeneuron/kubeneuron/internal/httpapi"
)

const (
	podNameExtra = "authentication.kubernetes.io/pod-name"
	podUIDExtra  = "authentication.kubernetes.io/pod-uid"
)

// Config is the fixed installation identity an agent request must match.
type Config struct {
	Audience          string
	Namespace         string
	ServiceAccount    string
	AgentDaemonSet    string
	InstallationName  string
	InstallationUID   string
	MaximumCertPeriod time.Duration
}

// Authenticator validates projected tokens using the Kubernetes API.
type Authenticator struct {
	client kubernetes.Interface
	cfg    Config
	now    func() time.Time
}

// New builds a fail-closed Kubernetes agent authenticator.
func New(client kubernetes.Interface, cfg Config) (*Authenticator, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}
	for name, value := range map[string]string{
		"audience":          cfg.Audience,
		"namespace":         cfg.Namespace,
		"service account":   cfg.ServiceAccount,
		"agent DaemonSet":   cfg.AgentDaemonSet,
		"installation name": cfg.InstallationName,
		"installation UID":  cfg.InstallationUID,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	if cfg.MaximumCertPeriod == 0 {
		cfg.MaximumCertPeriod = 100 * 24 * time.Hour
	}
	if cfg.MaximumCertPeriod < 0 {
		return nil, fmt.Errorf("maximum certificate period must be positive")
	}
	return &Authenticator{client: client, cfg: cfg, now: time.Now}, nil
}

// AuthenticateAgent implements httpapi.AgentAuthenticator.
func (a *Authenticator) AuthenticateAgent(r *http.Request) (httpapi.AgentPrincipal, error) {
	if err := a.verifyTLSIdentity(r); err != nil {
		return httpapi.AgentPrincipal{}, err
	}
	token, err := bearerToken(r)
	if err != nil {
		return httpapi.AgentPrincipal{}, err
	}

	review, err := a.client.AuthenticationV1().TokenReviews().Create(r.Context(), &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{a.cfg.Audience},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return httpapi.AgentPrincipal{}, unavailable("TokenReview failed", err)
	}
	if !review.Status.Authenticated || !contains(review.Status.Audiences, a.cfg.Audience) {
		return httpapi.AgentPrincipal{}, unauthorized("token is not authenticated for the agent audience")
	}
	wantUsername := "system:serviceaccount:" + a.cfg.Namespace + ":" + a.cfg.ServiceAccount
	if review.Status.User.Username != wantUsername {
		return httpapi.AgentPrincipal{}, forbidden("token belongs to an unexpected ServiceAccount")
	}
	podName, ok := singleExtra(review.Status.User.Extra, podNameExtra)
	if !ok {
		return httpapi.AgentPrincipal{}, forbidden("token has no unique Pod name binding")
	}
	podUID, ok := singleExtra(review.Status.User.Extra, podUIDExtra)
	if !ok {
		return httpapi.AgentPrincipal{}, forbidden("token has no unique Pod UID binding")
	}

	serviceAccount, err := a.client.CoreV1().ServiceAccounts(a.cfg.Namespace).Get(r.Context(), a.cfg.ServiceAccount, metav1.GetOptions{})
	if err != nil {
		return httpapi.AgentPrincipal{}, lookupError("ServiceAccount", err)
	}
	if serviceAccount.DeletionTimestamp != nil || string(serviceAccount.UID) != review.Status.User.UID {
		return httpapi.AgentPrincipal{}, forbidden("token ServiceAccount UID is not current")
	}

	pod, err := a.client.CoreV1().Pods(a.cfg.Namespace).Get(r.Context(), podName, metav1.GetOptions{})
	if err != nil {
		return httpapi.AgentPrincipal{}, lookupError("Pod", err)
	}
	if string(pod.UID) != podUID || pod.DeletionTimestamp != nil {
		return httpapi.AgentPrincipal{}, forbidden("token Pod UID is not current")
	}
	if pod.Spec.ServiceAccountName != a.cfg.ServiceAccount {
		return httpapi.AgentPrincipal{}, forbidden("Pod uses an unexpected ServiceAccount")
	}
	if pod.Status.Phase != "Running" {
		return httpapi.AgentPrincipal{}, forbidden("Pod is not running")
	}
	if pod.Spec.NodeName == "" {
		return httpapi.AgentPrincipal{}, forbidden("Pod is not bound to a node")
	}
	if pod.Labels["app.kubernetes.io/name"] != "kube-neuron" ||
		pod.Labels["app.kubernetes.io/instance"] != a.cfg.InstallationName ||
		pod.Labels["app.kubernetes.io/component"] != "agent" ||
		pod.Labels["app.kubernetes.io/managed-by"] != "kubeneuron-operator" {
		return httpapi.AgentPrincipal{}, forbidden("Pod labels do not identify the expected agent")
	}

	daemonSet, err := a.client.AppsV1().DaemonSets(a.cfg.Namespace).Get(r.Context(), a.cfg.AgentDaemonSet, metav1.GetOptions{})
	if err != nil {
		return httpapi.AgentPrincipal{}, lookupError("DaemonSet", err)
	}
	if daemonSet.DeletionTimestamp != nil {
		return httpapi.AgentPrincipal{}, forbidden("agent DaemonSet is terminating")
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" ||
		owner.Name != daemonSet.Name || owner.UID != daemonSet.UID {
		return httpapi.AgentPrincipal{}, forbidden("Pod is not controlled by the expected agent DaemonSet")
	}

	node, err := a.client.CoreV1().Nodes().Get(r.Context(), pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return httpapi.AgentPrincipal{}, lookupError("Node", err)
	}
	if node.DeletionTimestamp != nil {
		return httpapi.AgentPrincipal{}, forbidden("Node is terminating")
	}

	return httpapi.AgentPrincipal{
		Namespace:      a.cfg.Namespace,
		ServiceAccount: a.cfg.ServiceAccount,
		PodName:        pod.Name,
		PodUID:         string(pod.UID),
		NodeName:       node.Name,
		NodeUID:        string(node.UID),
	}, nil
}

func (a *Authenticator) verifyTLSIdentity(r *http.Request) error {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return unauthorized("verified client certificate is required")
	}
	leaf := r.TLS.PeerCertificates[0]
	now := a.now()
	if leaf.IsCA || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return forbidden("client certificate is not a current leaf")
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > a.cfg.MaximumCertPeriod {
		return forbidden("client certificate lifetime exceeds the installation policy")
	}
	if !hasUsage(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return forbidden("client certificate is not valid for client authentication")
	}
	wantURI := &url.URL{
		Scheme: "spiffe",
		Host:   "kubeneuron.io",
		Path:   "/installation/" + a.cfg.InstallationUID + "/agent",
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != wantURI.String() {
		return forbidden("client certificate belongs to another installation")
	}
	return nil
}

func bearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", unauthorized("exactly one bearer credential is required")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(values[0], prefix) {
		return "", unauthorized("bearer credential is malformed")
	}
	token := strings.TrimSpace(strings.TrimPrefix(values[0], prefix))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", unauthorized("bearer credential is malformed")
	}
	return token, nil
}

func singleExtra(extra map[string]authenticationv1.ExtraValue, key string) (string, bool) {
	values := extra[key]
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", false
	}
	return values[0], true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasUsage(values []x509.ExtKeyUsage, want x509.ExtKeyUsage) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func lookupError(kind string, err error) error {
	if apierrors.IsNotFound(err) {
		return forbidden(kind + " identity no longer exists")
	}
	return unavailable(kind+" lookup failed", err)
}

type authError struct {
	status int
	msg    string
	cause  error
}

func (e *authError) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *authError) Unwrap() error   { return e.cause }
func (e *authError) HTTPStatus() int { return e.status }

func unauthorized(message string) error {
	return &authError{status: http.StatusUnauthorized, msg: message}
}

func forbidden(message string) error {
	return &authError{status: http.StatusForbidden, msg: message}
}

func unavailable(message string, cause error) error {
	return &authError{status: http.StatusServiceUnavailable, msg: message, cause: cause}
}

var _ httpapi.AgentAuthenticator = (*Authenticator)(nil)
