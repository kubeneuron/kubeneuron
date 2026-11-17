// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	vmalertrules "github.com/kubeneuron/kubeneuron/configs/vmalert"
)

// A Managed store comes with a vmalert that evaluates the GPU recording
// rules into it, so the dashboards' kubeneuron_gpu_* series exist. It has no
// alerting rules and no notifier: it reads from and writes to that store
// only. Apache-2.0, referenced by digest and pulled from its publisher;
// --metrics-rules-image overrides it for a mirror.
const DefaultMetricsRulesImage = "victoriametrics/vmalert:v1.147.0@sha256:45ea994098b77d56904f3d5d7cb2771effad9ddefb1d07d4ffa0c1da4c3e4c5c"

const (
	rulesComponent      = "metrics-rules"
	rulesPort           = 8880
	rulesKey            = "recording-rules.yaml"
	rulesMountPath      = "/etc/vmalert"
	rulesReadyCondition = "ObservabilityRulesReady"
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
		Data:       map[string]string{rulesKey: vmalertrules.Recording},
	}
}

func rulesDeployment(installation *kubeneuronv1alpha1.KubeNeuron, image, rulesDigest string) *appsv1.Deployment {
	labels := resourceLabels(installation, rulesComponent)
	store := serviceURL(installation, storeName(installation), storePort)
	container := corev1.Container{
		Name:            "vmalert",
		Image:           image,
		ImagePullPolicy: defaultImagePullPolicy(image),
		Args: []string{
			"-httpListenAddr=:" + strconv.Itoa(rulesPort),
			"-rule=" + rulesMountPath + "/" + rulesKey,
			"-datasource.url=" + store,
			"-remoteWrite.url=" + store,
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
		VolumeMounts:             []corev1.VolumeMount{{Name: "rules", MountPath: rulesMountPath, ReadOnly: true}},
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
					Volumes: []corev1.Volume{{Name: "rules", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: rulesName(installation)},
						DefaultMode:          ptr.To(corev1.ConfigMapVolumeSourceDefaultMode),
					}}}},
				},
			},
		},
	}
}

// rulesNetworkPolicy admits nothing and lets vmalert reach DNS and the store.
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
	digest := sha256Hex([]byte(configMap.Data[rulesKey]))
	if err := r.ensureDeployment(ctx, installation, rulesDeployment(installation, r.metricsRulesImage(), digest)); err != nil {
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
