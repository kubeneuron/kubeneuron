package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/cloud"
	"github.com/kubeneuron/kubeneuron/internal/config"
)

func testKubeNeuron() *kubeneuronv1alpha1.KubeNeuron {
	return &kubeneuronv1alpha1.KubeNeuron{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet", UID: "installation-uid"},
		Spec: kubeneuronv1alpha1.KubeNeuronSpec{
			Namespace:  "kube-neuron-system",
			Controller: kubeneuronv1alpha1.ComponentSpec{Image: "example/controller:v1"},
			Agent:      kubeneuronv1alpha1.AgentSpec{Image: "example/agent:v1"},
			WorkflowStore: kubeneuronv1alpha1.WorkflowStoreSpec{
				Type:   "SQLite",
				SQLite: &kubeneuronv1alpha1.SQLiteStoreSpec{Size: defaultSQLiteStorageSize},
			},
			Observability: kubeneuronv1alpha1.ObservabilitySpec{
				VictoriaMetrics: kubeneuronv1alpha1.DependencySpec{Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://vm.example"},
				Alertmanager:    kubeneuronv1alpha1.DependencySpec{Mode: kubeneuronv1alpha1.IntegrationModeExternal, Endpoint: "http://am.example"},
			},
			Notifications: &kubeneuronv1alpha1.NotificationsSpec{
				OperatorAPIToken: &kubeneuronv1alpha1.SecretReference{Name: "fleet-api-token"},
				WebhookToken:     &kubeneuronv1alpha1.SecretReference{Name: "fleet-webhook-token"},
			},
			TLS: kubeneuronv1alpha1.TLSSpec{
				ServerSecretRef:   &kubeneuronv1alpha1.SecretReference{Name: "fleet-controller-tls"},
				ClientCASecretRef: &kubeneuronv1alpha1.SecretReference{Name: "fleet-agent-client-ca"},
				ClientSecretRef:   &kubeneuronv1alpha1.SecretReference{Name: "fleet-agent-tls"},
				ServerCASecretRef: &kubeneuronv1alpha1.SecretReference{Name: "fleet-controller-server-ca"},
			},
		},
	}
}

func TestCompileSnapshotDefaultsToDryRun(t *testing.T) {
	installation := testKubeNeuron()
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-reset"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: "fleet",
			Target:        kubeneuronv1alpha1.PlaybookTargetGPU,
			Cooldown:      "30m",
			Steps: []kubeneuronv1alpha1.PlaybookStep{
				{Name: "idle", Action: kubeneuronv1alpha1.ActionIdleCheck, Timeout: "2m"},
				{Name: "reset", Action: kubeneuronv1alpha1.ActionGPUReset, Timeout: "5m"},
			},
		},
	}}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "gsp"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: "fleet",
			Priority:      10,
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "gsp-error"},
			PlaybookRef:   "gpu-reset",
		},
	}}

	snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	if !strings.Contains(string(snapshot.PoliciesYAML), "dry_run: true") {
		t.Fatalf("policies must default to dry-run, got:\n%s", snapshot.PoliciesYAML)
	}
	var compiled config.Config
	if err := yaml.Unmarshal(snapshot.PoliciesYAML, &compiled); err != nil {
		t.Fatalf("unmarshal compiled policies: %v", err)
	}
	if compiled.Safety.MaxConcurrentRemediations != 2 || compiled.Safety.MaxConcurrentReboots != 1 {
		t.Fatalf("compiled concurrency defaults = %d/%d, want 2/1", compiled.Safety.MaxConcurrentRemediations, compiled.Safety.MaxConcurrentReboots)
	}
	if compiled.Safety.Flap.Count != 3 || compiled.Safety.Flap.Window.Std() != 24*time.Hour {
		t.Fatalf("compiled flap defaults = %#v, want count 3/window 24h", compiled.Safety.Flap)
	}
	if compiled.Safety.VerifyQuietWindow.Std() != 10*time.Minute {
		t.Fatalf("compiled quiet window = %v, want 10m", compiled.Safety.VerifyQuietWindow.Std())
	}
	if compiled.Approvals.TTL.Std() != 12*time.Hour {
		t.Fatalf("compiled approval TTL = %v, want 12h", compiled.Approvals.TTL.Std())
	}
	if _, ok := snapshot.Playbooks["gpu-reset.yaml"]; !ok {
		t.Fatal("compiled snapshot is missing gpu-reset.yaml")
	}
	if snapshot.Digest == "" {
		t.Fatal("compiled snapshot must have a digest")
	}
}

