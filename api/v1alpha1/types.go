package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ExecutionMode controls whether the operator may schedule remediation.
// +kubebuilder:validation:Enum=DryRun;Enabled;Paused
type ExecutionMode string

const (
	ExecutionModeDryRun  ExecutionMode = "DryRun"
	ExecutionModeEnabled ExecutionMode = "Enabled"
	ExecutionModePaused  ExecutionMode = "Paused"
)

// IntegrationMode describes whether a dependency is supplied by the cluster
// or managed by its dedicated upstream operator.
// +kubebuilder:validation:Enum=External;Managed;Disabled
type IntegrationMode string

const (
	IntegrationModeExternal IntegrationMode = "External"
	IntegrationModeManaged  IntegrationMode = "Managed"
	IntegrationModeDisabled IntegrationMode = "Disabled"
)

// CloudSpec configures node recycle/replace against a cloud provider. On AWS a
// RecycleNode stops/starts the EC2 instance (reinitializing the GPU
// passthrough) and a ReplaceNode terminates it for the autoscaler.
//
// Provider-specific settings live in a per-provider sub-block (aws today) so a
// future provider adds its own block rather than widening a shared top level:
// nothing here is cloud-specific except which block the provider selects.
// +kubebuilder:validation:XValidation:rule="self.provider != 'aws' || has(self.aws)",message="spec.cloud.aws is required when provider is aws"
type CloudSpec struct {
	// Provider is the cloud backing the nodes. Only "aws" is implemented.
	// +kubebuilder:validation:Enum=aws
	Provider string `json:"provider"`
	// AWS carries the AWS-specific configuration. Required when provider is aws.
	AWS *AWSCloudSpec `json:"aws,omitempty"`
}

// AWSCloudSpec configures the AWS EC2 backing for node recycle/replace.
type AWSCloudSpec struct {
	// Region is the EC2 region (e.g. us-east-1). Empty uses the controller's
	// ambient region (AWS_REGION).
	Region string `json:"region,omitempty"`
	// IAMRoleARN is set as the eks.amazonaws.com/role-arn annotation on the
	// controller ServiceAccount so IRSA grants ec2:Stop/Start/Terminate/
	// DescribeInstances. Required unless the role is attached another way.
	IAMRoleARN string `json:"iamRoleARN,omitempty"`
}

// TLSIssuer names who fills in an installation's TLS Secrets.
// +kubebuilder:validation:Enum=External;Operator
type TLSIssuer string

const (
	// TLSIssuerExternal leaves the material to whoever created it.
	TLSIssuerExternal TLSIssuer = "External"
	// TLSIssuerOperator has KubeNeuron issue and renew the material.
	TLSIssuerOperator TLSIssuer = "Operator"
)

// PlaybookTarget is the failure-domain target a playbook addresses.
// +kubebuilder:validation:Enum=GPU;Node
// +kubebuilder:validation:Enum=GPU;Node
type PlaybookTarget string

const (
	PlaybookTargetGPU  PlaybookTarget = "GPU"
	PlaybookTargetNode PlaybookTarget = "Node"
)

// PlaybookAction is deliberately closed. Arbitrary commands must never be
// admitted through a configuration CRD.
// +kubebuilder:validation:Enum=Observe;Cordon;Drain;Uncordon;EvictGPUWorkload;IdleCheck;WaitIdle;QuiesceAcceleratorStack;RestoreAcceleratorStack;GPUReset;CollectBundle;Reboot;RecycleNode;ReplaceNode;RunDiag;VerifyGPUHealth;VerifyNodeHealth;OpenTicket
type PlaybookAction string

const (
	ActionObserve          PlaybookAction = "Observe"
	ActionCordon           PlaybookAction = "Cordon"
	ActionDrain            PlaybookAction = "Drain"
	ActionUncordon         PlaybookAction = "Uncordon"
	ActionEvictGPUWorkload PlaybookAction = "EvictGPUWorkload"
	ActionIdleCheck        PlaybookAction = "IdleCheck"
	ActionWaitIdle         PlaybookAction = "WaitIdle"
	// ActionQuiesceAcceleratorStack stops the GPU vendor's own monitoring
	// components on the node. They hold open handles on the device nodes, so a
	// GPUReset fails while they run no matter how well the node was drained.
	ActionQuiesceAcceleratorStack PlaybookAction = "QuiesceAcceleratorStack"
	// ActionRestoreAcceleratorStack puts back exactly what the quiesce step
	// stopped. The controller also restores automatically once an incident
	// stops running, so a playbook that fails midway cannot leave monitoring
	// switched off.
	ActionRestoreAcceleratorStack PlaybookAction = "RestoreAcceleratorStack"
	ActionGPUReset                PlaybookAction = "GPUReset"
	ActionCollectBundle           PlaybookAction = "CollectBundle"
	ActionReboot                  PlaybookAction = "Reboot"
	// ActionRecycleNode stops and starts the node's cloud instance. It is the
	// cloud-native stand-in for a GPU reset on a virtualized instance, where a
	// hardware reset is impossible: stop/start reinitializes the GPU passthrough
	// on the same node. Whole-VM restart, so approval is required.
	ActionRecycleNode PlaybookAction = "RecycleNode"
	// ActionReplaceNode terminates the node's cloud instance for the autoscaler
	// to replace with a fresh one. The escalation rung after RecycleNode.
	ActionReplaceNode      PlaybookAction = "ReplaceNode"
	ActionRunDiag          PlaybookAction = "RunDiag"
	ActionVerifyGPUHealth  PlaybookAction = "VerifyGPUHealth"
	ActionVerifyNodeHealth PlaybookAction = "VerifyNodeHealth"
	ActionOpenTicket       PlaybookAction = "OpenTicket"
)

// ApprovalRequirement controls whether a step requires an operator decision.
// +kubebuilder:validation:Enum=None;Required
type ApprovalRequirement string

const (
	ApprovalNone     ApprovalRequirement = "None"
	ApprovalRequired ApprovalRequirement = "Required"
)

