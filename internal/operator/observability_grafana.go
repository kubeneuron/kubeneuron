// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	dashboards "github.com/kubeneuron/kubeneuron/deploy/grafana"
)

// A Managed Grafana is the official open-source image (AGPL-3.0), unmodified,
// referenced by digest and pulled from Docker Hub (grafana/grafana): KubeNeuron
// does not build, copy or redistribute it and loads no plugins into it.
// --grafana-image overrides it for a mirror.
const DefaultGrafanaImage = "grafana/grafana:12.4.13@sha256:41bbc1b5e04416f81891806076db2a2ff60a5d4da5f78d36a88b033a8c199185"

const (
	grafanaComponent        = "grafana"
	grafanaPort             = 3000
	grafanaReadyCondition   = "ObservabilityGrafanaReady"
	grafanaDatasourceUID    = "kubeneuron-metrics"
	grafanaAdminUserKey     = "admin-user"
	grafanaAdminPasswordKey = "admin-password"
	grafanaAdminUser        = "admin"
	// grafanaAuthorizationEnv carries an External store's Authorization
	// header into the datasource provisioning, which Grafana expands.
	grafanaAuthorizationEnv = "KUBENEURON_DATASOURCE_AUTHORIZATION"
	grafanaProvisioningPath = "/etc/grafana/provisioning"
	grafanaDashboardsPath   = "/etc/grafana/dashboards"
	grafanaDataPath         = "/var/lib/grafana"
)

func grafanaName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	return installation.Name + "-" + grafanaComponent
}

func grafanaAdminName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	return grafanaName(installation) + "-admin"
}

func grafanaDashboardsName(installation *kubeneuronv1alpha1.KubeNeuron) string {
	return grafanaName(installation) + "-dashboards"
}

func grafanaMode(installation *kubeneuronv1alpha1.KubeNeuron) kubeneuronv1alpha1.IntegrationMode {
	if g := installation.Spec.Observability.Grafana; g != nil {
		return g.Mode
	}
	return kubeneuronv1alpha1.IntegrationModeDisabled
}

func (r *KubeNeuronReconciler) grafanaImage() string {
	if r.GrafanaImage != "" {
		return r.GrafanaImage
	}
	return DefaultGrafanaImage
}

// grafanaDatasourceURL is the store Grafana queries: the Managed store, or
// the query address named for an External one.
func grafanaDatasourceURL(installation *kubeneuronv1alpha1.KubeNeuron) string {
	if storeMode(installation) == kubeneuronv1alpha1.IntegrationModeManaged {
		return serviceURL(installation, storeName(installation), storePort)
	}
	if g := installation.Spec.Observability.Grafana; g != nil && g.Datasource != nil {
		return g.Datasource.URL
	}
	return ""
}

func grafanaDatasourceAuthorization(installation *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.SecretReference {
	if storeMode(installation) != kubeneuronv1alpha1.IntegrationModeExternal {
		return nil
	}
	if g := installation.Spec.Observability.Grafana; g != nil && g.Datasource != nil {
		return g.Datasource.AuthorizationSecretRef
	}
	return nil
}

// grafanaProvisioning is the datasource and the dashboard provider. The
// Authorization header, when there is one, stays a reference to an
// environment variable: no credential is written into the ConfigMap.
func grafanaProvisioning(installation *kubeneuronv1alpha1.KubeNeuron) (*corev1.ConfigMap, error) {
	datasource := map[string]any{
		"name": "KubeNeuron metrics", "uid": grafanaDatasourceUID, "type": "prometheus", "access": "proxy",
		"url": grafanaDatasourceURL(installation), "isDefault": true, "editable": false,
		"jsonData": map[string]any{"timeInterval": "30s"},
	}
	if grafanaDatasourceAuthorization(installation) != nil {
		datasource["jsonData"].(map[string]any)["httpHeaderName1"] = "Authorization"
		datasource["secureJsonData"] = map[string]any{"httpHeaderValue1": "${" + grafanaAuthorizationEnv + "}"}
	}
	datasources, err := sigsyaml.Marshal(map[string]any{"apiVersion": 1, "datasources": []any{datasource}})
	if err != nil {
		return nil, err
	}
	providers, err := sigsyaml.Marshal(map[string]any{"apiVersion": 1, "providers": []any{map[string]any{
		"name": "kubeneuron", "type": "file", "disableDeletion": true,
		// Edits made in the Grafana screen are not kept: the files win.
		"allowUiUpdates": false, "updateIntervalSeconds": 60,
		"options": map[string]any{"path": grafanaDashboardsPath, "foldersFromFilesStructure": false},
	}}})
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: grafanaName(installation), Namespace: installation.Spec.Namespace, Labels: resourceLabels(installation, grafanaComponent)},
		Data:       map[string]string{"datasources.yaml": string(datasources), "dashboards.yaml": string(providers)},
	}, nil
}