func TestCompileSnapshotIncludesDeterministicAcceleratorRuntimeProfiles(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()
	profiles := []kubeneuronv1alpha1.AcceleratorRuntimeProfile{
		testAcceleratorRuntimeProfile(installation.Name, "nvidia-h100", "h100", "1"),
		testAcceleratorRuntimeProfile(installation.Name, "nvidia-a100", "a100", "0"),
	}

	snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil, profiles)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	var compiled config.Config
	if err := yaml.Unmarshal(snapshot.PoliciesYAML, &compiled); err != nil {
		t.Fatalf("unmarshal compiled policies: %v", err)
	}
	if len(compiled.AcceleratorProfiles) != 2 {
		t.Fatalf("compiled accelerator profiles = %#v, want two", compiled.AcceleratorProfiles)
	}
	if compiled.AcceleratorProfiles[0].Name != "nvidia-a100" || compiled.AcceleratorProfiles[1].Name != "nvidia-h100" {
		t.Fatalf("compiled profile order = %#v, want name order", compiled.AcceleratorProfiles)
	}
	a100 := compiled.AcceleratorProfiles[0]
	if a100.NodeSelector["pool"] != "a100" || a100.MaxReportAge.Std() != 10*time.Minute ||
		a100.DriverVersion != "570.86.15" || a100.ProfileUID != "uid-nvidia-a100" || a100.ProfileGeneration != 1 ||
		!a100.Allows("reset-device", "physical-device") ||
		!a100.RequiresVerifiedUnpartitionedTopology("reset-device", "physical-device") {
		t.Fatalf("compiled accelerator profile = %#v, want complete server-owned gate", a100)
	}

	// Object list order must not cause a rollout. The profile data is part of
	// policies.yaml, so a real desired-contract change does change the digest.
	reversed := []kubeneuronv1alpha1.AcceleratorRuntimeProfile{profiles[1], profiles[0]}
	reordered, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil, reversed)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Digest != snapshot.Digest {
		t.Fatalf("profile list order changed snapshot digest: %q != %q", reordered.Digest, snapshot.Digest)
	}
	profiles[0].Spec.ProfileDigest = "sha256:" + strings.Repeat("f", 64)
	changed, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == snapshot.Digest {
		t.Fatal("accelerator profile change must change snapshot digest")
	}
	profiles[0].Spec.ProfileDigest = "sha256:" + strings.Repeat("1", 64)
	profiles[0].Generation = 2
	generationChanged, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if generationChanged.Digest == snapshot.Digest {
		t.Fatal("accelerator profile generation change must change snapshot digest")
	}
}

func TestCompileSnapshotRejectsOverlappingAcceleratorRuntimeProfiles(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()
	broad := testAcceleratorRuntimeProfile(installation.Name, "nvidia-all", "ignored", "1")
	broad.Spec.NodeSelector.MatchLabels = map[string]string{"accelerator": "nvidia"}
	specific := testAcceleratorRuntimeProfile(installation.Name, "nvidia-a100", "a100", "2")

	_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil,
		[]kubeneuronv1alpha1.AcceleratorRuntimeProfile{specific, broad})
	if err == nil || !strings.Contains(err.Error(), "overlapping node selectors") {
		t.Fatalf("overlapping accelerator profiles error = %v, want fail-closed overlap rejection", err)
	}
}

func testAcceleratorRuntimeProfile(installationName, name, pool, digestNibble string) kubeneuronv1alpha1.AcceleratorRuntimeProfile {
	return kubeneuronv1alpha1.AcceleratorRuntimeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: k8stypes.UID("uid-" + name), Generation: 1},
		Spec: kubeneuronv1alpha1.AcceleratorRuntimeProfileSpec{
			KubeNeuronRef: installationName,
			NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"accelerator": "nvidia",
				"pool":        pool,
			}},
			Vendor:         kubeneuronv1alpha1.AcceleratorRuntimeVendorNVIDIA,
			ProfileDigest:  "sha256:" + strings.Repeat(digestNibble, 64),
			DriverVersion:  "570.86.15",
			RuntimeVersion: "dcgm-4.1",
			MaxReportAge:   metav1.Duration{Duration: 10 * time.Minute},
			AllowedActions: []kubeneuronv1alpha1.AcceleratorRuntimeActionPolicy{{
				Action: kubeneuronv1alpha1.AcceleratorRuntimeActionResetDevice,
				Scopes: []kubeneuronv1alpha1.AcceleratorRuntimeScope{
					kubeneuronv1alpha1.AcceleratorRuntimeScopePhysicalDevice,
				},
				RequireVerifiedUnpartitionedTopology: true,
			}},
		},
	}
}

func TestCompileSnapshotRejectsGPUPlaybookWithNodeActionWithoutEffect(t *testing.T) {
	installation := testKubeNeuron()
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "unsafe-reset"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: "fleet",
			Target:        kubeneuronv1alpha1.PlaybookTargetGPU,
			Steps: []kubeneuronv1alpha1.PlaybookStep{
				{Name: "drain", Action: kubeneuronv1alpha1.ActionDrain},
			},
		},
	}}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "unsafe"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: "fleet",
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "ecc-dbe"},
			PlaybookRef:   "unsafe-reset",
		},
	}}

	_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "nodeScheduling") {
		t.Fatalf("expected nodeScheduling validation error, got %v", err)
	}
}