// SecretReference refers to a Kubernetes Secret. Credentials must never live
// directly in a KubeNeuron custom resource.
type SecretReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`
}

// ComponentSpec configures a managed controller component.
type ComponentSpec struct {
	// AllowGPUNodes lets the controller be scheduled onto GPU nodes.
	//
	// Off by default, and worth leaving off: a reboot playbook has been
	// observed rebooting the node its own controller was running on, which cut
	// the playbook off mid-step and left the node cordoned. Enable it only on
	// clusters where every node has a GPU, and accept that a node reboot will
	// interrupt whatever the controller was doing.
	AllowGPUNodes bool `json:"allowGPUNodes,omitempty"`
	// Image must be pinned by an explicit tag or digest; the floating
	// :latest tag is rejected so a managed rollout is always reproducible.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="!self.endsWith(':latest')",message="image must not use the floating :latest tag"
	// +kubebuilder:validation:XValidation:rule="self.contains('@') || self.lastIndexOf(':') > self.lastIndexOf('/')",message="image must be pinned by an explicit tag or digest"
	Image     string                      `json:"image"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// AgentSpec configures the node agent managed by the operator.
type AgentSpec struct {
	// Image must be pinned by an explicit tag or digest; the floating
	// :latest tag is rejected so a managed rollout is always reproducible.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="!self.endsWith(':latest')",message="image must not use the floating :latest tag"
	// +kubebuilder:validation:XValidation:rule="self.contains('@') || self.lastIndexOf(':') > self.lastIndexOf('/')",message="image must be pinned by an explicit tag or digest"
	Image        string              `json:"image"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	// HostTooling opts the agent DaemonSet into read-only host mounts of the
	// NVIDIA userspace tooling (nvidia-smi, dcgmi, driver libraries) so the
	// distroless agent image can execute them. Setting it also arms
	// --require-real-driver: with host tooling declared, a silent Fake-driver
	// fallback becomes a startup failure instead of a lie. Leave unset on
	// CPU-only installs and on nodes where the device plugin injects the
	// tooling by other means.
	HostTooling *HostToolingSpec `json:"hostTooling,omitempty"`
}

// HostToolingSpec mounts NVIDIA userspace tooling from the node into the
// agent container. All paths are host paths and are mounted read-only; the
// defaults match the AL2023 EKS NVIDIA AMI (nvidia-smi in /usr/bin, driver
// libraries in /usr/lib64).
type HostToolingSpec struct {
	// BinDir is the host directory containing nvidia-smi (and dcgmi when
	// present). It is appended to the agent's PATH.
	// +kubebuilder:default="/usr/bin"
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/') && !self.contains(':')",message="binDir must be an absolute host path without ':'"
	BinDir string `json:"binDir,omitempty"`
	// LibDirs are host directories holding the NVIDIA driver userspace
	// libraries nvidia-smi is linked against; they become the agent's
	// LD_LIBRARY_PATH. Defaults to /usr/lib64 (AL2023 NVIDIA AMI).
	// +kubebuilder:default={"/usr/lib64"}
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:validation:items:XValidation:rule="self.startsWith('/') && !self.contains(':')",message="libDirs entries must be absolute host paths without ':'"
	LibDirs []string `json:"libDirs,omitempty"`
	// ScriptsDir optionally mounts operator-provisioned remediation scripts
	// read-only at /etc/kube-neuron/scripts (the agent's --scripts-dir).
	// The run_script action additionally requires destructive enablement,
	// which the operator does not set; the mount alone executes nothing.
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/') && !self.contains(':')",message="scriptsDir must be an absolute host path without ':'"
	ScriptsDir string `json:"scriptsDir,omitempty"`
	// DCGMEndpoint is where this node's DCGM host engine listens. Runtime
	// attestation — the evidence every destructive action depends on —
	// needs to reach it.
	//
	// With the NVIDIA GPU Operator this is
	// "nvidia-dcgm.gpu-operator.svc:5555" — measured, not assumed: the
	// operator runs the engine as an ordinary pod with no host port, so
	// neither the node's address nor its loopback reaches it. That Service
	// carries internalTrafficPolicy=Local, which is what keeps the evidence
	// node-local: a request only ever lands on the engine sharing this
	// node. Verify that policy before pointing this at any other Service,
	// or attestation quietly becomes hearsay about someone else's hardware.
	//
	// The engine must exist: the operator ships it disabled
	// (dcgm.enabled=false) because its exporter embeds a private one. Empty
	// here keeps dcgmi's local default and, with no reachable engine,
	// leaves the report degraded — fail-closed, no destructive action.
	// +kubebuilder:validation:XValidation:rule="self.contains(':')",message="dcgmEndpoint must be host:port, for example nvidia-dcgm.gpu-operator.svc:5555"
	DCGMEndpoint string `json:"dcgmEndpoint,omitempty"`
	// DCGMIPath selects a dcgmi client already installed on the node instead
	// of the one shipped in the agent image.
	//
	// Leave it unset unless you mean it. The attested runtime version is
	// whichever client answers, and an AcceleratorRuntimeProfile pins one exact
	// value; taking the client from the node makes that value depend on how
	// each node was provisioned, so a fleet built from mixed images would
	// attest different versions and part of it would go degraded. Set this only
	// when the node's client is deliberately the one your profile pins.
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/') && !self.contains(':')",message="dcgmiPath must be an absolute host path without ':'"
	DCGMIPath string `json:"dcgmiPath,omitempty"`
}

// SafetySpec holds execution limits. ExecutionMode defaults to DryRun in the
// operator, even when omitted from the CR.
type SafetySpec struct {
	// +kubebuilder:default=DryRun
	ExecutionMode ExecutionMode `json:"executionMode,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	MaxConcurrentRemediations int32 `json:"maxConcurrentRemediations,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MaxConcurrentReboots int32 `json:"maxConcurrentReboots,omitempty"`
	// +kubebuilder:default="10m"
	VerifyQuietWindow string `json:"verifyQuietWindow,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	FlapCount int32 `json:"flapCount,omitempty"`
	// +kubebuilder:default="24h"
	FlapWindow string `json:"flapWindow,omitempty"`
	// DestructiveExecution is the only way to reach executionMode Enabled.
	// It arms real reset, reboot, and driver actions, and it deliberately
	// cannot be switched on fleet-wide: a node selector is mandatory, and
	// only nodes matching it ever receive an agent armed for destructive
	// work.
	DestructiveExecution *DestructiveExecutionSpec `json:"destructiveExecution,omitempty"`
	// Quiesce tunes how the vendor's own components are stood down before a
	// GPU reset.
	Quiesce *QuiesceSpec `json:"quiesce,omitempty"`
	// TaintDegradedNodes optionally stops a node under an open incident from
	// attracting NEW GPU work, before any playbook cordons it.
	//
	// Omitted means off. Scheduling is the one thing every workload in the
	// cluster depends on, so this must never arrive by default with an
	// upgrade.
	TaintDegradedNodes *TaintDegradedNodesSpec `json:"taintDegradedNodes,omitempty"`
}

// NodeTaintEffect is the scheduling effect of the degraded-node taint.
// +kubebuilder:validation:Enum=PreferNoSchedule;NoSchedule
type NodeTaintEffect string

const (
	// TaintEffectPreferNoSchedule biases the scheduler away from the node
	// without forbidding anything.
	TaintEffectPreferNoSchedule NodeTaintEffect = "PreferNoSchedule"
	// TaintEffectNoSchedule refuses every new pod that does not tolerate the
	// taint.
	TaintEffectNoSchedule NodeTaintEffect = "NoSchedule"
)

// TaintDegradedNodesSpec configures the kubeneuron.io/degraded taint: applied
// when an incident starts working on a node, removed when that incident halts.
//
// It is scheduler feedback, not remediation. A node that has already begun
// failing keeps being handed fresh jobs right up to the moment a playbook
// cordons it, and every job started in that window is one more workload the
// remediation has to evict. The taint closes that window without taking any
// capacity away: nothing already running is touched.
type TaintDegradedNodesSpec struct {
	// Enabled turns the taint on. Default false, and false is also what an
	// absent block means: KubeNeuron does not touch node scheduling until
	// somebody asks it to.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
	// Effect selects how hard the hint is.
	//
	// PreferNoSchedule, the default, is a bias: the scheduler avoids the node
	// when it has somewhere else to put the pod, and still uses it rather than
	// leaving work pending. NoSchedule is a wall — for a node under an OPEN
	// incident, whose fault may yet turn out to be benign, that is a bigger
	// hammer than the cordon a playbook applies deliberately and after
	// evidence, which is why it is never the default.
	// +kubebuilder:default=PreferNoSchedule
	Effect NodeTaintEffect `json:"effect,omitempty"`
}

// QuiesceSpec declares node-side facts KubeNeuron cannot discover on its own.
type QuiesceSpec struct {
	// ForbidResetWhenPresent lists process names whose presence on a node means
	// a per-device reset must not be attempted there at all.
	//
	// Some processes hold the GPU and must not be stopped. `nv-fabricmanager`
	// is the standing example: on NVSwitch machines it is required for the GPUs
	// to function, and the honest answer is "this node is not reset per-device"
	// rather than killing a critical daemon to make room for a remediation.
	// Declaring it here turns a reset attempt into an early, explained refusal
	// instead of a cordoned and drained node followed by a failure.
	//
	// Names are matched against the process name the node reports (Linux
	// truncates these to 15 characters, so give the truncated form when it is
	// longer).
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	ForbidResetWhenPresent []string `json:"forbidResetWhenPresent,omitempty"`
}

// DestructiveExecutionSpec confines real destructive actions to an
// explicitly named set of nodes. Every field is required on purpose: a
// half-configured block must not resolve to "the whole fleet".
type DestructiveExecutionSpec struct {
	// NodeSelector selects the nodes permitted to execute destructive
	// actions. It must be non-empty, and the operator arms the agent only
	// on nodes that match it.
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`
	// Acknowledgement must read exactly
	// "I understand these nodes may be reset, rebooted, or destroyed".
	// A typo is a rejected installation, not a rebooted production node.
	// +kubebuilder:validation:XValidation:rule="self == 'I understand these nodes may be reset, rebooted, or destroyed'",message="acknowledgement text must match exactly"
	Acknowledgement string `json:"acknowledgement"`
}