// grafanaDashboards holds every KubeNeuron dashboard bound to the datasource.
func grafanaDashboards(installation *kubeneuronv1alpha1.KubeNeuron) (*corev1.ConfigMap, error) {
	names, err := fs.Glob(dashboards.Files, "*.json")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	data := make(map[string]string, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(dashboards.Files, name)
		if err != nil {
			return nil, err
		}
		data[name] = strings.ReplaceAll(string(raw), dashboards.DatasourcePlaceholder, grafanaDatasourceUID)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: grafanaDashboardsName(installation), Namespace: installation.Spec.Namespace, Labels: resourceLabels(installation, grafanaComponent)},
		Data:       data,
	}, nil
}

func generatePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ensureGrafanaAdmin creates the admin account's Secret once, with a random
// password. It belongs to the installation and goes with it; a value the
// user has changed is never overwritten.
func (r *KubeNeuronReconciler) ensureGrafanaAdmin(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: grafanaAdminName(installation), Namespace: installation.Spec.Namespace}, &existing)
	if err == nil {
		if !metav1.IsControlledBy(&existing, installation) {
			return fmt.Errorf("secret %s exists but is not controlled by this installation", existing.Name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	password, err := generatePassword()
	if err != nil {
		return fmt.Errorf("generate the grafana admin password: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: grafanaAdminName(installation), Namespace: installation.Spec.Namespace, Labels: resourceLabels(installation, grafanaComponent)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{grafanaAdminUserKey: []byte(grafanaAdminUser), grafanaAdminPasswordKey: []byte(password)},
	}
	if err := setOwner(r.Scheme, installation, secret); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

func plainEnv(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key},
	}}
}

// grafanaEnv turns off everything that would reach outside the cluster or
// expose a dashboard without a login.
func grafanaEnv(installation *kubeneuronv1alpha1.KubeNeuron) []corev1.EnvVar {
	env := []corev1.EnvVar{
		secretEnv("GF_SECURITY_ADMIN_USER", grafanaAdminName(installation), grafanaAdminUserKey),
		secretEnv("GF_SECURITY_ADMIN_PASSWORD", grafanaAdminName(installation), grafanaAdminPasswordKey),
		plainEnv("GF_SERVER_HTTP_PORT", strconv.Itoa(grafanaPort)),
		plainEnv("GF_PATHS_DATA", grafanaDataPath),
		plainEnv("GF_PATHS_PLUGINS", grafanaDataPath+"/plugins"),
		plainEnv("GF_PATHS_PROVISIONING", grafanaProvisioningPath),
		plainEnv("GF_LOG_MODE", "console"),
		plainEnv("GF_AUTH_ANONYMOUS_ENABLED", "false"),
		plainEnv("GF_USERS_ALLOW_SIGN_UP", "false"),
		plainEnv("GF_USERS_ALLOW_ORG_CREATE", "false"),
		plainEnv("GF_ANALYTICS_REPORTING_ENABLED", "false"),
		plainEnv("GF_ANALYTICS_CHECK_FOR_UPDATES", "false"),
		plainEnv("GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES", "false"),
		plainEnv("GF_ANALYTICS_FEEDBACK_LINKS_ENABLED", "false"),
		plainEnv("GF_PLUGINS_PREINSTALL_DISABLED", "true"),
		plainEnv("GF_PLUGINS_PLUGIN_ADMIN_ENABLED", "false"),
		plainEnv("GF_PLUGINS_PUBLIC_KEY_RETRIEVAL_DISABLED", "true"),
		plainEnv("GF_NEWS_NEWS_FEED_ENABLED", "false"),
		plainEnv("GF_SECURITY_DISABLE_GRAVATAR", "true"),
		plainEnv("GF_SNAPSHOTS_EXTERNAL_ENABLED", "false"),
		plainEnv("GF_PUBLIC_DASHBOARDS_ENABLED", "false"),
	}
	if ref := grafanaDatasourceAuthorization(installation); ref != nil {
		env = append(env, secretEnv(grafanaAuthorizationEnv, ref.Name, ref.Key))
	}
	return env
}

func grafanaDeployment(installation *kubeneuronv1alpha1.KubeNeuron, image, configDigest string) *appsv1.Deployment {
	labels := resourceLabels(installation, grafanaComponent)
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	if g := installation.Spec.Observability.Grafana; g != nil && g.Resources != nil {
		resources = *g.Resources.DeepCopy()
	}
	configMapVolume := func(name, configMap string, items []corev1.KeyToPath) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMap}, Items: items,
			DefaultMode: ptr.To(corev1.ConfigMapVolumeSourceDefaultMode),
		}}}
	}
	container := corev1.Container{
		Name:            "grafana",
		Image:           image,
		ImagePullPolicy: defaultImagePullPolicy(image),
		Env:             grafanaEnv(installation),
		Resources:       resources,
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: grafanaPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe:  defaultReadinessHTTPProbe("/api/health", "http"),
		LivenessProbe:   defaultHTTPProbe("/api/health", "http"),
		SecurityContext: restrictedContainerSecurity(),
		// Everything Grafana keeps is provisioned, so its database is an
		// emptyDir: each start reads the admin password from the Secret and
		// the dashboards from the files again.
		VolumeMounts: []corev1.VolumeMount{
			{Name: "provisioning", MountPath: grafanaProvisioningPath, ReadOnly: true},
			{Name: "dashboards", MountPath: grafanaDashboardsPath, ReadOnly: true},
			{Name: "data", MountPath: grafanaDataPath},
			{Name: "tmp", MountPath: "/tmp"},
		},
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: grafanaName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.To(int32(1)),
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			RevisionHistoryLimit:    ptr.To(int32(defaultRevisionHistoryLimit)),
			ProgressDeadlineSeconds: ptr.To(int32(defaultProgressDeadline)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"kubeneuron.io/config-digest": configDigest}},
				Spec: corev1.PodSpec{
					ServiceAccountName:            grafanaName(installation),
					DeprecatedServiceAccount:      grafanaName(installation),
					AutomountServiceAccountToken:  ptr.To(false),
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: ptr.To(int64(defaultTerminationGrace)),
					DNSPolicy:                     corev1.DNSClusterFirst,
					SecurityContext:               restrictedPodSecurity(),
					SchedulerName:                 corev1.DefaultSchedulerName,
					Containers:                    []corev1.Container{container},
					Volumes: []corev1.Volume{
						configMapVolume("provisioning", grafanaName(installation), []corev1.KeyToPath{
							{Key: "datasources.yaml", Path: "datasources/kubeneuron.yaml"},
							{Key: "dashboards.yaml", Path: "dashboards/kubeneuron.yaml"},
						}),
						configMapVolume("dashboards", grafanaDashboardsName(installation), nil),
						{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

func grafanaService(installation *kubeneuronv1alpha1.KubeNeuron) *corev1.Service {
	return clusterIPService(installation, grafanaName(installation), grafanaComponent, "http", grafanaPort)
}

// grafanaNetworkPolicy admits the installation namespace only and lets
// Grafana reach DNS and the store it reads. An External store's address
// cannot be named, so its port is open to any destination.
func grafanaNetworkPolicy(installation *kubeneuronv1alpha1.KubeNeuron) (*networkingv1.NetworkPolicy, error) {
	labels := resourceLabels(installation, grafanaComponent)
	egress := []networkingv1.NetworkPolicyEgressRule{dnsEgress()}
	if storeMode(installation) == kubeneuronv1alpha1.IntegrationModeManaged {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: resourceLabels(installation, storeComponent)}}},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(storePort)},
		})
	} else {
		port, err := endpointPort(grafanaDatasourceURL(installation))
		if err != nil {
			return nil, fmt.Errorf("spec.observability.grafana.datasource.url: %w", err)
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{tcpPort(port)}})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: grafanaName(installation), Namespace: installation.Spec.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(grafanaPort)},
			}},
			Egress: egress,
		},
	}, nil
}