func testPolicyFixture() ([]kubeneuronv1alpha1.GPURemediationPolicy, []kubeneuronv1alpha1.GPUPlaybook) {
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-reset"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: "fleet",
			Target:        kubeneuronv1alpha1.PlaybookTargetGPU,
			Cooldown:      "30m",
			Steps: []kubeneuronv1alpha1.PlaybookStep{
				{Name: "reset", Action: kubeneuronv1alpha1.ActionGPUReset, Timeout: "5m"},
			},
		},
	}}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "gsp"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: "fleet",
			Priority:      10,
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "gsp-error"},
			PlaybookRef:   "gpu-reset",
		},
	}}
	return policies, playbooks
}

func TestCompileSnapshotRejectsEnabledWithoutDeclaredNodes(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModeEnabled
	policies, playbooks := testPolicyFixture()

	// A notification channel does not authorise destructive execution.
	// Enabled stays fail-closed until the nodes permitted to run real
	// resets and reboots are declared explicitly.
	installation.Spec.Notifications = &kubeneuronv1alpha1.NotificationsSpec{
		Slack: &kubeneuronv1alpha1.SecretReference{Name: "slack-webhook"},
	}
	_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "requires spec.safety.destructiveExecution") {
		t.Fatalf("expected Enabled to fail closed, got %v", err)
	}
}

func TestCompileSnapshotEnabledRejectsBadSlackRef(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Notifications = &kubeneuronv1alpha1.NotificationsSpec{
		Slack:        &kubeneuronv1alpha1.SecretReference{Name: "slack", Namespace: "elsewhere"},
		WebhookToken: &kubeneuronv1alpha1.SecretReference{Name: "fleet-webhook-token"},
	}
	_, err := CompileSnapshot(installation, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "namespace must be omitted") {
		t.Fatalf("expected cross-namespace slack ref rejection, got %v", err)
	}
}

func TestCompileSnapshotPausedCompilesDryRun(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModePaused
	policies, playbooks := testPolicyFixture()
	snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err != nil {
		t.Fatalf("paused mode must compile: %v", err)
	}
	// Paused keeps compiled dry_run on: the pause gate is the enforcement,
	// dry-run is the belt-and-suspenders default until explicitly Enabled.
	if !strings.Contains(string(snapshot.PoliciesYAML), "dry_run: true") {
		t.Fatalf("paused mode must keep dry_run: true; got:\n%s", snapshot.PoliciesYAML)
	}
}

func TestCompileSnapshotRejectsMissingWebhookTokenAndPausedAPIAuthentication(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.Notifications.WebhookToken = nil
	if _, err := CompileSnapshot(installation, nil, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "webhookToken is required") {
		t.Fatalf("missing webhook token error = %v", err)
	}

	installation = testKubeNeuron()
	installation.Spec.Safety.ExecutionMode = kubeneuronv1alpha1.ExecutionModePaused
	installation.Spec.Notifications.OperatorAPIToken = nil
	if _, err := CompileSnapshot(installation, nil, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "operatorAPIToken is required") {
		t.Fatalf("missing paused API token error = %v", err)
	}
}

func TestCompileSnapshotRequiresApprovalForReboot(t *testing.T) {
	installation := testKubeNeuron()
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "reboot"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: installation.Name,
			Target:        kubeneuronv1alpha1.PlaybookTargetNode,
			Steps: []kubeneuronv1alpha1.PlaybookStep{{
				Name:   "reboot",
				Action: kubeneuronv1alpha1.ActionReboot,
			}},
		},
	}}
	_, err := CompileSnapshot(installation, nil, playbooks, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Reboot requires approval Required") {
		t.Fatalf("expected reboot approval validation error, got %v", err)
	}
}

func TestCompileSnapshotPostgresWorkflowStore(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.WorkflowStore = kubeneuronv1alpha1.WorkflowStoreSpec{
		Type: "Postgres",
		SecretRef: &kubeneuronv1alpha1.SecretReference{
			Name: "workflow-store",
		},
	}
	book, policy := reconcileConfiguration(installation)
	if _, err := CompileSnapshot(installation,
		[]kubeneuronv1alpha1.GPURemediationPolicy{*policy},
		[]kubeneuronv1alpha1.GPUPlaybook{*book}, nil, nil, nil); err != nil {
		t.Fatalf("Postgres store with a DSN secret must compile, got %v", err)
	}

	// Fail-closed variants.
	for name, mutate := range map[string]func(*kubeneuronv1alpha1.KubeNeuron){
		"missing secretRef": func(k *kubeneuronv1alpha1.KubeNeuron) {
			k.Spec.WorkflowStore.SecretRef = nil
		},
		"cross-namespace secretRef": func(k *kubeneuronv1alpha1.KubeNeuron) {
			k.Spec.WorkflowStore.SecretRef.Namespace = "other"
		},
		"sqlite settings on postgres": func(k *kubeneuronv1alpha1.KubeNeuron) {
			k.Spec.WorkflowStore.SQLite = &kubeneuronv1alpha1.SQLiteStoreSpec{}
		},
	} {
		broken := testKubeNeuron()
		broken.Spec.WorkflowStore = kubeneuronv1alpha1.WorkflowStoreSpec{
			Type:      "Postgres",
			SecretRef: &kubeneuronv1alpha1.SecretReference{Name: "workflow-store"},
		}
		mutate(broken)
		brokenBook, brokenPolicy := reconcileConfiguration(broken)
		if _, err := CompileSnapshot(broken,
			[]kubeneuronv1alpha1.GPURemediationPolicy{*brokenPolicy},
			[]kubeneuronv1alpha1.GPUPlaybook{*brokenBook}, nil, nil, nil); err == nil {
			t.Fatalf("%s: expected compile rejection", name)
		}
	}
}