// ApprovalSpec configures human approval behavior.
type ApprovalSpec struct {
	Channels []string `json:"channels,omitempty"`
	// +kubebuilder:default="12h"
	TTL string `json:"ttl,omitempty"`
}

// NotificationsSpec configures human-facing notification delivery. Enabled
// execution is deliberately unavailable in the current Kubernetes runtime;
// notification delivery remains useful for dry-run and paused installations.
type NotificationsSpec struct {
	// Slack references a Secret in spec.namespace containing a Slack
	// incoming-webhook URL. Key defaults to "webhook-url". Namespace must be
	// omitted. The operator mounts the Secret into the controller; it never
	// reads the URL itself.
	Slack *SecretReference `json:"slack,omitempty"`
	// OperatorAPIToken references a Secret in spec.namespace containing the
	// bearer token for the human/operator API. Key defaults to "token".
	// Namespace must be omitted. The operator mounts this Secret without
	// reading its contents.
	OperatorAPIToken *SecretReference `json:"operatorAPIToken,omitempty"`
	// WebhookToken references a Secret in spec.namespace containing the bearer
	// token required by the Alertmanager webhook. Key defaults to "token".
	// Namespace must be omitted. The public webhook is never left anonymous on
	// an operator-managed installation.
	WebhookToken *SecretReference `json:"webhookToken,omitempty"`
	// Webhook references a Secret in spec.namespace holding a generic
	// notification endpoint: key "url" (required) receives every incident
	// notification as versioned JSON; key "token" (optional) is sent as an
	// Authorization bearer credential. Namespace must be omitted. Delivery
	// is retried with backoff and dead-lettered to the log on failure.
	Webhook *SecretReference `json:"webhook,omitempty"`
	// PagerDuty references a Secret in spec.namespace containing a PagerDuty
	// Events API v2 routing key. Key defaults to "routing-key". Namespace
	// must be omitted. Incidents map to PagerDuty alerts by incident ID:
	// needs-human and approval requests page critical, resolution resolves
	// the alert.
	PagerDuty *SecretReference `json:"pagerduty,omitempty"`
}

