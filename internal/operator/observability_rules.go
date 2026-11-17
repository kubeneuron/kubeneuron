// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	vmalertrules "github.com/kubeneuron/kubeneuron/configs/vmalert"
	"github.com/kubeneuron/kubeneuron/internal/inventory"
)

// A Managed store comes with a vmalert that evaluates the GPU recording
// rules into it, so the dashboards' kubeneuron_gpu_* series exist, and the
// alerting rules for the faults that reach KubeNeuron on NVIDIA only as
// metric alerts. Its one notifier is the controller, with the webhook token
// as a bearer header; it reaches nothing else but that store. Apache-2.0, referenced by digest and pulled from its publisher;
// --metrics-rules-image overrides it for a mirror.
const DefaultMetricsRulesImage = "victoriametrics/vmalert:v1.147.0@sha256:45ea994098b77d56904f3d5d7cb2771effad9ddefb1d07d4ffa0c1da4c3e4c5c"

const (
	rulesComponent      = "metrics-rules"
	rulesPort           = 8880
	rulesKey            = "recording-rules.yaml"
	alertingKey         = "alerting-rules.yaml"
	rulesMountPath      = "/etc/vmalert"
	rulesReadyCondition = "ObservabilityRulesReady"
	// alertDetectionCondition says whether metric alerts reach the controller.
	alertDetectionCondition = "AlertDetectionConfigured"
	// vmalertWebhookPath is the notifier base; vmalert appends /api/v2/alerts.
	vmalertWebhookPath = "/api/v1/webhooks/vmalert"
	notifierTokenDir   = "/etc/vmalert-notifier/token"
	notifierCADir      = "/etc/vmalert-notifier/ca"
)

func rulesName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	return installation.Name + "-" + rulesComponent
}

func (r *KubeNeuronReconciler) metricsRulesImage() string {
	if r.MetricsRulesImage != "" {
		return r.MetricsRulesImage
	}
	return DefaultMetricsRulesImage
}

func rulesConfigMap(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rulesName(installation), Namespace: installation.Spec.Namespace, Labels: resourceLabels(installation, rulesComponent)},
		Data:       map[string]string{rulesKey: vmalertrules.Recording, alertingKey: vmalertrules.Alerting},
	}
}

// notifierURL is the controller's vmalert webhook, over TLS when the
// controller's public listener serves it.
func notifierURL(installation *kubeneuronv1alpha1.KubeNeuron) string {
	scheme := "http://"
	if installation.Spec.TLS.PublicServerSecretRef != nil {
		scheme = "https://"
	}
	return scheme + installation.Name + "-controller." + installation.Spec.Namespace + ".svc:" +
		strconv.Itoa(controllerPort) + vmalertWebhookPath
}

// rulesDeployment runs vmalert. notifierCA mounts the ca.crt of the
// controller's public TLS Secret, when it has one, to verify the controller.
func rulesDeployment(installation *kubeneuronv1alpha1.KubeNeuron, image, rulesDigest string, notifierCA bool) *appsv1.Deployment {
	labels := resourceLabels(installation, rulesComponent)
	store := serviceURL(installation, storeName(installation), storePort)
	container := corev1.Container{
		Name:            "vmalert",
		Image:           image,
		ImagePullPolicy: defaultImagePullPolicy(image),
		Args: []string{
			"-httpListenAddr=:" + strconv.Itoa(rulesPort),
			"-rule=" + rulesMountPath + "/" + rulesKey,
			"-rule=" + rulesMountPath + "/" + alertingKey,
			"-datasource.url=" + store,
			"-remoteWrite.url=" + store,
			"-notifier.url=" + notifierURL(installation),
			"-notifier.bearerTokenFile=" + notifierTokenDir + "/token",
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		Ports:                    []corev1.ContainerPort{{Name: "http", ContainerPort: rulesPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe:           defaultReadinessHTTPProbe("/health", "http"),
		LivenessProbe:            defaultHTTPProbe("/health", "http"),
		SecurityContext:          restrictedContainerSecurity(),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "rules", MountPath: rulesMountPath, ReadOnly: true},
			{Name: "notifier-token", MountPath: notifierTokenDir, ReadOnly: true},
		},
	}
	volumes := []corev1.Volume{
		{Name: "rules", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: rulesName(installation)},
			DefaultMode:          ptr.To(corev1.ConfigMapVolumeSourceDefaultMode),
		}}},
	}
	// The token is mandatory on every installation; it is the same Secret
	// the controller checks the bearer against, read from a file so it is
	// neither in the URL nor in vmalert's flags.
	if ref := webhookTokenRef(installation); ref != nil {
		volumes = append(volumes, corev1.Volume{Name: "notifier-token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  ref.Name,
			DefaultMode: ptr.To(int32(defaultSecretMode)),
			Items:       []corev1.KeyToPath{{Key: tokenSecretKey(ref), Path: "token"}},
		}}})
	}
	if ref := installation.Spec.TLS.PublicServerSecretRef; ref != nil && notifierCA {
		container.Args = append(container.Args, "-notifier.tlsCAFile="+notifierCADir+"/ca.crt")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "notifier-ca", MountPath: notifierCADir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "notifier-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  ref.Name,
			DefaultMode: ptr.To(int32(defaultSecretMode)),
			Items:       []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
		}}})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: rulesName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.To(int32(1)),
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			RevisionHistoryLimit:    ptr.To(int32(defaultRevisionHistoryLimit)),
			ProgressDeadlineSeconds: ptr.To(int32(defaultProgressDeadline)),
			Template: corev1.PodTemplateSpec{
				// The digest rolls vmalert when the rule file changes.
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"kubeneuron.io/config-digest": rulesDigest}},
				Spec: corev1.PodSpec{
					ServiceAccountName:            rulesName(installation),
					DeprecatedServiceAccount:      rulesName(installation),
					AutomountServiceAccountToken:  ptr.To(false),
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: ptr.To(int64(defaultTerminationGrace)),
					DNSPolicy:                     corev1.DNSClusterFirst,
					SecurityContext:               restrictedPodSecurity(),
					SchedulerName:                 corev1.DefaultSchedulerName,
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
				},
			},
		},
	}
}