func TestValidateInstallationRequiresStableSQLiteSettings(t *testing.T) {
	installation := testKubeNeuron()
	installation.Spec.WorkflowStore.SQLite = nil
	if err := validateInstallation(installation); err == nil || !strings.Contains(err.Error(), "sqlite is required") {
		t.Fatalf("validateInstallation() error = %v, want required sqlite settings", err)
	}
}

func TestValidateInstallationDependencyContract(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*kubeneuronv1alpha1.KubeNeuron)
		wantErr string
	}{
		{
			name: "omitted archive",
		},
		{
			name: "disabled archive",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Archive = &kubeneuronv1alpha1.ArchiveSpec{
					ClickHouse: &kubeneuronv1alpha1.DependencySpec{Mode: kubeneuronv1alpha1.IntegrationModeDisabled},
				}
			},
		},
		{
			name: "disabled archive with endpoint",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Archive = &kubeneuronv1alpha1.ArchiveSpec{
					ClickHouse: &kubeneuronv1alpha1.DependencySpec{
						Mode:     kubeneuronv1alpha1.IntegrationModeDisabled,
						Endpoint: "http://ignored.example",
					},
				}
			},
			wantErr: "disabled clickHouse",
		},
		{
			name: "external archive",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Archive = &kubeneuronv1alpha1.ArchiveSpec{
					ClickHouse: &kubeneuronv1alpha1.DependencySpec{
						Mode:     kubeneuronv1alpha1.IntegrationModeExternal,
						Endpoint: "http://clickhouse.example",
					},
				}
			},
			wantErr: "archive ingestion is not implemented",
		},
		{
			name: "managed victoria metrics",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Observability.VictoriaMetrics = kubeneuronv1alpha1.DependencySpec{
					Mode: kubeneuronv1alpha1.IntegrationModeManaged,
				}
			},
			wantErr: "mode Managed is not supported",
		},
		{
			name: "unknown alertmanager mode",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Observability.Alertmanager.Mode = kubeneuronv1alpha1.IntegrationMode("Surprise")
			},
			wantErr: "unsupported mode",
		},
		{
			name: "whitespace endpoint",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Observability.VictoriaMetrics.Endpoint = " \t "
			},
			wantErr: "requires a non-empty endpoint",
		},
		{
			name: "ignored credentials",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.Observability.Alertmanager.SecretRef = &kubeneuronv1alpha1.SecretReference{Name: "alertmanager"}
			},
			wantErr: "secretRef is not supported",
		},
		{
			name: "incomplete TLS references",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.TLS.ClientSecretRef = nil
			},
			wantErr: "spec.tls.clientSecretRef is required",
		},
		{
			name: "cross namespace TLS reference",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.TLS.ServerCASecretRef.Namespace = "other"
			},
			wantErr: "namespace must be omitted",
		},
		{
			name: "key selected from key-pair Secret",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.TLS.ServerSecretRef.Key = "tls.crt"
			},
			wantErr: "key is not supported for a TLS key-pair Secret",
		},
		{
			name: "custom CA keys",
			mutate: func(installation *kubeneuronv1alpha1.KubeNeuron) {
				installation.Spec.TLS.ClientCASecretRef.Key = "clients.pem"
				installation.Spec.TLS.ServerCASecretRef.Key = "servers.pem"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installation := testKubeNeuron()
			if tt.mutate != nil {
				tt.mutate(installation)
			}
			err := validateInstallation(installation)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateInstallation() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateInstallation() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCompileSnapshotIgnoresOtherInstallationsResources(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()
	// Resources selecting another installation are ignored, not rejected —
	// even shapes this installation would fail closed on.
	_, err := CompileSnapshot(installation, policies, playbooks,
		[]kubeneuronv1alpha1.GPUSignalMapping{{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign"},
			Spec:       kubeneuronv1alpha1.GPUSignalMappingSpec{KubeNeuronRef: "another-installation", Source: "dmesg"},
		}},
		[]kubeneuronv1alpha1.GPUMaintenanceWindow{{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign"},
			Spec:       kubeneuronv1alpha1.GPUMaintenanceWindowSpec{KubeNeuronRef: "another-installation"},
		}},
		[]kubeneuronv1alpha1.GPUNodeConfig{{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign"},
			Spec: kubeneuronv1alpha1.GPUNodeConfigSpec{
				KubeNeuronRef: "another-installation", NodeName: "n1",
				SSHSecretRef: &kubeneuronv1alpha1.SecretReference{Name: "ssh"},
			},
		}})
	if err != nil {
		t.Fatalf("resources for another installation must be ignored, got %v", err)
	}
}

func TestCompileMaintenanceWindows(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()
	starts := metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	ends := metav1.NewTime(time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC))

	windows := []kubeneuronv1alpha1.GPUMaintenanceWindow{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "rack-42"},
			Spec: kubeneuronv1alpha1.GPUMaintenanceWindowSpec{
				KubeNeuronRef:   "fleet",
				NodeSelector:    metav1.LabelSelector{MatchLabels: map[string]string{"rack": "42"}},
				StartsAt:        &starts,
				EndsAt:          &ends,
				PauseAutomation: true,
			},
		},
		{
			// Selects a different installation: ignored, not rejected.
			ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Spec:       kubeneuronv1alpha1.GPUMaintenanceWindowSpec{KubeNeuronRef: "someone-else"},
		},
	}
	snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, windows, nil)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	compiled, err := config.LoadWindowsFromBytes(snapshot.WindowsYAML)
	if err != nil {
		t.Fatalf("compiled windows must load: %v", err)
	}
	if len(compiled) != 1 || compiled[0].Name != "rack-42" || compiled[0].MatchLabels["rack"] != "42" {
		t.Fatalf("compiled windows = %+v", compiled)
	}

	// The digest must change when a window changes.
	windows[0].Spec.NodeSelector.MatchLabels["rack"] = "43"
	snapshot2, err := CompileSnapshot(installation, policies, playbooks, nil, windows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot2.Digest == snapshot.Digest {
		t.Fatal("digest must include maintenance windows")
	}
}