// AuthSpec configures how humans sign in to the operator API and panel.
// Bearer tokens (static break-glass and Kubernetes TokenReview) keep
// working regardless; this adds browser-friendly sign-in.
type AuthSpec struct {
	// Users references a Secret in spec.namespace enabling username and
	// password sign-in: every Secret key is a username and its value a
	// bcrypt password hash (generate with
	// `htpasswd -bnBC 12 "" 'pass' | tr -d ':\n'`). Namespace must be
	// omitted. The operator mounts the Secret; hashes are re-read per
	// attempt, so rotation needs no restart.
	Users *SecretReference `json:"users,omitempty"`
	// OIDC enables single sign-on through any spec-compliant provider
	// (Okta, Keycloak, Dex, Entra, Google, …).
	OIDC *OIDCSpec `json:"oidc,omitempty"`
}

// OIDCSpec configures the authorization-code single sign-on flow.
type OIDCSpec struct {
	// IssuerURL is the provider's issuer, e.g.
	// https://your-org.okta.com or https://keycloak.example/realms/prod.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self.startsWith('https://')",message="issuerURL must be https"
	IssuerURL string `json:"issuerURL"`
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`
	// ClientSecretRef references a Secret in spec.namespace holding the
	// OIDC client secret. Key defaults to "client-secret". Namespace must
	// be omitted.
	ClientSecretRef SecretReference `json:"clientSecretRef"`
	// RedirectURL is the externally reachable callback,
	// https://<panel-host>/api/v1/auth/oidc/callback — register the same
	// value at the provider.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self.startsWith('https://') || self.startsWith('http://')",message="redirectURL must be a URL"
	RedirectURL string `json:"redirectURL"`
	// AllowedEmailDomains optionally restricts sign-in to verified email
	// addresses of these domains; empty admits any principal the issuer
	// authenticates.
	// +kubebuilder:validation:MaxItems=8
	AllowedEmailDomains []string `json:"allowedEmailDomains,omitempty"`
}

// SQLiteStoreSpec configures the persistent volume used by the single-node
// SQLite runtime. Omitting size defaults to 5Gi. Size may grow after claim
// creation, but it cannot shrink, and storageClassName cannot change.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.size) || (has(self.size) && isQuantity(self.size) && isQuantity(oldSelf.size) && quantity(self.size).compareTo(quantity(oldSelf.size)) >= 0)",message="size cannot decrease"
// +kubebuilder:validation:XValidation:rule="has(self.storageClassName) == has(oldSelf.storageClassName) && (!has(self.storageClassName) || self.storageClassName == oldSelf.storageClassName)",message="storageClassName is immutable"
type SQLiteStoreSpec struct {
	// +kubebuilder:default="5Gi"
	// +kubebuilder:validation:XValidation:rule="isQuantity(self) && quantity(self).compareTo(quantity('0')) > 0",message="size must be a positive Kubernetes quantity"
	Size             string  `json:"size"`
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// WorkflowStoreSpec describes the authoritative store for workflow state.
// PostgreSQL is required for a highly available controller; SQLite is only for
// local development and a single controller replica.
// +kubebuilder:validation:XValidation:rule="self.type != 'SQLite' || !has(self.secretRef)",message="secretRef is not supported for SQLite"
// +kubebuilder:validation:XValidation:rule="self.type != 'SQLite' || !has(self.database)",message="database is not supported for SQLite"
// +kubebuilder:validation:XValidation:rule="self.type != 'Postgres' || has(self.secretRef)",message="secretRef is required for Postgres"
// +kubebuilder:validation:XValidation:rule="self.type != 'Postgres' || !has(self.sqlite)",message="sqlite settings are not supported for Postgres"
// +kubebuilder:validation:XValidation:rule="self.type != 'SQLite' || has(self.sqlite)",message="sqlite settings are required for SQLite"
// +kubebuilder:validation:XValidation:rule="self.type != 'Postgres' || !has(self.secretRef) || !has(self.secretRef.namespace)",message="the Postgres DSN Secret must omit namespace and use spec.namespace"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="workflow store type is immutable"
type WorkflowStoreSpec struct {
	// +kubebuilder:validation:Enum=Postgres;SQLite
	Type      string           `json:"type"`
	SecretRef *SecretReference `json:"secretRef,omitempty"`
	Database  string           `json:"database,omitempty"`
	SQLite    *SQLiteStoreSpec `json:"sqlite,omitempty"`
}

// DependencySpec describes a dependency managed by its own upstream operator
// or supplied as an existing endpoint.
// +kubebuilder:validation:XValidation:rule="self.mode != 'External' || (has(self.endpoint) && self.endpoint.size() > 0)",message="External dependencies require an endpoint"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Disabled' || (!has(self.endpoint) && !has(self.secretRef))",message="Disabled dependencies cannot set endpoint or secretRef"
type DependencySpec struct {
	// +kubebuilder:default=External
	Mode      IntegrationMode  `json:"mode,omitempty"`
	Endpoint  string           `json:"endpoint,omitempty"`
	SecretRef *SecretReference `json:"secretRef,omitempty"`
}

// ObservabilitySpec describes metrics and alerting dependencies.
// The preview runtime accepts only explicit external endpoints. Managed
// discovery and credential wiring remain reserved until implemented.
// +kubebuilder:validation:XValidation:rule="self.victoriaMetrics.mode == 'External'",message="victoriaMetrics currently supports only External mode"
// +kubebuilder:validation:XValidation:rule="self.alertmanager.mode == 'External'",message="alertmanager currently supports only External mode"
// +kubebuilder:validation:XValidation:rule="!has(self.victoriaMetrics.secretRef)",message="victoriaMetrics secretRef is not supported"
// +kubebuilder:validation:XValidation:rule="!has(self.alertmanager.secretRef)",message="alertmanager secretRef is not supported"
type ObservabilitySpec struct {
	VictoriaMetrics DependencySpec `json:"victoriaMetrics"`
	Alertmanager    DependencySpec `json:"alertmanager"`
}

// ArchiveSpec describes the optional raw-event archive. It must never be used
// as the workflow lock or incident authority.
// The preview runtime has no archive ingestion path, so ClickHouse may only be
// omitted or explicitly disabled.
// +kubebuilder:validation:XValidation:rule="!has(self.clickHouse) || self.clickHouse.mode == 'Disabled'",message="clickHouse currently supports only Disabled mode"
type ArchiveSpec struct {
	ClickHouse *DependencySpec `json:"clickHouse,omitempty"`
}

// TLSSpec references mTLS material used for agent/controller traffic. The
// operator writes Secret volume references into managed workloads; kubelet
// performs the mounts. The operator never reads or copies private key bytes
// into a ConfigMap or custom-resource status.
// +kubebuilder:validation:XValidation:rule="has(self.serverSecretRef) && has(self.clientCASecretRef) && has(self.clientSecretRef) && has(self.serverCASecretRef)",message="serverSecretRef, clientCASecretRef, clientSecretRef, and serverCASecretRef are required"
// +kubebuilder:validation:XValidation:rule="!has(self.serverSecretRef.key) && !has(self.clientSecretRef.key) && (!has(self.publicServerSecretRef) || !has(self.publicServerSecretRef.key))",message="TLS key-pair Secret references cannot select one key"
type TLSSpec struct {
	// Issuer decides who fills in the Secrets this block names.
	//
	// External, the default, means somebody else owns them: the installer,
	// cert-manager, an organisation's own CA. The operator never writes them,
	// and reports how close they are to expiry so they cannot lapse silently.
	//
	// Operator means KubeNeuron issues and renews them itself — a long-lived
	// authority signing short-lived certificates that are replaced well before
	// they expire.
	//
	// It is deliberately explicit rather than inferred from whether the Secrets
	// happen to exist. "Absent" and "supposed to be mine" are different things:
	// treating a missing Secret as an invitation would let the operator take
	// over material somebody else manages the moment they deleted it mid-
	// rotation.
	// +kubebuilder:validation:Enum=External;Operator
	// +kubebuilder:default=External
	Issuer TLSIssuer `json:"issuer,omitempty"`
	// ServerSecretRef selects a kubernetes.io/tls-style Secret containing
	// tls.crt and tls.key. The certificate must cover the controller Service DNS
	// name and be valid for server authentication.
	// +kubebuilder:validation:Required
	ServerSecretRef *SecretReference `json:"serverSecretRef,omitempty"`
	// ClientCASecretRef selects the CA bundle used by the controller to verify
	// agent fleet certificates. Key defaults to ca.crt.
	// +kubebuilder:validation:Required
	ClientCASecretRef *SecretReference `json:"clientCASecretRef,omitempty"`
	// ClientSecretRef selects a kubernetes.io/tls-style Secret mounted by every
	// agent. Its clientAuth leaf must contain exactly the installation URI SAN
	// documented in docs/design.md; the projected Pod token supplies node
	// identity, so this shared leaf is not treated as a node credential.
	// +kubebuilder:validation:Required
	ClientSecretRef *SecretReference `json:"clientSecretRef,omitempty"`
	// ServerCASecretRef selects the CA bundle agents use to verify the
	// controller Service certificate. Key defaults to ca.crt.
	// +kubebuilder:validation:Required
	ServerCASecretRef *SecretReference `json:"serverCASecretRef,omitempty"`
	// PublicServerSecretRef optionally selects a kubernetes.io/tls-style
	// Secret for the controller's public listener (operator API, Alertmanager
	// webhook, control panel, metrics). When omitted, the public listener
	// serves plain HTTP and should be protected by an Ingress/Gateway that
	// terminates TLS; when set, bearer tokens never cross the network in
	// cleartext even inside the cluster.
	PublicServerSecretRef *SecretReference `json:"publicServerSecretRef,omitempty"`
}

// KubeNeuronSpec is the root desired-state resource for one KubeNeuron
// installation. All configuration resources reference this object by name.
// +kubebuilder:validation:XValidation:rule="!has(self.tls.serverSecretRef.namespace) && !has(self.tls.clientCASecretRef.namespace) && !has(self.tls.clientSecretRef.namespace) && !has(self.tls.serverCASecretRef.namespace) && (!has(self.tls.publicServerSecretRef) || !has(self.tls.publicServerSecretRef.namespace))",message="TLS Secret references must omit namespace and use spec.namespace"
// +kubebuilder:validation:XValidation:rule="!has(self.safety) || self.safety.executionMode != 'Enabled' || has(self.safety.destructiveExecution)",message="executionMode Enabled requires spec.safety.destructiveExecution: real reset and reboot are confined to explicitly declared nodes"
// +kubebuilder:validation:XValidation:rule="has(self.notifications) && has(self.notifications.webhookToken)",message="notifications.webhookToken is required: Alertmanager ingress must be authenticated"
// +kubebuilder:validation:XValidation:rule="!has(self.safety) || self.safety.executionMode != 'Paused' || (has(self.notifications) && has(self.notifications.operatorAPIToken))",message="notifications.operatorAPIToken is required for Paused mode so the gate can be resumed through the authenticated API"
// +kubebuilder:validation:XValidation:rule="!has(self.auth) || ((!has(self.auth.users) || !has(self.auth.users.namespace)) && (!has(self.auth.oidc) || !has(self.auth.oidc.clientSecretRef.namespace)))",message="auth Secret references must omit namespace and use spec.namespace"
// +kubebuilder:validation:XValidation:rule="!has(self.notifications) || ((!has(self.notifications.slack) || !has(self.notifications.slack.namespace)) && (!has(self.notifications.operatorAPIToken) || !has(self.notifications.operatorAPIToken.namespace)) && (!has(self.notifications.webhookToken) || !has(self.notifications.webhookToken.namespace)) && (!has(self.notifications.webhook) || !has(self.notifications.webhook.namespace)) && (!has(self.notifications.pagerduty) || !has(self.notifications.pagerduty.namespace)))",message="notification Secret references must omit namespace and use spec.namespace"
type KubeNeuronSpec struct {
	// Namespace is immutable because moving an installation would otherwise
	// leave the old namespace's runtime resources active.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namespace is immutable"
	Namespace     string             `json:"namespace"`
	Controller    ComponentSpec      `json:"controller"`
	Agent         AgentSpec          `json:"agent"`
	Auth          *AuthSpec          `json:"auth,omitempty"`
	Safety        SafetySpec         `json:"safety,omitempty"`
	Approvals     ApprovalSpec       `json:"approvals,omitempty"`
	Notifications *NotificationsSpec `json:"notifications,omitempty"`
	WorkflowStore WorkflowStoreSpec  `json:"workflowStore"`
	// Cloud enables node recycle/replace remediation against the cloud provider
	// behind the nodes. Leave empty to disable it; a RecycleNode/ReplaceNode
	// step then fails closed rather than silently succeeding.
	Cloud         *CloudSpec        `json:"cloud,omitempty"`
	Observability ObservabilitySpec `json:"observability"`
	Archive       *ArchiveSpec      `json:"archive,omitempty"`
	TLS           TLSSpec           `json:"tls"`
}

// KubeNeuronStatus summarizes the operator's observed desired state.
type KubeNeuronStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ConfigDigest       string             `json:"configDigest,omitempty"`
	RuntimeConfigMap   string             `json:"runtimeConfigMap,omitempty"`
	PlaybooksConfigMap string             `json:"playbooksConfigMap,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=kneuron
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.safety.executionMode"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// KubeNeuron defines a complete KubeNeuron installation.
type KubeNeuron struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KubeNeuronSpec   `json:"spec"`
	Status            KubeNeuronStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KubeNeuronList contains KubeNeuron objects.
type KubeNeuronList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubeNeuron `json:"items"`
}

