// Package operator compiles KubeNeuron custom resources into the immutable
// runtime configuration consumed by the current controller during migration.
package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/cloud"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	RuntimeConfigSuffix   = "-runtime"
	PlaybooksConfigSuffix = "-playbooks"
)

// Snapshot is the deterministic, operator-generated controller input. The
// config maps are immutable from the controller's perspective; a new digest
// creates a rollout rather than relying on in-process file reloads.
type Snapshot struct {
	PoliciesYAML []byte
	// WindowsYAML is the compiled maintenance-window set (may be an empty
	// document when no window selects the installation).
	WindowsYAML []byte
	// MappingsYAML is the compiled signal-override set.
	MappingsYAML []byte
	// NodeConfigsYAML is the compiled per-node configuration set.
	NodeConfigsYAML []byte
	Playbooks       map[string][]byte
	Digest          string
}

// CompileSnapshot validates the CRD configuration and turns the subset that
// the current controller understands into its legacy file format. Features
// without an execution implementation are rejected rather than ignored.
func CompileSnapshot(
	installation *kubeneuronv1alpha1.KubeNeuron,
	policies []kubeneuronv1alpha1.GPURemediationPolicy,
	playbooks []kubeneuronv1alpha1.GPUPlaybook,
	mappings []kubeneuronv1alpha1.GPUSignalMapping,
	maintenance []kubeneuronv1alpha1.GPUMaintenanceWindow,
	nodeConfigs []kubeneuronv1alpha1.GPUNodeConfig,
	acceleratorProfileSets ...[]kubeneuronv1alpha1.AcceleratorRuntimeProfile,
) (*Snapshot, error) {
	if err := validateInstallation(installation); err != nil {
		return nil, err
	}
	if len(acceleratorProfileSets) > 1 {
		return nil, fmt.Errorf("at most one accelerator runtime profile set may be compiled")
	}
	var acceleratorProfiles []kubeneuronv1alpha1.AcceleratorRuntimeProfile
	if len(acceleratorProfileSets) == 1 {
		acceleratorProfiles = acceleratorProfileSets[0]
	}

	books, err := compilePlaybookIndex(installation.Name, playbooks)
	if err != nil {
		return nil, err
	}
	if err := validateCloudCapabilities(installation, playbooks); err != nil {
		return nil, err
	}

	compiledPolicies, err := compilePolicies(installation, policies, books)
	if err != nil {
		return nil, err
	}
	compiledAcceleratorProfiles, err := compileAcceleratorRuntimeProfiles(installation.Name, acceleratorProfiles)
	if err != nil {
		return nil, err
	}

	flapWindow, err := configDuration(installation.Spec.Safety.FlapWindow, "24h")
	if err != nil {
		return nil, fmt.Errorf("spec.safety.flapWindow: %w", err)
	}
	quietWindow, err := configDuration(installation.Spec.Safety.VerifyQuietWindow, "10m")
	if err != nil {
		return nil, fmt.Errorf("spec.safety.verifyQuietWindow: %w", err)
	}
	approvalTTL, err := configDuration(installation.Spec.Approvals.TTL, "12h")
	if err != nil {
		return nil, fmt.Errorf("spec.approvals.ttl: %w", err)
	}

	// Dry-run is the default and stays on for every mode except Enabled,
	// which validateRuntimeSupport has already confined to the declared
	// destructive-execution nodes.
	cfg := config.Config{
		Safety: config.Safety{
			DryRun:                    effectiveExecutionMode(installation.Spec.Safety) != kubeneuronv1alpha1.ExecutionModeEnabled,
			MaxConcurrentRemediations: effectivePositive(installation.Spec.Safety.MaxConcurrentRemediations, 2),
			MaxConcurrentReboots:      effectivePositive(installation.Spec.Safety.MaxConcurrentReboots, 1),
			Flap: config.Flap{
				Count:  effectivePositive(installation.Spec.Safety.FlapCount, 3),
				Window: flapWindow,
			},
			VerifyQuietWindow:                quietWindow,
			QuiesceForbidResetWhenPresent:    quiesceForbiddenHolders(installation.Spec.Safety.Quiesce),
			DestructiveExecutionNodeSelector: destructiveNodeSelector(installation.Spec.Safety),
		},
		Approvals: config.Approvals{
			Channels: append([]string(nil), installation.Spec.Approvals.Channels...),
			TTL:      approvalTTL,
		},
		Policies:            compiledPolicies,
		AcceleratorProfiles: compiledAcceleratorProfiles,
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("compiled policies: %w", err)
	}

	policiesYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal policies: %w", err)
	}
	windowsYAML, err := compileMaintenanceWindows(installation.Name, maintenance)
	if err != nil {
		return nil, err
	}
	mappingsYAML, err := compileSignalMappings(installation.Name, mappings)
	if err != nil {
		return nil, err
	}
	nodeConfigsYAML, err := compileNodeConfigs(installation.Name, nodeConfigs)
	if err != nil {
		return nil, err
	}

	output := &Snapshot{
		PoliciesYAML:    policiesYAML,
		WindowsYAML:     windowsYAML,
		MappingsYAML:    mappingsYAML,
		NodeConfigsYAML: nodeConfigsYAML,
		Playbooks:       make(map[string][]byte, len(books)),
	}
	bookNames := make([]string, 0, len(books))
	for name := range books {
		bookNames = append(bookNames, name)
	}
	sort.Strings(bookNames)
	for _, name := range bookNames {
		data, err := yaml.Marshal(books[name])
		if err != nil {
			return nil, fmt.Errorf("marshal playbook %q: %w", name, err)
		}
		output.Playbooks[name+".yaml"] = data
	}

	h := sha256.New()
	_, _ = h.Write(output.PoliciesYAML)
	_, _ = h.Write(output.WindowsYAML)
	_, _ = h.Write(output.MappingsYAML)
	_, _ = h.Write(output.NodeConfigsYAML)
	for _, name := range bookNames {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write(output.Playbooks[name+".yaml"])
	}
	output.Digest = hex.EncodeToString(h.Sum(nil))
	return output, nil
}