func TestCompileMaintenanceWindowsFailClosed(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()
	starts := metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	ends := metav1.NewTime(time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC))

	for name, mutate := range map[string]func(*kubeneuronv1alpha1.GPUMaintenanceWindow){
		"missing times": func(w *kubeneuronv1alpha1.GPUMaintenanceWindow) { w.Spec.StartsAt = nil },
		"pause false":   func(w *kubeneuronv1alpha1.GPUMaintenanceWindow) { w.Spec.PauseAutomation = false },
		"match expressions": func(w *kubeneuronv1alpha1.GPUMaintenanceWindow) {
			w.Spec.NodeSelector.MatchExpressions = []metav1.LabelSelectorRequirement{
				{Key: "rack", Operator: metav1.LabelSelectorOpExists},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			window := kubeneuronv1alpha1.GPUMaintenanceWindow{
				ObjectMeta: metav1.ObjectMeta{Name: "w"},
				Spec: kubeneuronv1alpha1.GPUMaintenanceWindowSpec{
					KubeNeuronRef:   "fleet",
					StartsAt:        &starts,
					EndsAt:          &ends,
					PauseAutomation: true,
				},
			}
			mutate(&window)
			_, err := CompileSnapshot(installation, policies, playbooks, nil,
				[]kubeneuronv1alpha1.GPUMaintenanceWindow{window}, nil)
			if err == nil {
				t.Fatal("unsupported window shape must fail closed")
			}
		})
	}
}

func TestCompileSignalMappingsAndNodeConfigs(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()

	mappings := []kubeneuronv1alpha1.GPUSignalMapping{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "xid-45"},
			Spec: kubeneuronv1alpha1.GPUSignalMappingSpec{
				KubeNeuronRef: "fleet", Source: "xid", XIDCodes: []int32{45},
				Class: "xid-app", Severity: "info",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "vendor-alert"},
			Spec: kubeneuronv1alpha1.GPUSignalMappingSpec{
				KubeNeuronRef: "fleet", Source: "alertmanager", AlertName: "VendorGpuFault",
				Class: "driver-hang", Severity: "critical",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "quiet-dbe"},
			Spec: kubeneuronv1alpha1.GPUSignalMappingSpec{
				KubeNeuronRef: "fleet", Source: "fault",
				Faults: []kubeneuronv1alpha1.FaultCodeSelector{{Vendor: "nvidia", Code: "ecc-dbe"}},
				Class:  "ecc-contained", Severity: "warning",
			},
		},
	}
	nodeConfigs := []kubeneuronv1alpha1.GPUNodeConfig{{
		ObjectMeta: metav1.ObjectMeta{Name: "n1-paused"},
		Spec:       kubeneuronv1alpha1.GPUNodeConfigSpec{KubeNeuronRef: "fleet", NodeName: "n1", Paused: true},
	}}

	snapshot, err := CompileSnapshot(installation, policies, playbooks, mappings, nil, nodeConfigs)
	if err != nil {
		t.Fatalf("CompileSnapshot() error = %v", err)
	}
	overrides, err := config.LoadSignalOverridesFromBytes(snapshot.MappingsYAML)
	if err != nil || len(overrides) != 3 {
		t.Fatalf("compiled overrides = %+v, %v", overrides, err)
	}
	var faultOverride *config.SignalOverride
	for i := range overrides {
		if overrides[i].Name == "quiet-dbe" {
			faultOverride = &overrides[i]
		}
	}
	if faultOverride == nil || len(faultOverride.Faults) != 1 ||
		faultOverride.Faults[0] != (config.FaultOverride{Vendor: "nvidia", Code: "ecc-dbe"}) {
		t.Fatalf("fault mapping survived compile as %+v, want nvidia/ecc-dbe", faultOverride)
	}
	nodes, err := config.LoadNodeConfigsFromBytes(snapshot.NodeConfigsYAML)
	if err != nil || len(nodes) != 1 || !nodes[0].Paused || nodes[0].NodeName != "n1" {
		t.Fatalf("compiled node configs = %+v, %v", nodes, err)
	}

	// Digest covers both inputs.
	nodeConfigs[0].Spec.Paused = false
	snapshot2, err := CompileSnapshot(installation, policies, playbooks, mappings, nil, nodeConfigs)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot2.Digest == snapshot.Digest {
		t.Fatal("digest must include node configs")
	}
}