// SignalMatch selects normalized signals for a remediation policy.
type SignalMatch struct {
	// +kubebuilder:validation:MinLength=1
	Class        string                `json:"class"`
	Source       string                `json:"source,omitempty"`
	Severity     string                `json:"severity,omitempty"`
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

// GPURemediationPolicySpec binds one normalized signal class to a playbook.
// The runtime consumes only class matching today; the other match fields are
// rejected at admission so a policy cannot silently mean less than it says
// (the compiler applies the same fail-closed rule).
// +kubebuilder:validation:XValidation:rule="!has(self.match.source) && !has(self.match.severity) && !has(self.match.nodeSelector)",message="match fields source/severity/nodeSelector are not supported by the current runtime"
type GPURemediationPolicySpec struct {
	// +kubebuilder:validation:MinLength=1
	KubeNeuronRef string      `json:"kubeNeuronRef"`
	Priority      int32       `json:"priority"`
	Match         SignalMatch `json:"match"`
	// +kubebuilder:validation:MinLength=1
	PlaybookRef string            `json:"playbookRef"`
	Parameters  map[string]string `json:"parameters,omitempty"`
}

// GPURemediationPolicyStatus records validation and reference resolution.
type GPURemediationPolicyStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	ResolvedPlaybook string             `json:"resolvedPlaybook,omitempty"`
	Digest           string             `json:"digest,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gpolicy
// +kubebuilder:subresource:status

// GPURemediationPolicy selects a playbook for a normalized problem class.
type GPURemediationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GPURemediationPolicySpec   `json:"spec"`
	Status            GPURemediationPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPURemediationPolicyList contains GPURemediationPolicy objects.
type GPURemediationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPURemediationPolicy `json:"items"`
}

// PlaybookStep is one typed action in a remediation playbook.
type PlaybookStep struct {
	// +kubebuilder:validation:MinLength=1
	Name     string              `json:"name"`
	Action   PlaybookAction      `json:"action"`
	Approval ApprovalRequirement `json:"approval,omitempty"`
	// Timeout is a Go duration (e.g. "10m").
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	Timeout string            `json:"timeout,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
}