func (r *KubeNeuronReconciler) ensureGrafana(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	if err := r.ensureGrafanaAdmin(ctx, installation); err != nil {
		return fmt.Errorf("reconcile grafana admin Secret: %w", err)
	}
	provisioning, err := grafanaProvisioning(installation)
	if err != nil {
		return err
	}
	boards, err := grafanaDashboards(installation)
	if err != nil {
		return err
	}
	for _, configMap := range []*corev1.ConfigMap{provisioning, boards} {
		if err := r.ensureConfigMap(ctx, installation, configMap); err != nil {
			return fmt.Errorf("reconcile grafana ConfigMap %s: %w", configMap.Name, err)
		}
	}
	if err := r.ensureServiceAccount(ctx, installation, observabilityServiceAccount(installation, grafanaName(installation), grafanaComponent)); err != nil {
		return fmt.Errorf("reconcile grafana ServiceAccount: %w", err)
	}
	policy, err := grafanaNetworkPolicy(installation)
	if err != nil {
		return err
	}
	if err := r.ensureNetworkPolicy(ctx, installation, policy); err != nil {
		return fmt.Errorf("reconcile grafana NetworkPolicy: %w", err)
	}
	if err := r.ensureService(ctx, installation, grafanaService(installation)); err != nil {
		return fmt.Errorf("reconcile grafana Service: %w", err)
	}
	// The digest rolls Grafana when the datasource or a dashboard changes.
	digest := sha256Hex([]byte(configMapContent(provisioning) + configMapContent(boards)))
	if err := r.ensureDeployment(ctx, installation, grafanaDeployment(installation, r.grafanaImage(), digest)); err != nil {
		return fmt.Errorf("reconcile grafana Deployment: %w", err)
	}
	return nil
}

func configMapContent(configMap *corev1.ConfigMap) string {
	keys := make([]string, 0, len(configMap.Data))
	for k := range configMap.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "\x00" + configMap.Data[k] + "\x00")
	}
	return b.String()
}

// removeGrafana removes a Managed Grafana. Its admin Secret stays until the
// installation is deleted, which garbage-collects it: the operator has no
// delete on Secrets, and a password with no Grafana behind it opens nothing.
func (r *KubeNeuronReconciler) removeGrafana(ctx context.Context, installation *kubeneuronv1alpha1.KubeNeuron) error {
	name := grafanaName(installation)
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}, &corev1.ServiceAccount{}, &corev1.ConfigMap{}} {
		if err := r.removeOwned(ctx, installation, object, name); err != nil {
			return fmt.Errorf("remove grafana: %w", err)
		}
	}
	if err := r.removeOwned(ctx, installation, &corev1.ConfigMap{}, grafanaDashboardsName(installation)); err != nil {
		return fmt.Errorf("remove grafana: %w", err)
	}
	return nil
}