func TestCompileSignalMappingsFailClosed(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()

	for name, spec := range map[string]kubeneuronv1alpha1.GPUSignalMappingSpec{
		"labels unsupported": {
			KubeNeuronRef: "fleet", Source: "xid", XIDCodes: []int32{45},
			Labels: map[string]string{"gpu": "0"}, Class: "x", Severity: "info",
		},
		"bad source": {
			KubeNeuronRef: "fleet", Source: "dmesg", XIDCodes: []int32{45},
			Class: "x", Severity: "info",
		},
		"source mismatch": {
			KubeNeuronRef: "fleet", Source: "xid", AlertName: "A",
			Class: "x", Severity: "info",
		},
		"fault source without faults": {
			KubeNeuronRef: "fleet", Source: "fault", XIDCodes: []int32{45},
			Class: "x", Severity: "info",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileSnapshot(installation, policies, playbooks,
				[]kubeneuronv1alpha1.GPUSignalMapping{{
					ObjectMeta: metav1.ObjectMeta{Name: "m"}, Spec: spec,
				}}, nil, nil)
			if err == nil {
				t.Fatal("unsupported mapping shape must fail closed")
			}
		})
	}

	// Overriding the same XID twice fails at compile time.
	duplicate := kubeneuronv1alpha1.GPUSignalMappingSpec{
		KubeNeuronRef: "fleet", Source: "xid", XIDCodes: []int32{45}, Class: "x", Severity: "info",
	}
	_, err := CompileSnapshot(installation, policies, playbooks,
		[]kubeneuronv1alpha1.GPUSignalMapping{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: duplicate},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: duplicate},
		}, nil, nil)
	if err == nil {
		t.Fatal("duplicate XID overrides must fail closed")
	}
}

func TestCompileNodeConfigsFailClosed(t *testing.T) {
	installation := testKubeNeuron()
	policies, playbooks := testPolicyFixture()

	// Credential references are not consumed by any actuator yet.
	_, err := CompileSnapshot(installation, policies, playbooks, nil, nil,
		[]kubeneuronv1alpha1.GPUNodeConfig{{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Spec: kubeneuronv1alpha1.GPUNodeConfigSpec{
				KubeNeuronRef: "fleet", NodeName: "n1",
				SSHSecretRef: &kubeneuronv1alpha1.SecretReference{Name: "ssh"},
			},
		}})
	if err == nil {
		t.Fatal("ssh secret ref must fail closed")
	}

	// Two configs for one node conflict.
	_, err = CompileSnapshot(installation, policies, playbooks, nil, nil,
		[]kubeneuronv1alpha1.GPUNodeConfig{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: kubeneuronv1alpha1.GPUNodeConfigSpec{KubeNeuronRef: "fleet", NodeName: "n1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: kubeneuronv1alpha1.GPUNodeConfigSpec{KubeNeuronRef: "fleet", NodeName: "n1", Paused: true}},
		})
	if err == nil {
		t.Fatal("duplicate node configs must fail closed")
	}
}

// RecycleNode and ReplaceNode restart or destroy the whole VM, so the compiler
// must force approval on them exactly as it does for Reboot.
func TestCompileSnapshotRequiresApprovalForCloudActions(t *testing.T) {
	for _, action := range []kubeneuronv1alpha1.PlaybookAction{
		kubeneuronv1alpha1.ActionRecycleNode,
		kubeneuronv1alpha1.ActionReplaceNode,
	} {
		installation := testKubeNeuron()
		playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
			ObjectMeta: metav1.ObjectMeta{Name: "cloud"},
			Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
				KubeNeuronRef: installation.Name,
				Target:        kubeneuronv1alpha1.PlaybookTargetNode,
				Steps:         []kubeneuronv1alpha1.PlaybookStep{{Name: "act", Action: action}},
			},
		}}
		_, err := CompileSnapshot(installation, nil, playbooks, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "requires approval Required") {
			t.Fatalf("%s without approval: got %v, want an approval error", action, err)
		}
	}
}