// PlaybookFailure defines escalation after failure.
type PlaybookFailure struct {
	// +kubebuilder:validation:MinLength=1
	PlaybookRef string `json:"playbookRef,omitempty"`
}

// GPUPlaybookSpec describes a typed, declarative remediation workflow.
// Reboot is the most destructive typed action; requiring its approval at
// admission mirrors the compiler's fail-closed rule.
// +kubebuilder:validation:XValidation:rule="self.steps.all(s, s.action != 'Reboot' || (has(s.approval) && s.approval == 'Required'))",message="Reboot steps require approval Required"
type GPUPlaybookSpec struct {
	// +kubebuilder:validation:MinLength=1
	KubeNeuronRef string         `json:"kubeNeuronRef"`
	Target        PlaybookTarget `json:"target"`
	Effects       []string       `json:"effects,omitempty"`
	// Cooldown is a Go duration (e.g. "6h").
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	Cooldown string `json:"cooldown,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Steps     []PlaybookStep   `json:"steps"`
	OnFailure *PlaybookFailure `json:"onFailure,omitempty"`
}

// GPUPlaybookStatus records validation and its immutable effective digest.
type GPUPlaybookStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Digest     string             `json:"digest,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gplaybook
// +kubebuilder:subresource:status

// GPUPlaybook is a typed remediation playbook.
type GPUPlaybook struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GPUPlaybookSpec   `json:"spec"`
	Status            GPUPlaybookStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUPlaybookList contains GPUPlaybook objects.
type GPUPlaybookList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUPlaybook `json:"items"`
}

// FaultCodeSelector names one vendor-native neutral fault code — the
// non-XID analogue of an XID number, matching AgentEvent.Fault (vendor,
// code) as emitted by e.g. the nvidia-smi/DCGM fallback source.
type FaultCodeSelector struct {
	// +kubebuilder:validation:MinLength=1
	Vendor string `json:"vendor"`
	// +kubebuilder:validation:MinLength=1
	Code string `json:"code"`
}