// rulesNetworkPolicy admits nothing and lets vmalert reach DNS, the store
// and the controller's public port.
func rulesNetworkPolicy(installation *kubeneuronv1alpha1.KubeNeuron) *networkingv1.NetworkPolicy {
	labels := resourceLabels(installation, rulesComponent)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: rulesName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{dnsEgress(), {
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, storeComponent)}}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(storePort)},
			}, {
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, "controller")}}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(controllerPort)},
			}},
		},
	}
}

func (r *KubeNeuronReconciler) ensureRules(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	configMap := rulesConfigMap(installation)
	if err := r.ensureConfigMap(ctx, installation, configMap); err != nil {
		return fmt.Errorf("reconcile metrics rules ConfigMap: %w", err)
	}
	if err := r.ensureServiceAccount(ctx, installation, observabilityServiceAccount(installation, rulesName(installation), rulesComponent)); err != nil {
		return fmt.Errorf("reconcile metrics rules ServiceAccount: %w", err)
	}
	if err := r.ensureNetworkPolicy(ctx, installation, rulesNetworkPolicy(installation)); err != nil {
		return fmt.Errorf("reconcile metrics rules NetworkPolicy: %w", err)
	}
	notifierCA, err := r.publicServerHasCA(ctx, installation)
	if err != nil {
		return err
	}
	digest := sha256Hex([]byte(configMap.Data[rulesKey] + "\x00" + configMap.Data[alertingKey]))
	if err := r.ensureDeployment(ctx, installation, rulesDeployment(installation, r.metricsRulesImage(), digest, notifierCA)); err != nil {
		return fmt.Errorf("reconcile metrics rules Deployment: %w", err)
	}
	return nil
}

func (r *KubeNeuronReconciler) removeRules(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	name := rulesName(installation)
	for _, object := range []client.Object{&appsv1.Deployment{}, &networkingv1.NetworkPolicy{}, &corev1.ServiceAccount{}, &corev1.ConfigMap{}} {
		if err := r.removeOwned(ctx, installation, object, name); err != nil {
			return fmt.Errorf("remove metrics rules: %w", err)
		}
	}
	return nil
}

// publicServerHasCA reports whether the controller's public TLS Secret
// carries a ca.crt for vmalert to verify the controller with. Without one
// vmalert trusts the system roots.
func (r *KubeNeuronReconciler) publicServerHasCA(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) (bool, error) {
	ref := installation.Spec.TLS.PublicServerSecretRef
	if ref == nil {
		return false, nil
	}
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: installation.Spec.Namespace}, &secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the controller's public TLS Secret: %w", err)
	}
	return len(secret.Data["ca.crt"]) > 0, nil
}

// firstAlertAccepted reads when the controller first accepted a metric
// alert from the installation's inventory summary. It returns nil until then,
// and when the summary cannot be read or is not the installation's.
func (r *KubeNeuronReconciler) firstAlertAccepted(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) *time.Time {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: installation.Spec.Namespace, Name: installation.Name + inventory.Suffix}
	if err := r.Get(ctx, key, &cm); err != nil || !metav1.IsControlledBy(&cm, installation) {
		return nil
	}
	at, ok := inventory.FirstAlert(cm.Data)
	if !ok {
		return nil
	}
	return &at
}

// alertDetection says whether metric alerts reach the controller: with the
// Managed store when vmalert with the alerting rules is available; with an
// External store they are the user's to run and point at the webhook, and
// the condition turns True once the controller has accepted one.
func alertDetection(installation *kubeneuronv1alpha1.KubeNeuron, rules *metav1.Condition, firstAlert *time.Time) *metav1.Condition {
	switch storeMode(installation) {
	case kubeneuronv1alpha1.IntegrationModeManaged:
		if rules == nil || rules.Status != metav1.ConditionTrue {
			return notReady("RulesNotReady", "alert-based detection waits for the metrics rules evaluator")
		}
		return &metav1.Condition{Status: metav1.ConditionTrue, Reason: "ManagedRules",
			Message: "the managed vmalert evaluates the alerting rules and sends them to the controller"}
	case kubeneuronv1alpha1.IntegrationModeExternal:
		if firstAlert != nil {
			return &metav1.Condition{Status: metav1.ConditionTrue, Reason: "AlertReceived",
				Message: "the controller accepted its first metric alert at " + firstAlert.UTC().Format(time.RFC3339)}
		}
		return notReady("ExternalStore",
			"alert-based detection is not configured: run the alert rules against your store and point them at the controller webhook")
	default:
		return nil
	}
}