// compileMaintenanceWindows validates the windows selecting this
// installation and serializes them for the controller. Only the implemented
// subset is accepted: matchLabels selectors and pauseAutomation=true;
// everything else fails closed.
func compileMaintenanceWindows(installationName string, maintenance []kubeneuronv1alpha1.GPUMaintenanceWindow) ([]byte, error) {
	var windows []config.MaintenanceWindow
	for i := range maintenance {
		w := &maintenance[i]
		if w.Spec.KubeNeuronRef != installationName {
			continue
		}
		if w.Spec.StartsAt == nil || w.Spec.EndsAt == nil {
			return nil, fmt.Errorf("maintenance window %q: startsAt and endsAt are required", w.Name)
		}
		if !w.Spec.StartsAt.Time.Before(w.Spec.EndsAt.Time) {
			return nil, fmt.Errorf("maintenance window %q: startsAt must be before endsAt", w.Name)
		}
		if !w.Spec.PauseAutomation {
			return nil, fmt.Errorf("maintenance window %q: pauseAutomation false has no runtime meaning; omit the window instead", w.Name)
		}
		if len(w.Spec.NodeSelector.MatchExpressions) > 0 {
			return nil, fmt.Errorf("maintenance window %q: nodeSelector.matchExpressions is not supported by the current runtime; use matchLabels", w.Name)
		}
		windows = append(windows, config.MaintenanceWindow{
			Name:        w.Name,
			StartsAt:    w.Spec.StartsAt.UTC(),
			EndsAt:      w.Spec.EndsAt.UTC(),
			MatchLabels: w.Spec.NodeSelector.MatchLabels,
		})
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Name < windows[j].Name })
	data, err := yaml.Marshal(struct {
		Windows []config.MaintenanceWindow `yaml:"windows"`
	}{Windows: windows})
	if err != nil {
		return nil, fmt.Errorf("marshal maintenance windows: %w", err)
	}
	return data, nil
}

// validateCloudCapabilities fails a compile closed when a playbook uses a cloud
// primitive the configured provider does not declare. It is the compile-time
// half of the fail-closed boundary: a provider that cannot reinitialize an
// instance in place must not admit a RecycleNode playbook, rather than the
// controller discovering the gap in the middle of an incident.
//
// The capability axis is provider-side: the required primitive comes from the
// action registry, and whether the provider has it comes from that provider's
// own declaration (internal/cloud), queried by name without any credentials.
// When NO provider is configured a cloud action can never run at all — there is
// nothing that could ever perform it — so such a playbook is rejected here
// rather than shipped to compile-clean and then fail closed mid-incident, after
// a cordon+drain+approval, at the runtime check. An unregistered provider name
// also fails closed here.
func validateCloudCapabilities(installation *kubeneuronv1alpha1.KubeNeuron, playbooks []kubeneuronv1alpha1.GPUPlaybook) error {
	cloudSpec := installation.Spec.Cloud
	providerConfigured := cloudSpec != nil && cloudSpec.Provider != ""
	var caps cloud.Capabilities
	var known bool
	if providerConfigured {
		caps, known = cloud.DeclaredCapabilities(cloudSpec.Provider)
	}
	for i := range playbooks {
		pb := &playbooks[i]
		if pb.Spec.KubeNeuronRef != installation.Name {
			continue
		}
		for _, step := range pb.Spec.Steps {
			def, found := action.ByPlaybookAction(step.Action)
			if !found || def.CloudPrimitive == action.CloudPrimitiveNone {
				continue
			}
			if !providerConfigured {
				return fmt.Errorf("playbook %q step %q: action %s is a cloud operation but spec.cloud is not configured; no provider can ever perform it, so the playbook is rejected", pb.Name, step.Name, step.Action)
			}
			if !known {
				return fmt.Errorf("playbook %q step %q: action %s needs cloud provider %q, which is not a known provider", pb.Name, step.Name, step.Action, cloudSpec.Provider)
			}
			if !cloudCapabilitySatisfied(def.CloudPrimitive, caps) {
				return fmt.Errorf("playbook %q step %q: action %s requires the %q capability, which cloud provider %q does not declare", pb.Name, step.Name, step.Action, def.CloudPrimitive, cloudSpec.Provider)
			}
		}
	}
	return nil
}