// GPUSignalMappingSpec makes signal normalization declarative. Exactly one
// source matcher should be set for a mapping.
// +kubebuilder:validation:XValidation:rule="(has(self.xidCodes) ? 1 : 0) + (has(self.alertName) ? 1 : 0) + (has(self.faults) ? 1 : 0) == 1",message="exactly one of xidCodes, alertName, or faults must be set"
type GPUSignalMappingSpec struct {
	// +kubebuilder:validation:MinLength=1
	KubeNeuronRef string `json:"kubeNeuronRef"`
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Minimum=1
	XIDCodes []int32 `json:"xidCodes,omitempty"`
	// +kubebuilder:validation:MinLength=1
	AlertName string `json:"alertName,omitempty"`
	// Faults overrides classification of vendor-neutral fault codes (source
	// "fault"), so a policy override reaches both detection encodings: an
	// installation that remaps XID 48 can remap the equivalent neutral
	// "nvidia/ecc-dbe" the fallback source emits, instead of the two sources
	// silently applying different policy for one physical condition.
	// +kubebuilder:validation:MinItems=1
	Faults []FaultCodeSelector `json:"faults,omitempty"`
	Labels map[string]string   `json:"labels,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Class string `json:"class"`
	// +kubebuilder:validation:Enum=info;warning;critical
	Severity string `json:"severity"`
}

// GPUSignalMappingStatus records mapping validation.
type GPUSignalMappingStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Digest     string             `json:"digest,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gsignal
// +kubebuilder:subresource:status

// GPUSignalMapping configures XID or alert normalization.
type GPUSignalMapping struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GPUSignalMappingSpec   `json:"spec"`
	Status            GPUSignalMappingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUSignalMappingList contains GPUSignalMapping objects.
type GPUSignalMappingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUSignalMapping `json:"items"`
}

// GPUMaintenanceWindowSpec pauses automation for nodes selected during a
// bounded maintenance window.
// +kubebuilder:validation:XValidation:rule="self.startsAt < self.endsAt",message="startsAt must be before endsAt"
type GPUMaintenanceWindowSpec struct {
	// +kubebuilder:validation:MinLength=1
	KubeNeuronRef   string               `json:"kubeNeuronRef"`
	NodeSelector    metav1.LabelSelector `json:"nodeSelector"`
	StartsAt        *metav1.Time         `json:"startsAt"`
	EndsAt          *metav1.Time         `json:"endsAt"`
	PauseAutomation bool                 `json:"pauseAutomation"`
}

// GPUMaintenanceWindowStatus records whether a maintenance window is active.
type GPUMaintenanceWindowStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
	AffectedNodes int32              `json:"affectedNodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gmaint
// +kubebuilder:subresource:status

// GPUMaintenanceWindow pauses automation for matching nodes.
type GPUMaintenanceWindow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GPUMaintenanceWindowSpec   `json:"spec"`
	Status            GPUMaintenanceWindowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUMaintenanceWindowList contains GPUMaintenanceWindow objects.
type GPUMaintenanceWindowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUMaintenanceWindow `json:"items"`
}

// GPUNodeConfigSpec holds per-node desired configuration. Secret references
// preserve address and credential ownership without agents being able to
// overwrite inventory fields.
// SSH/BMC credential references stay fail-closed at admission (as in the
// compiler) until an actuator consumes them.
// +kubebuilder:validation:XValidation:rule="!has(self.sshSecretRef) && !has(self.bmcSecretRef)",message="sshSecretRef/bmcSecretRef are not supported: no actuator consumes them yet"
type GPUNodeConfigSpec struct {
	// +kubebuilder:validation:MinLength=1
	KubeNeuronRef string `json:"kubeNeuronRef"`
	// +kubebuilder:validation:MinLength=1
	NodeName     string           `json:"nodeName"`
	Paused       bool             `json:"paused,omitempty"`
	SSHSecretRef *SecretReference `json:"sshSecretRef,omitempty"`
	BMCSecretRef *SecretReference `json:"bmcSecretRef,omitempty"`
}

// GPUNodeConfigStatus reflects the native node and agent observation.
type GPUNodeConfigStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
	NodeUID       string             `json:"nodeUID,omitempty"`
	AgentLastSeen *metav1.Time       `json:"agentLastSeen,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gnodecfg
// +kubebuilder:subresource:status

// GPUNodeConfig configures one physical node.
type GPUNodeConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GPUNodeConfigSpec   `json:"spec"`
	Status            GPUNodeConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUNodeConfigList contains GPUNodeConfig objects.
type GPUNodeConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUNodeConfig `json:"items"`
}

// AcceleratorRuntimeVendor identifies the accelerator runtime contract a
// profile governs. NVIDIA is intentionally the only admitted contract in this
// API version: a later vendor must add its own explicit preflight and
// remediation safety contract instead of inheriting NVIDIA permissions.
// +kubebuilder:validation:Enum=nvidia
type AcceleratorRuntimeVendor string

const (
	AcceleratorRuntimeVendorNVIDIA AcceleratorRuntimeVendor = "nvidia"
)

// AcceleratorRuntimeAction is a semantic remediation capability. It names a
// controller-level action, never an arbitrary command or an enablement switch.
// +kubebuilder:validation:Enum=evacuate-workloads;quarantine-node;reset-device;restart-runtime;reboot-node;collect-diagnostics;verify-health;replace-node
type AcceleratorRuntimeAction string

const (
	AcceleratorRuntimeActionEvacuateWorkloads  AcceleratorRuntimeAction = "evacuate-workloads"
	AcceleratorRuntimeActionQuarantineNode     AcceleratorRuntimeAction = "quarantine-node"
	AcceleratorRuntimeActionResetDevice        AcceleratorRuntimeAction = "reset-device"
	AcceleratorRuntimeActionRestartRuntime     AcceleratorRuntimeAction = "restart-runtime"
	AcceleratorRuntimeActionRebootNode         AcceleratorRuntimeAction = "reboot-node"
	AcceleratorRuntimeActionCollectDiagnostics AcceleratorRuntimeAction = "collect-diagnostics"
	AcceleratorRuntimeActionVerifyHealth       AcceleratorRuntimeAction = "verify-health"
	AcceleratorRuntimeActionReplaceNode        AcceleratorRuntimeAction = "replace-node"
)

// AcceleratorRuntimeScope identifies the failure-domain scope at which an
// action can be considered. A physical device and a partition remain distinct
// so a logical partition can never silently inherit a physical reset.
// +kubebuilder:validation:Enum=node;physical-device;partition
type AcceleratorRuntimeScope string

const (
	AcceleratorRuntimeScopeNode           AcceleratorRuntimeScope = "node"
	AcceleratorRuntimeScopePhysicalDevice AcceleratorRuntimeScope = "physical-device"
	AcceleratorRuntimeScopePartition      AcceleratorRuntimeScope = "partition"
)