// With approval set, a RecycleNode step compiles to the platform action the
// controller executes.
func TestCompileSnapshotRecycleNodeCompiles(t *testing.T) {
	installation := testKubeNeuron()
	// A cloud action needs a capable provider to compile: with none configured
	// the playbook is now rejected (see TestCloudCapabilityGateRejectsWithoutCloud).
	installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: fauxFullCloud}
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "recycle"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: installation.Name,
			Target:        kubeneuronv1alpha1.PlaybookTargetNode,
			Steps: []kubeneuronv1alpha1.PlaybookStep{{
				Name:     "recycle",
				Action:   kubeneuronv1alpha1.ActionRecycleNode,
				Approval: kubeneuronv1alpha1.ApprovalRequired,
			}},
		},
	}}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: installation.Name,
			Priority:      1,
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "fell-off-bus"},
			PlaybookRef:   "recycle",
		},
	}}
	snapshot, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(snapshot.Playbooks["recycle.yaml"]); !strings.Contains(got, "platform.recycle_node") {
		t.Fatalf("compiled playbook = %q, want platform.recycle_node", got)
	}
}

// The capability gate is proven with in-package test-double providers, never
// AWS: the seam is provider-agnostic, so a provider is just a name plus a
// declared capability set. These register once under unique names.
//
//	fauxFullCloud   — declares both primitives (a fully-capable provider)
//	fauxReplaceOnly — can Replace but not ReinitializeInPlace
//	fauxNoPrimitive — declares nothing (a provider with no safe primitive)
const (
	fauxFullCloud    = "faux-full"
	fauxReplaceOnly  = "faux-replace-only"
	fauxNoPrimitive  = "faux-none"
	fauxUnregistered = "faux-unregistered"
)

func init() {
	// The factory is never called by the capability gate (it queries the static
	// declaration), but Register requires a non-nil one.
	unusedFactory := func(context.Context, cloud.Config) (cloud.Provider, error) {
		return nil, nil
	}
	cloud.Register(fauxFullCloud, cloud.Capabilities{ReinitializeInPlace: true, Replace: true}, unusedFactory)
	cloud.Register(fauxReplaceOnly, cloud.Capabilities{Replace: true}, unusedFactory)
	cloud.Register(fauxNoPrimitive, cloud.Capabilities{}, unusedFactory)
}

// cloudPlaybookInstallation builds a valid installation pinned to a cloud
// provider, plus a policy and a single-step node playbook using the given
// action, so CompileSnapshot exercises the capability gate.
func cloudPlaybookInstallation(provider string, act kubeneuronv1alpha1.PlaybookAction) (*kubeneuronv1alpha1.KubeNeuron, []kubeneuronv1alpha1.GPURemediationPolicy, []kubeneuronv1alpha1.GPUPlaybook) {
	installation := testKubeNeuron()
	installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: provider}
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: installation.Name,
			Target:        kubeneuronv1alpha1.PlaybookTargetNode,
			Steps: []kubeneuronv1alpha1.PlaybookStep{{
				Name:     "act",
				Action:   act,
				Approval: kubeneuronv1alpha1.ApprovalRequired,
			}},
		},
	}}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: installation.Name,
			Priority:      1,
			Match:         kubeneuronv1alpha1.SignalMatch{Class: "fell-off-bus"},
			PlaybookRef:   "cloud",
		},
	}}
	return installation, policies, playbooks
}

// TestCloudCapabilityGateFailsClosed proves the operator rejects a playbook
// whose cloud action the configured provider cannot perform, and admits one it
// can — the whole point of the provider-neutral capability declaration.
func TestCloudCapabilityGateFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		action   kubeneuronv1alpha1.PlaybookAction
		wantErr  string // substring; empty means compile must succeed
	}{
		{
			name:     "fully capable provider admits recycle",
			provider: fauxFullCloud,
			action:   kubeneuronv1alpha1.ActionRecycleNode,
		},
		{
			name:     "fully capable provider admits replace",
			provider: fauxFullCloud,
			action:   kubeneuronv1alpha1.ActionReplaceNode,
		},
		{
			name:     "replace-only provider rejects recycle",
			provider: fauxReplaceOnly,
			action:   kubeneuronv1alpha1.ActionRecycleNode,
			wantErr:  "reinitialize-in-place",
		},
		{
			name:     "replace-only provider admits replace",
			provider: fauxReplaceOnly,
			action:   kubeneuronv1alpha1.ActionReplaceNode,
		},
		{
			name:     "no-primitive provider rejects replace",
			provider: fauxNoPrimitive,
			action:   kubeneuronv1alpha1.ActionReplaceNode,
			wantErr:  "replace",
		},
		{
			name:     "unregistered provider fails closed",
			provider: fauxUnregistered,
			action:   kubeneuronv1alpha1.ActionRecycleNode,
			wantErr:  "not a known provider",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installation, policies, playbooks := cloudPlaybookInstallation(tc.provider, tc.action)
			_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("provider %q + %s: got error %v, want a clean compile", tc.provider, tc.action, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("provider %q + %s: got %v, want an error containing %q", tc.provider, tc.action, err, tc.wantErr)
			}
		})
	}
}