// cloudCapabilitySatisfied maps a required cloud primitive to the provider's
// declared capability. An unknown primitive is treated as satisfied: it names
// no gate, so it cannot be the thing that fails a compile.
func cloudCapabilitySatisfied(primitive action.CloudPrimitive, caps cloud.Capabilities) bool {
	switch primitive {
	case action.CloudPrimitiveReinitializeInPlace:
		return caps.ReinitializeInPlace
	case action.CloudPrimitiveReplace:
		return caps.Replace
	default:
		return true
	}
}

func validateInstallation(s *kubeneuronv1alpha1.KubeNeuron) error {
	if s.Name == "" {
		return fmt.Errorf("installation name is required")
	}
	if s.Spec.Namespace == "" {
		return fmt.Errorf("spec.namespace is required")
	}
	if s.Spec.Controller.Image == "" {
		return fmt.Errorf("spec.controller.image is required")
	}
	if s.Spec.Agent.Image == "" {
		return fmt.Errorf("spec.agent.image is required")
	}
	if s.Spec.WorkflowStore.Type == "" {
		return fmt.Errorf("spec.workflowStore.type is required")
	}
	if err := validateRuntimeSupport(s); err != nil {
		return err
	}
	if s.Spec.WorkflowStore.Type != "SQLite" && s.Spec.WorkflowStore.Type != "Postgres" {
		return fmt.Errorf("unsupported workflow store %q", s.Spec.WorkflowStore.Type)
	}
	if s.Spec.WorkflowStore.Type == "SQLite" {
		if s.Spec.WorkflowStore.SQLite == nil {
			return fmt.Errorf("spec.workflowStore.sqlite is required for SQLite")
		}
		if _, err := sqliteStorageQuantity(s.Spec.WorkflowStore); err != nil {
			return err
		}
	}
	return nil
}

// validateRuntimeSupport rejects API values reserved for future runtime
// implementations. Keep this check separate from structural validation so
// resource construction can apply the same fail-closed boundary.
// destructiveExecutionAcknowledgement is the exact sentence an operator
// must type to arm real destructive execution. Deliberately long and
// unambiguous: it cannot be produced by a stray `true`.
const destructiveExecutionAcknowledgement = "I understand these nodes may be reset, rebooted, or destroyed"

// destructiveNodeSelector returns the compiled blast-radius selector the
// controller confines its own destructive platform steps to. It is populated
// only for an Enabled install with a destructiveExecution block —
// validateRuntimeSupport has already proven that block names a non-empty
// selector — so an empty result means "no confinement configured", which is
// safe because every other mode is globally dry-run.
func destructiveNodeSelector(safety kubeneuronv1alpha1.SafetySpec) map[string]string {
	if effectiveExecutionMode(safety) != kubeneuronv1alpha1.ExecutionModeEnabled || safety.DestructiveExecution == nil {
		return nil
	}
	return copyStringMap(safety.DestructiveExecution.NodeSelector)
}