// AcceleratorRuntimeActionPolicy permits a semantic action at explicit target
// scopes. It is only a server-owned allow-list: it does not enable execution
// and must still be combined with a fresh, matching agent report and all
// normal controller safety gates.
//
// A reset is deliberately narrower than other semantic actions. It may target
// only a physical device and must require verified-unpartitioned topology; an
// agent report remains the runtime evidence for that precondition.
// +kubebuilder:validation:XValidation:rule="self.action != 'reset-device' || (self.scopes.all(scope, scope == 'physical-device') && self.requireVerifiedUnpartitionedTopology)",message="reset-device only supports physical-device scope and requires requireVerifiedUnpartitionedTopology=true"
// +kubebuilder:validation:XValidation:rule="self.action == 'reset-device' || !has(self.requireVerifiedUnpartitionedTopology) || !self.requireVerifiedUnpartitionedTopology",message="requireVerifiedUnpartitionedTopology is only valid for reset-device"
type AcceleratorRuntimeActionPolicy struct {
	Action AcceleratorRuntimeAction `json:"action"`
	// Scopes is bounded (there are only three scope values) so the CEL cost
	// estimator can price the reset-device rule; without MaxItems the API
	// server rejects the whole CRD as exceeding the validation cost budget.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +listType=set
	Scopes []AcceleratorRuntimeScope `json:"scopes"`
	// RequireVerifiedUnpartitionedTopology forces the controller to demand
	// current runtime evidence with topology_safety=verified-unpartitioned.
	// It is required for every physical reset and invalid for other actions.
	RequireVerifiedUnpartitionedTopology bool `json:"requireVerifiedUnpartitionedTopology,omitempty"`
}

// AcceleratorRuntimeProfileSpec is the server-owned accelerator runtime
// contract for a selected NVIDIA node fleet. It intentionally has no
// execution-mode field and cannot make the Kubernetes runtime Enabled.
//
// +kubebuilder:validation:XValidation:rule="has(self.nodeSelector.matchLabels) && self.nodeSelector.matchLabels.size() > 0",message="nodeSelector.matchLabels is required and cannot select every node"
// +kubebuilder:validation:XValidation:rule="!has(self.nodeSelector.matchExpressions) || self.nodeSelector.matchExpressions.size() == 0",message="nodeSelector supports matchLabels only"
// +kubebuilder:validation:XValidation:rule="duration(self.maxReportAge) > duration('0s')",message="maxReportAge must be a positive duration"
type AcceleratorRuntimeProfileSpec struct {
	// +kubebuilder:validation:MinLength=1
	KubeNeuronRef string `json:"kubeNeuronRef"`
	// NodeSelector is deliberately restricted to a nonempty matchLabels map.
	// Match expressions would make overlap and fail-closed profile selection
	// substantially harder to reason about in this first API version.
	NodeSelector metav1.LabelSelector     `json:"nodeSelector"`
	Vendor       AcceleratorRuntimeVendor `json:"vendor"`
	// +kubebuilder:validation:Pattern="^sha256:[a-f0-9]{64}$"
	ProfileDigest string `json:"profileDigest"`
	// DriverVersion pins the exact NVIDIA driver version that this profile was
	// reviewed against. The node agent must observe this value from the live
	// driver; this field is an allow-list input, never a substitute for runtime
	// evidence.
	// +kubebuilder:validation:MinLength=1
	DriverVersion string `json:"driverVersion"`
	// RuntimeVersion pins the exact locally attested DCGM/runtime version
	// reviewed with this profile, normalized as dcgm-X.Y[.Z...]. It must match
	// a bounded node-local probe; this declaration never substitutes for
	// runtime evidence.
	//
	// This is the DCGM *client* the agent runs, which is not necessarily the
	// version of the host engine it queries: `dcgmi --version` answers from the
	// binary alone, and DCGM exposes no way to ask the engine for its build. A
	// newer client serving an older engine is normal with the GPU Operator. The
	// engine is attested separately, by a live connection whose GPU count must
	// match the independent nvidia-smi inventory. Pin this to the client the
	// agent image ships.
	// +kubebuilder:validation:MinLength=1
	RuntimeVersion string `json:"runtimeVersion"`
	// MaxReportAge bounds how long an agent observation can support a
	// capability decision. A missing or non-positive duration is rejected.
	// +kubebuilder:validation:Format=duration
	MaxReportAge metav1.Duration `json:"maxReportAge"`
	// AllowedActions is intentionally optional: an empty list is an explicit
	// observation-only profile. It never authorizes a side effect on its own.
	// Bounded by the closed action vocabulary (eight actions, one entry per
	// action via the map list key).
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=action
	AllowedActions []AcceleratorRuntimeActionPolicy `json:"allowedActions,omitempty"`
}

// AcceleratorRuntimeProfileStatus records only reconciliation of the desired
// profile object. It never claims accelerator health, report freshness, or
// remediation eligibility; those remain runtime observations and controller
// decisions.
type AcceleratorRuntimeProfileStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=arprofile
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Vendor",type="string",JSONPath=".spec.vendor"
// +kubebuilder:printcolumn:name="Profile",type="string",JSONPath=".spec.profileDigest"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AcceleratorRuntimeProfile defines a server-owned runtime contract for an
// accelerator fleet. It narrows what may be considered for remediation; it
// does not enable execution.
type AcceleratorRuntimeProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AcceleratorRuntimeProfileSpec   `json:"spec"`
	Status            AcceleratorRuntimeProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AcceleratorRuntimeProfileList contains AcceleratorRuntimeProfile objects.
type AcceleratorRuntimeProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AcceleratorRuntimeProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&KubeNeuron{}, &KubeNeuronList{},
		&GPURemediationPolicy{}, &GPURemediationPolicyList{},
		&GPUPlaybook{}, &GPUPlaybookList{},
		&GPUSignalMapping{}, &GPUSignalMappingList{},
		&GPUMaintenanceWindow{}, &GPUMaintenanceWindowList{},
		&GPUNodeConfig{}, &GPUNodeConfigList{},
		&AcceleratorRuntimeProfile{}, &AcceleratorRuntimeProfileList{},
	)
}