// --- Fix 6: an escalation cycle is rejected at compile time ---

// Two playbooks that escalate to each other (A->B->A) pass a per-playbook
// existence check but, under a persistent fault, would let the ladder repeat its
// destructive rungs forever. The compiler must reject the cycle.
func TestCompileSnapshotRejectsEscalationCycle(t *testing.T) {
	installation := testKubeNeuron()
	step := []kubeneuronv1alpha1.PlaybookStep{{Name: "reset", Action: kubeneuronv1alpha1.ActionGPUReset}}
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
				KubeNeuronRef: installation.Name, Target: kubeneuronv1alpha1.PlaybookTargetGPU,
				Steps: step, OnFailure: &kubeneuronv1alpha1.PlaybookFailure{PlaybookRef: "b"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "b"},
			Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
				KubeNeuronRef: installation.Name, Target: kubeneuronv1alpha1.PlaybookTargetGPU,
				Steps: step, OnFailure: &kubeneuronv1alpha1.PlaybookFailure{PlaybookRef: "a"},
			},
		},
	}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: installation.Name, Priority: 1,
			Match: kubeneuronv1alpha1.SignalMatch{Class: "fell-off-bus"}, PlaybookRef: "a",
		},
	}}
	_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("A->B->A escalation must fail compile with a cycle error, got %v", err)
	}
}

// --- Fix 14: negative (and zero) durations are rejected at compile ---

// A negative flapWindow silently disables flap quarantine rather than tightening
// it; a negative/zero window or TTL diverges from the declared intent. These
// must fail compile, not compile into a snapshot that behaves nothing like the
// configuration.
func TestCompileSnapshotRejectsNonPositiveDurations(t *testing.T) {
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-reset"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: "fleet", Target: kubeneuronv1alpha1.PlaybookTargetGPU,
			Steps: []kubeneuronv1alpha1.PlaybookStep{{Name: "reset", Action: kubeneuronv1alpha1.ActionGPUReset}},
		},
	}}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: "fleet", Priority: 1,
			Match: kubeneuronv1alpha1.SignalMatch{Class: "fell-off-bus"}, PlaybookRef: "gpu-reset",
		},
	}}

	for _, tc := range []struct {
		name string
		set  func(*kubeneuronv1alpha1.KubeNeuron)
		want string
	}{
		{"negative flap window", func(k *kubeneuronv1alpha1.KubeNeuron) { k.Spec.Safety.FlapWindow = "-24h" }, "flapWindow"},
		{"zero verify quiet window", func(k *kubeneuronv1alpha1.KubeNeuron) { k.Spec.Safety.VerifyQuietWindow = "0s" }, "verifyQuietWindow"},
		{"negative approval ttl", func(k *kubeneuronv1alpha1.KubeNeuron) { k.Spec.Approvals.TTL = "-1h" }, "ttl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installation := testKubeNeuron()
			tc.set(installation)
			_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// A negative cooldown on a playbook is likewise rejected.
func TestCompileSnapshotRejectsNegativePlaybookCooldown(t *testing.T) {
	installation := testKubeNeuron()
	playbooks := []kubeneuronv1alpha1.GPUPlaybook{{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-reset"},
		Spec: kubeneuronv1alpha1.GPUPlaybookSpec{
			KubeNeuronRef: installation.Name, Target: kubeneuronv1alpha1.PlaybookTargetGPU,
			Cooldown: "-1h",
			Steps:    []kubeneuronv1alpha1.PlaybookStep{{Name: "reset", Action: kubeneuronv1alpha1.ActionGPUReset}},
		},
	}}
	policies := []kubeneuronv1alpha1.GPURemediationPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: kubeneuronv1alpha1.GPURemediationPolicySpec{
			KubeNeuronRef: installation.Name, Priority: 1,
			Match: kubeneuronv1alpha1.SignalMatch{Class: "fell-off-bus"}, PlaybookRef: "gpu-reset",
		},
	}}
	_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative cooldown must fail compile, got %v", err)
	}
}

// TestCloudCapabilityGateRejectsWithoutCloud confirms a cloud-action playbook is
// rejected at compile time when NO provider is configured: nothing can ever
// perform a RecycleNode/ReplaceNode with no spec.cloud, so the gap must be
// caught here rather than surfacing mid-incident at the runtime fail-closed
// check (after cordon+drain+approval).
func TestCloudCapabilityGateRejectsWithoutCloud(t *testing.T) {
	for _, act := range []kubeneuronv1alpha1.PlaybookAction{
		kubeneuronv1alpha1.ActionRecycleNode,
		kubeneuronv1alpha1.ActionReplaceNode,
	} {
		_, policies, playbooks := cloudPlaybookInstallation(fauxFullCloud, act)
		installation := testKubeNeuron() // no spec.cloud
		_, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "spec.cloud is not configured") {
			t.Fatalf("%s with no cloud configured must fail compile, got %v", act, err)
		}
	}
}