// quiesceForbiddenHolders normalizes the declared process names, dropping blanks
// so a stray empty entry cannot forbid every reset by matching a blank command.
func quiesceForbiddenHolders(quiesce *kubeneuronv1alpha1.QuiesceSpec) []string {
	if quiesce == nil {
		return nil
	}
	var out []string
	for _, name := range quiesce.ForbidResetWhenPresent {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// validateHostTooling rejects host tooling that could not work at runtime.
//
// binDir is the only host directory mounted into the agent, so a dcgmiPath
// outside it names a file that would simply not exist in the container. Saying
// so at compile time beats a node whose attestation silently stays degraded.
func validateHostTooling(tooling *kubeneuronv1alpha1.HostToolingSpec) error {
	if tooling == nil {
		return nil
	}
	dcgmiPath := strings.TrimSpace(tooling.DCGMIPath)
	if dcgmiPath == "" {
		return nil
	}
	binDir := strings.TrimSpace(tooling.BinDir)
	if binDir == "" {
		binDir = "/usr/bin"
	}
	if filepath.Dir(filepath.Clean(dcgmiPath)) != filepath.Clean(binDir) {
		return fmt.Errorf(
			"spec.agent.hostTooling.dcgmiPath %q must be directly inside binDir %q: binDir is the only host directory mounted into the agent",
			dcgmiPath, binDir)
	}
	return nil
}

func validateRuntimeSupport(s *kubeneuronv1alpha1.KubeNeuron) error {
	// Real side effects are confined to explicitly declared nodes: Enabled
	// without spec.safety.destructiveExecution stays rejected, and the block
	// itself must name those nodes. This is the compiler-side half of the
	// boundary; the CRD enforces the same rule at admission, and the
	// operator arms the agent only on nodes matching the selector. Paused
	// remains a dry-run belt-and-suspenders mode.
	if effectiveExecutionMode(s.Spec.Safety) == kubeneuronv1alpha1.ExecutionModeEnabled {
		destructive := s.Spec.Safety.DestructiveExecution
		if destructive == nil {
			return fmt.Errorf("spec.safety.executionMode Enabled requires spec.safety.destructiveExecution: real reset and reboot are confined to explicitly declared nodes")
		}
		if len(destructive.NodeSelector) == 0 {
			return fmt.Errorf("spec.safety.destructiveExecution.nodeSelector must name the permitted nodes; an empty selector would arm the whole fleet")
		}
		if destructive.Acknowledgement != destructiveExecutionAcknowledgement {
			return fmt.Errorf("spec.safety.destructiveExecution.acknowledgement must read exactly %q", destructiveExecutionAcknowledgement)
		}
	}
	if err := validateHostTooling(s.Spec.Agent.HostTooling); err != nil {
		return err
	}
	if webhookTokenRef(s) == nil {
		return fmt.Errorf("spec.notifications.webhookToken is required: Alertmanager ingress must be authenticated")
	}
	if effectiveExecutionMode(s.Spec.Safety) == kubeneuronv1alpha1.ExecutionModePaused && operatorAPITokenRef(s) == nil {
		return fmt.Errorf("spec.notifications.operatorAPIToken is required for Paused mode so the gate can be resumed through the authenticated API")
	}
	if err := validateNotifications(s.Spec.Notifications); err != nil {
		return err
	}
	if s.Spec.WorkflowStore.Type == "Postgres" {
		ref := s.Spec.WorkflowStore.SecretRef
		if ref == nil || strings.TrimSpace(ref.Name) == "" {
			return fmt.Errorf("spec.workflowStore.secretRef is required for Postgres: it selects the DSN Secret")
		}
		if ref.Namespace != "" {
			return fmt.Errorf("spec.workflowStore.secretRef.namespace must be omitted; the DSN Secret lives in spec.namespace")
		}
		if s.Spec.WorkflowStore.SQLite != nil {
			return fmt.Errorf("spec.workflowStore.sqlite is not supported for Postgres")
		}
	}
	if s.Spec.WorkflowStore.Type == "SQLite" {
		if s.Spec.WorkflowStore.SQLite == nil {
			return fmt.Errorf("spec.workflowStore.sqlite is required for SQLite")
		}
		if s.Spec.WorkflowStore.SecretRef != nil {
			return fmt.Errorf("spec.workflowStore.secretRef is not supported for SQLite")
		}
		if s.Spec.WorkflowStore.Database != "" {
			return fmt.Errorf("spec.workflowStore.database is not supported for SQLite")
		}
	}
	if err := validateTLS(s); err != nil {
		return err
	}
	if err := validateDependency("victoriaMetrics", s.Spec.Observability.VictoriaMetrics); err != nil {
		return err
	}
	if err := validateDependency("alertmanager", s.Spec.Observability.Alertmanager); err != nil {
		return err
	}
	if err := validateArchive(s.Spec.Archive); err != nil {
		return err
	}
	return nil
}

func validateTLS(s *kubeneuronv1alpha1.KubeNeuron) error {
	refs := []struct {
		name    string
		ref     *kubeneuronv1alpha1.SecretReference
		keyPair bool
	}{
		{name: "serverSecretRef", ref: s.Spec.TLS.ServerSecretRef, keyPair: true},
		{name: "clientCASecretRef", ref: s.Spec.TLS.ClientCASecretRef},
		{name: "clientSecretRef", ref: s.Spec.TLS.ClientSecretRef, keyPair: true},
		{name: "serverCASecretRef", ref: s.Spec.TLS.ServerCASecretRef},
	}
	if ref := s.Spec.TLS.PublicServerSecretRef; ref != nil {
		refs = append(refs, struct {
			name    string
			ref     *kubeneuronv1alpha1.SecretReference
			keyPair bool
		}{name: "publicServerSecretRef", ref: ref, keyPair: true})
	}
	for _, item := range refs {
		if item.ref == nil {
			return fmt.Errorf("spec.tls.%s is required", item.name)
		}
		if strings.TrimSpace(item.ref.Name) == "" {
			return fmt.Errorf("spec.tls.%s.name is required", item.name)
		}
		if item.ref.Namespace != "" {
			return fmt.Errorf("spec.tls.%s.namespace must be omitted; TLS Secrets use spec.namespace", item.name)
		}
		if item.keyPair && item.ref.Key != "" {
			return fmt.Errorf("spec.tls.%s.key is not supported for a TLS key-pair Secret", item.name)
		}
	}
	return nil
}

func validateNotifications(n *kubeneuronv1alpha1.NotificationsSpec) error {
	if n == nil {
		return nil
	}
	for _, item := range []struct {
		name string
		ref  *kubeneuronv1alpha1.SecretReference
	}{
		{name: "slack", ref: n.Slack},
		{name: "operatorAPIToken", ref: n.OperatorAPIToken},
		{name: "webhookToken", ref: n.WebhookToken},
		{name: "webhook", ref: n.Webhook},
		{name: "pagerduty", ref: n.PagerDuty},
	} {
		if item.ref == nil {
			continue
		}
		if strings.TrimSpace(item.ref.Name) == "" {
			return fmt.Errorf("spec.notifications.%s.name is required", item.name)
		}
		if item.ref.Namespace != "" {
			return fmt.Errorf("spec.notifications.%s.namespace must be omitted; notification Secrets use spec.namespace", item.name)
		}
	}
	return nil
}

// slackRef returns the Slack webhook Secret reference, or nil.
func slackRef(s *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.SecretReference {
	if s.Spec.Notifications == nil {
		return nil
	}
	return s.Spec.Notifications.Slack
}

func operatorAPITokenRef(s *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.SecretReference {
	if s.Spec.Notifications == nil {
		return nil
	}
	return s.Spec.Notifications.OperatorAPIToken
}

func webhookTokenRef(s *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.SecretReference {
	if s.Spec.Notifications == nil {
		return nil
	}
	return s.Spec.Notifications.WebhookToken
}

func authUsersRef(s *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.SecretReference {
	if s.Spec.Auth == nil {
		return nil
	}
	return s.Spec.Auth.Users
}

func authOIDC(s *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.OIDCSpec {
	if s.Spec.Auth == nil {
		return nil
	}
	return s.Spec.Auth.OIDC
}

// oidcClientSecretKey is the Secret key holding the OIDC client secret.
func oidcClientSecretKey(ref kubeneuronv1alpha1.SecretReference) string {
	if ref.Key != "" {
		return ref.Key
	}
	return "client-secret"
}

func notifyWebhookRef(s *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.SecretReference {
	if s.Spec.Notifications == nil {
		return nil
	}
	return s.Spec.Notifications.Webhook
}

func pagerdutyRef(s *kubeneuronv1alpha1.KubeNeuron) *kubeneuronv1alpha1.SecretReference {
	if s.Spec.Notifications == nil {
		return nil
	}
	return s.Spec.Notifications.PagerDuty
}

// pagerdutySecretKey is the Secret key holding the Events v2 routing key.
func pagerdutySecretKey(ref *kubeneuronv1alpha1.SecretReference) string {
	if ref != nil && ref.Key != "" {
		return ref.Key
	}
	return "routing-key"
}

// slackSecretKey is the Secret key holding the webhook URL.
func slackSecretKey(ref *kubeneuronv1alpha1.SecretReference) string {
	if ref != nil && ref.Key != "" {
		return ref.Key
	}
	return "webhook-url"
}

func tokenSecretKey(ref *kubeneuronv1alpha1.SecretReference) string {
	if ref != nil && ref.Key != "" {
		return ref.Key
	}
	return "token"
}

func caSecretKey(ref *kubeneuronv1alpha1.SecretReference) string {
	if ref != nil && ref.Key != "" {
		return ref.Key
	}
	return "ca.crt"
}

func dsnSecretKey(ref *kubeneuronv1alpha1.SecretReference) string {
	if ref != nil && ref.Key != "" {
		return ref.Key
	}
	return "dsn"
}

func validateDependency(name string, dependency kubeneuronv1alpha1.DependencySpec) error {
	mode := dependency.Mode
	if mode == "" {
		mode = kubeneuronv1alpha1.IntegrationModeExternal
	}
	if dependency.SecretRef != nil {
		return fmt.Errorf("%s secretRef is not supported by the current runtime", name)
	}
	switch mode {
	case kubeneuronv1alpha1.IntegrationModeExternal:
		if strings.TrimSpace(dependency.Endpoint) == "" {
			return fmt.Errorf("external %s requires a non-empty endpoint", name)
		}
	case kubeneuronv1alpha1.IntegrationModeManaged:
		return fmt.Errorf("%s mode Managed is not supported: dependency discovery and readiness are not implemented", name)
	case kubeneuronv1alpha1.IntegrationModeDisabled:
		return fmt.Errorf("%s cannot be disabled", name)
	default:
		return fmt.Errorf("%s has unsupported mode %q", name, mode)
	}
	return nil
}

func validateArchive(archive *kubeneuronv1alpha1.ArchiveSpec) error {
	if archive == nil || archive.ClickHouse == nil {
		return nil
	}
	dependency := archive.ClickHouse
	if dependency.Mode != kubeneuronv1alpha1.IntegrationModeDisabled {
		return fmt.Errorf("spec.archive.clickHouse is not supported: archive ingestion is not implemented")
	}
	if dependency.Endpoint != "" || dependency.SecretRef != nil {
		return fmt.Errorf("disabled clickHouse cannot set endpoint or secretRef")
	}
	return nil
}

func effectiveExecutionMode(s kubeneuronv1alpha1.SafetySpec) kubeneuronv1alpha1.ExecutionMode {
	if s.ExecutionMode == "" {
		return kubeneuronv1alpha1.ExecutionModeDryRun
	}
	return s.ExecutionMode
}

func configDuration(value, defaultValue string) (config.Duration, error) {
	if value == "" {
		value = defaultValue
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	// These are windows and TTLs (flap, verify quiet, approval). A negative value
	// silently subverts the declared intent — a negative flapWindow disables flap
	// quarantine entirely instead of tightening it — and a zero window/TTL is
	// meaningless, so both are rejected here rather than compiling into a snapshot
	// that behaves nothing like what was written.
	if duration <= 0 {
		return 0, fmt.Errorf("duration must be positive, got %q", value)
	}
	return config.Duration(duration), nil
}

func effectivePositive(value int32, defaultValue int) int {
	if value <= 0 {
		return defaultValue
	}
	return int(value)
}

func compilePolicies(
	installation *kubeneuronv1alpha1.KubeNeuron,
	policies []kubeneuronv1alpha1.GPURemediationPolicy,
	books map[string]*playbook.Playbook,
) ([]config.Policy, error) {
	sorted := make([]kubeneuronv1alpha1.GPURemediationPolicy, 0, len(policies))
	for i := range policies {
		if policies[i].Spec.KubeNeuronRef == installation.Name {
			sorted = append(sorted, policies[i])
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Spec.Priority != sorted[j].Spec.Priority {
			return sorted[i].Spec.Priority < sorted[j].Spec.Priority
		}
		return sorted[i].Name < sorted[j].Name
	})

	seenClass := map[string]int32{}
	result := make([]config.Policy, 0, len(sorted))
	for i := range sorted {
		policy := &sorted[i]
		if policy.Spec.Match.Class == "" {
			return nil, fmt.Errorf("policy %q: spec.match.class is required", policy.Name)
		}
		if policy.Spec.Match.Source != "" || policy.Spec.Match.Severity != "" || policy.Spec.Match.NodeSelector != nil {
			return nil, fmt.Errorf("policy %q uses match fields not supported by the current runtime", policy.Name)
		}
		if previous, exists := seenClass[policy.Spec.Match.Class]; exists && previous == policy.Spec.Priority {
			return nil, fmt.Errorf("policies for class %q have duplicate priority %d", policy.Spec.Match.Class, policy.Spec.Priority)
		}
		seenClass[policy.Spec.Match.Class] = policy.Spec.Priority
		if _, ok := books[policy.Spec.PlaybookRef]; !ok {
			return nil, fmt.Errorf("policy %q references missing playbook %q", policy.Name, policy.Spec.PlaybookRef)
		}
		result = append(result, config.Policy{
			Match:    config.Match{Class: types.ProblemClass(policy.Spec.Match.Class)},
			Playbook: policy.Spec.PlaybookRef,
			Params:   copyStringMap(policy.Spec.Parameters),
		})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("at least one GPURemediationPolicy must reference installation %q", installation.Name)
	}
	return result, nil
}

// compilePlaybookIndex compiles the playbooks referenced by one installation
// and validates their escalation graph. Keeping this as the one compilation
// path lets child-CR status expose exactly the same effective playbook that
// the runtime snapshot receives.
func compilePlaybookIndex(installationName string, playbooks []kubeneuronv1alpha1.GPUPlaybook) (map[string]*playbook.Playbook, error) {
	books := make(map[string]*playbook.Playbook, len(playbooks))
	for i := range playbooks {
		candidate := &playbooks[i]
		if candidate.Spec.KubeNeuronRef != installationName {
			continue
		}
		book, err := compilePlaybook(candidate)
		if err != nil {
			return nil, err
		}
		if _, exists := books[book.Name]; exists {
			return nil, fmt.Errorf("duplicate playbook %q", book.Name)
		}
		books[book.Name] = book
	}
	if err := playbook.ValidateEscalationGraph(books); err != nil {
		return nil, err
	}
	return books, nil
}

func compilePlaybook(in *kubeneuronv1alpha1.GPUPlaybook) (*playbook.Playbook, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("playbook name is required")
	}
	if in.Spec.Target != kubeneuronv1alpha1.PlaybookTargetGPU && in.Spec.Target != kubeneuronv1alpha1.PlaybookTargetNode {
		return nil, fmt.Errorf("playbook %q: target must be GPU or Node", in.Name)
	}
	if len(in.Spec.Steps) == 0 {
		return nil, fmt.Errorf("playbook %q: at least one step is required", in.Name)
	}
	nodeScoped := false
	cooldown, err := playbookDuration(in.Spec.Cooldown)
	if err != nil {
		return nil, fmt.Errorf("playbook %q cooldown: %w", in.Name, err)
	}
	steps := make([]playbook.Step, 0, len(in.Spec.Steps))
	for _, step := range in.Spec.Steps {
		action, scope, err := legacyAction(step.Action)
		if err != nil {
			return nil, fmt.Errorf("playbook %q step %q: %w", in.Name, step.Name, err)
		}
		if scope == "node" {
			nodeScoped = true
		}
		approval := "none"
		switch step.Approval {
		case "", kubeneuronv1alpha1.ApprovalNone:
		case kubeneuronv1alpha1.ApprovalRequired:
			approval = "required"
		default:
			return nil, fmt.Errorf("playbook %q step %q: unknown approval requirement %q", in.Name, step.Name, step.Approval)
		}
		// Every whole-VM action requires an explicit human decision: they
		// restart or destroy the instance, not just a process on it.
		if requiresApproval(step.Action) && approval != "required" {
			return nil, fmt.Errorf("playbook %q step %q: %s requires approval Required", in.Name, step.Name, step.Action)
		}
		timeout, err := playbookDuration(step.Timeout)
		if err != nil {
			return nil, fmt.Errorf("playbook %q step %q timeout: %w", in.Name, step.Name, err)
		}
		steps = append(steps, playbook.Step{
			Name:     step.Name,
			Action:   action,
			Approval: approval,
			Timeout:  timeout,
			Params:   copyStringMap(step.Params),
		})
	}
	if in.Spec.Target == kubeneuronv1alpha1.PlaybookTargetGPU && nodeScoped && !hasEffect(in.Spec.Effects, "nodeScheduling") {
		return nil, fmt.Errorf("playbook %q: GPU target with node-scoped steps requires effects: [nodeScheduling]", in.Name)
	}
	if in.Spec.Target == kubeneuronv1alpha1.PlaybookTargetNode && !nodeScoped && !hasEffect(in.Spec.Effects, "node") {
		return nil, fmt.Errorf("playbook %q: Node target requires a node-scoped step or effects: [node]", in.Name)
	}

	target := "gpu"
	if in.Spec.Target == kubeneuronv1alpha1.PlaybookTargetNode {
		target = "node"
	}
	book := &playbook.Playbook{
		Name:     in.Name,
		Target:   target,
		Cooldown: cooldown,
		Steps:    steps,
	}
	if in.Spec.OnFailure != nil {
		book.OnFailure.EscalateTo = in.Spec.OnFailure.PlaybookRef
	}
	if err := book.Validate(); err != nil {
		return nil, err
	}
	return book, nil
}

// requiresApproval reports whether an action restarts or destroys a whole node
// and therefore must not run without a human decision. It reads the registry's
// ForcesApproval flag so the whole-VM set lives in exactly one place.
func requiresApproval(a kubeneuronv1alpha1.PlaybookAction) bool {
	def, ok := action.ByPlaybookAction(a)
	return ok && def.ForcesApproval
}

// legacyAction maps a CRD enum action to its runtime wire string and
// failure-domain scope, both taken from the action registry.
func legacyAction(a kubeneuronv1alpha1.PlaybookAction) (wire string, scope string, err error) {
	def, ok := action.ByPlaybookAction(a)
	if !ok {
		return "", "", fmt.Errorf("unsupported action %q", a)
	}
	return def.Wire, string(def.Scope), nil
}

func playbookDuration(value string) (playbook.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	// Zero is the legitimate "unset" for a cooldown or step timeout, but a
	// negative duration is nonsense that would otherwise compile — a negative
	// cooldown never gates, a negative timeout diverges from the declared budget.
	if duration < 0 {
		return 0, fmt.Errorf("duration must not be negative, got %q", value)
	}
	return playbook.Duration(duration), nil
}

func hasEffect(effects []string, effect string) bool {
	for _, candidate := range effects {
		if strings.EqualFold(candidate, effect) {
			return true
		}
	}
	return false
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// compileAcceleratorRuntimeProfiles turns Kubernetes-native profile resources
// into the controller's digest-covered, server-owned configuration. Profiles
// are selectors, not priorities: any possible overlap is rejected here so a
// node can never receive a capability decision based on object/list order.
//
// The current CRD deliberately supports only matchLabels. Should a future API
// need matchExpressions, it must add a complete selector-intersection proof
// before this fail-closed compiler can accept it.
func compileAcceleratorRuntimeProfiles(
	installationName string,
	profiles []kubeneuronv1alpha1.AcceleratorRuntimeProfile,
) ([]config.AcceleratorRuntimeProfile, error) {
	selected := make([]kubeneuronv1alpha1.AcceleratorRuntimeProfile, 0, len(profiles))
	for i := range profiles {
		profile := profiles[i]
		if profile.Spec.KubeNeuronRef == installationName {
			selected = append(selected, profile)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })

	seenNames := make(map[string]struct{}, len(selected))
	compiled := make([]config.AcceleratorRuntimeProfile, 0, len(selected))
	for i := range selected {
		profile := &selected[i]
		if _, duplicate := seenNames[profile.Name]; duplicate {
			return nil, fmt.Errorf("accelerator runtime profiles contain duplicate name %q", profile.Name)
		}
		seenNames[profile.Name] = struct{}{}
		if len(profile.Spec.NodeSelector.MatchExpressions) != 0 {
			return nil, fmt.Errorf("accelerator runtime profile %q: nodeSelector.matchExpressions is not supported; use non-empty matchLabels", profile.Name)
		}
		if len(profile.Spec.NodeSelector.MatchLabels) == 0 {
			return nil, fmt.Errorf("accelerator runtime profile %q: nodeSelector.matchLabels is required and cannot select every node", profile.Name)
		}
		if _, err := metav1.LabelSelectorAsSelector(&profile.Spec.NodeSelector); err != nil {
			return nil, fmt.Errorf("accelerator runtime profile %q: invalid nodeSelector: %w", profile.Name, err)
		}

		candidate := config.AcceleratorRuntimeProfile{
			Name:              profile.Name,
			NodeSelector:      copyStringMap(profile.Spec.NodeSelector.MatchLabels),
			Vendor:            types.AcceleratorVendor(profile.Spec.Vendor),
			ProfileDigest:     profile.Spec.ProfileDigest,
			DriverVersion:     profile.Spec.DriverVersion,
			RuntimeVersion:    profile.Spec.RuntimeVersion,
			ProfileUID:        string(profile.UID),
			ProfileGeneration: profile.Generation,
			MaxReportAge:      config.Duration(profile.Spec.MaxReportAge.Duration),
			AllowedActions: make([]config.AcceleratorActionPolicy, 0,
				len(profile.Spec.AllowedActions)),
		}
		for _, allowed := range profile.Spec.AllowedActions {
			compiledPolicy := config.AcceleratorActionPolicy{
				Action:                               types.AcceleratorAction(allowed.Action),
				RequireVerifiedUnpartitionedTopology: allowed.RequireVerifiedUnpartitionedTopology,
				Scopes:                               make([]types.AcceleratorTargetScope, 0, len(allowed.Scopes)),
			}
			for _, scope := range allowed.Scopes {
				compiledPolicy.Scopes = append(compiledPolicy.Scopes, types.AcceleratorTargetScope(scope))
			}
			candidate.AllowedActions = append(candidate.AllowedActions, compiledPolicy)
		}
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("accelerator runtime profile %q: %w", profile.Name, err)
		}
		compiled = append(compiled, candidate)
	}

	for i := range compiled {
		for j := 0; j < i; j++ {
			if compiled[i].Vendor == compiled[j].Vendor && selectorsMayOverlap(compiled[i].NodeSelector, compiled[j].NodeSelector) {
				return nil, fmt.Errorf("accelerator runtime profiles %q and %q have overlapping node selectors for vendor %q", compiled[j].Name, compiled[i].Name, compiled[i].Vendor)
			}
		}
	}
	return compiled, nil
}

// selectorsMayOverlap is exact for nonempty Kubernetes matchLabels selectors:
// their intersection is nonempty unless they require different values for the
// same key. The caller has already rejected matchExpressions.
func selectorsMayOverlap(left, right map[string]string) bool {
	for key, leftValue := range left {
		if rightValue, found := right[key]; found && leftValue != rightValue {
			return false
		}
	}
	return true
}

// compileSignalMappings validates the mappings selecting this installation
// and serializes them as detection-catalog overrides. Only the implemented
// subset is accepted: source "xid" with xidCodes, source "alertmanager" with
// alertName, or source "fault" with faults (vendor-neutral codes); label
// matchers stay fail-closed.
func compileSignalMappings(installationName string, mappings []kubeneuronv1alpha1.GPUSignalMapping) ([]byte, error) {
	var overrides []config.SignalOverride
	for i := range mappings {
		m := &mappings[i]
		if m.Spec.KubeNeuronRef != installationName {
			continue
		}
		if len(m.Spec.Labels) > 0 {
			return nil, fmt.Errorf("signal mapping %q: label matchers are not supported by the current runtime", m.Name)
		}
		override := config.SignalOverride{
			Name:      m.Name,
			AlertName: m.Spec.AlertName,
			Class:     types.ProblemClass(m.Spec.Class),
			Severity:  types.Severity(m.Spec.Severity),
		}
		for _, code := range m.Spec.XIDCodes {
			override.XIDCodes = append(override.XIDCodes, int(code))
		}
		for _, f := range m.Spec.Faults {
			override.Faults = append(override.Faults, config.FaultOverride{Vendor: f.Vendor, Code: f.Code})
		}
		switch m.Spec.Source {
		case "xid":
			if len(override.XIDCodes) == 0 {
				return nil, fmt.Errorf("signal mapping %q: source xid requires xidCodes", m.Name)
			}
		case "alertmanager":
			if override.AlertName == "" {
				return nil, fmt.Errorf("signal mapping %q: source alertmanager requires alertName", m.Name)
			}
		case "fault":
			if len(override.Faults) == 0 {
				return nil, fmt.Errorf("signal mapping %q: source fault requires faults", m.Name)
			}
		default:
			return nil, fmt.Errorf("signal mapping %q: source must be \"xid\", \"alertmanager\", or \"fault\", got %q", m.Name, m.Spec.Source)
		}
		if err := override.Validate(); err != nil {
			return nil, err
		}
		overrides = append(overrides, override)
	}
	sort.Slice(overrides, func(i, j int) bool { return overrides[i].Name < overrides[j].Name })
	// A catalog build catches cross-mapping conflicts (duplicate XIDs/alerts).
	if _, err := detect.NewCatalog(overrides); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(struct {
		Overrides []config.SignalOverride `yaml:"overrides"`
	}{Overrides: overrides})
	if err != nil {
		return nil, fmt.Errorf("marshal signal mappings: %w", err)
	}
	return data, nil
}

// compileNodeConfigs validates the node configs selecting this installation.
// SSH/BMC credential references stay fail-closed until an actuator consumes
// them.
func compileNodeConfigs(installationName string, nodeConfigs []kubeneuronv1alpha1.GPUNodeConfig) ([]byte, error) {
	var nodes []config.NodeConfig
	seen := map[string]string{}
	for i := range nodeConfigs {
		n := &nodeConfigs[i]
		if n.Spec.KubeNeuronRef != installationName {
			continue
		}
		if n.Spec.SSHSecretRef != nil || n.Spec.BMCSecretRef != nil {
			return nil, fmt.Errorf("node config %q: sshSecretRef/bmcSecretRef are not supported: no actuator consumes them yet", n.Name)
		}
		if other, dup := seen[n.Spec.NodeName]; dup {
			return nil, fmt.Errorf("node configs %q and %q both configure node %q", other, n.Name, n.Spec.NodeName)
		}
		seen[n.Spec.NodeName] = n.Name
		nodes = append(nodes, config.NodeConfig{NodeName: n.Spec.NodeName, Paused: n.Spec.Paused})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeName < nodes[j].NodeName })
	data, err := yaml.Marshal(struct {
		Nodes []config.NodeConfig `yaml:"nodes"`
	}{Nodes: nodes})
	if err != nil {
		return nil, fmt.Errorf("marshal node configs: %w", err)
	}
	return data, nil
}
