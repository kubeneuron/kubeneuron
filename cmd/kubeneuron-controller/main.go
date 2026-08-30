// Command kubeneuron-controller is the KubeNeuron control plane: it ingests
// alerts and agent events, drives the incident state machine, and executes
// remediation playbooks through the safety gates.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/kubeneuron/kubeneuron/internal/actuator"
	"github.com/kubeneuron/kubeneuron/internal/actuator/agentrpc"
	"github.com/kubeneuron/kubeneuron/internal/agentauth"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/notify/pagerduty"
	"github.com/kubeneuron/kubeneuron/internal/notify/slack"
	"github.com/kubeneuron/kubeneuron/internal/notify/webhook"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/platform/baremetal"
	"github.com/kubeneuron/kubeneuron/internal/platform/kubernetes"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/postgres"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlcore"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/kubeneuron/kubeneuron/web"
)

var version = "dev"

type agentServerConfig struct {
	ListenAddr       string
	TLSCertFile      string
	TLSKeyFile       string
	ClientCAFile     string
	TokenAudience    string
	Namespace        string
	ServiceAccount   string
	DaemonSet        string
	InstallationName string
	InstallationUID  string
}

const (
	serverReadTimeout  = 15 * time.Second
	serverWriteTimeout = 30 * time.Second
	serverIdleTimeout  = 60 * time.Second
	serverMaxHeaders   = 1 << 20
)

func main() {
	var (
		listenAddr             = flag.String("listen", ":8080", "webhook, health, and public API listen address")
		publicTLSCert          = flag.String("public-tls-cert", "", "serving certificate for the public listener; empty serves plain HTTP")
		publicTLSKey           = flag.String("public-tls-key", "", "serving private key for the public listener")
		agentListen            = flag.String("agent-listen", ":8443", "mTLS agent API listen address")
		agentTLSCert           = flag.String("agent-tls-cert", "", "controller serving certificate for the agent API")
		agentTLSKey            = flag.String("agent-tls-key", "", "controller serving private key for the agent API")
		agentClientCA          = flag.String("agent-client-ca", "", "CA bundle used to verify agent client certificates")
		agentAudience          = flag.String("agent-token-audience", "kubeneuron-controller", "required projected ServiceAccount token audience")
		agentNamespace         = flag.String("agent-token-namespace", "", "namespace containing the managed agent Pods")
		agentServiceAccount    = flag.String("agent-token-service-account", "", "ServiceAccount required for managed agent Pods")
		agentDaemonSet         = flag.String("agent-daemonset", "", "comma-separated DaemonSet names allowed to own authenticated agent Pods")
		installationName       = flag.String("installation-name", "", "KubeNeuron installation name used for workload identity")
		installationUID        = flag.String("installation-uid", "", "KubeNeuron installation UID used for certificate identity")
		slackWebhookFile       = flag.String("slack-webhook-file", "", "file with a Slack incoming-webhook URL; empty disables Slack notifications")
		notifyWebhookURLFile   = flag.String("notify-webhook-url-file", "", "file with a generic notification webhook URL; empty disables the generic webhook")
		notifyWebhookTokenFile = flag.String("notify-webhook-token-file", "", "file with the bearer token for the generic notification webhook (optional)")
		pagerdutyKeyFile       = flag.String("pagerduty-routing-key-file", "", "file with a PagerDuty Events v2 routing key; empty disables PagerDuty")
		startPaused            = flag.Bool("start-paused", false, "start with automated remediation paused; resume via the operator API")
		apiTokenFile           = flag.String("api-token-file", "", "file with the operator API bearer token; empty disables the operator API")
		authUsersDir           = flag.String("auth-users-dir", "", "directory of bcrypt user files (filename=username, content=bcrypt hash) enabling password sign-in; empty disables it")
		oidcIssuer             = flag.String("oidc-issuer", "", "OIDC issuer URL enabling single sign-on; empty disables it")
		oidcClientID           = flag.String("oidc-client-id", "", "OIDC client ID")
		oidcClientSecretF      = flag.String("oidc-client-secret-file", "", "file with the OIDC client secret (never pass secrets via argv)")
		oidcRedirectURL        = flag.String("oidc-redirect-url", "", "externally reachable OIDC callback URL (https://<panel>/api/v1/auth/oidc/callback)")
		oidcAllowedDomains     = flag.String("oidc-allowed-email-domains", "", "comma-separated email domains allowed to sign in via OIDC; empty allows any authenticated principal")
		apiAuthnKubernetes     = flag.Bool("api-authn-kubernetes", false, "additionally accept per-caller Kubernetes credentials on the operator API (TokenReview identity + RBAC on the KubeNeuron object); the static token stays as break-glass")
		webhookTokenFile       = flag.String("webhook-token-file", "", "file with the Alertmanager webhook bearer token; empty leaves the webhook unauthenticated (development only)")
		configPath             = flag.String("config", "configs/policies.yaml", "policies/safety config file")
		windowsPath            = flag.String("windows", "", "maintenance windows YAML (missing file means no windows)")
		mappingsPath           = flag.String("signal-mappings", "", "signal-override YAML (missing file means built-in catalog only)")
		nodeConfigsPath        = flag.String("node-configs", "", "per-node configuration YAML (missing file means no per-node overrides)")
		playbooksDir           = flag.String("playbooks", "configs/playbooks", "playbooks directory")
		storeKind              = flag.String("store", "sqlite", "workflow store backend: sqlite | postgres")
		postgresDSNFile        = flag.String("postgres-dsn-file", "", "file containing the PostgreSQL DSN (never pass a DSN via argv)")
		dbPath                 = flag.String("db", "kubeneuron.db", "SQLite database path")
		storeRetention         = flag.Duration("store-retention", 90*24*time.Hour, "retention for archived events, acknowledged outbox entries, and completed queue actions; 0 keeps them forever")
		auditRetention         = flag.Duration("store-audit-retention", 0, "opt-in retention for terminal incidents with their audit/approval history; 0 (default) keeps the audit trail forever")
		leaderElect            = flag.Bool("leader-elect", false, "enable Lease-based leader election; standbys stay unready so Services route to the single leader")
		leaderElectionNS       = flag.String("leader-election-namespace", os.Getenv("POD_NAMESPACE"), "namespace for the leader-election Lease (default: POD_NAMESPACE)")
		leaderElectionName     = flag.String("leader-election-name", "", "Lease name (default: <installation-name>-controller)")
		platformName           = flag.String("platform", "kubernetes", "platform: kubernetes | baremetal")
		cloudProvider          = flag.String("cloud-provider", "", "cloud provider for node recycle/replace: empty (disabled) | aws. AWS needs an IRSA role with ec2:Stop/Start/Terminate/DescribeInstances.")
		cloudRegion            = flag.String("cloud-region", os.Getenv("AWS_REGION"), "cloud region for the provider (default: AWS_REGION)")
		kubeconfig             = flag.String("kubeconfig", "", "kubeconfig path (out-of-cluster)")
		inventoryPath          = flag.String("inventory", "", "bare-metal inventory YAML")
		showVersion            = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("kubeneuron-controller", version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	agentServer := agentServerConfig{
		ListenAddr:       *agentListen,
		TLSCertFile:      *agentTLSCert,
		TLSKeyFile:       *agentTLSKey,
		ClientCAFile:     *agentClientCA,
		TokenAudience:    *agentAudience,
		Namespace:        *agentNamespace,
		ServiceAccount:   *agentServiceAccount,
		DaemonSet:        *agentDaemonSet,
		InstallationName: *installationName,
		InstallationUID:  *installationUID,
	}
	if err := run(log, *listenAddr, agentServer, runtimeConfigPaths{
		policies:    *configPath,
		windows:     *windowsPath,
		mappings:    *mappingsPath,
		nodeConfigs: *nodeConfigsPath,
		playbooks:   *playbooksDir,
	}, *dbPath, *platformName, *kubeconfig, *inventoryPath, *apiTokenFile, *apiAuthnKubernetes, humanAuth{
		usersDir:           *authUsersDir,
		oidcIssuer:         *oidcIssuer,
		oidcClientID:       *oidcClientID,
		oidcClientSecretF:  *oidcClientSecretF,
		oidcRedirectURL:    *oidcRedirectURL,
		oidcAllowedDomains: *oidcAllowedDomains,
	}, *webhookTokenFile, notifyFiles{
		slackWebhook:        *slackWebhookFile,
		webhookURL:          *notifyWebhookURLFile,
		webhookToken:        *notifyWebhookTokenFile,
		pagerdutyRoutingKey: *pagerdutyKeyFile,
	}, *startPaused, *storeRetention, *auditRetention, *publicTLSCert, *publicTLSKey, *storeKind, *postgresDSNFile, electionConfig{
		enabled:   *leaderElect,
		namespace: *leaderElectionNS,
		name:      *leaderElectionName,
	}, *cloudProvider, *cloudRegion); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runtimeConfigPaths groups the file-based runtime configuration inputs.
type runtimeConfigPaths struct {
	policies    string
	windows     string
	mappings    string
	nodeConfigs string
	playbooks   string
}

// runtimeStore is the store surface the controller needs; both the SQLite
// and PostgreSQL backends satisfy it via the shared sqlcore engine.
type runtimeStore interface {
	store.Store
	store.EventOutbox
	store.AcceleratorReportStore
	SaveSafetyState(kind string, payload []byte) error
	LoadSafetyState(kind string) ([]byte, error)
	Prune(ctx context.Context, dataRetention, auditRetention time.Duration) (sqlcore.PruneStats, error)
	CountPendingActions(ctx context.Context) (int, error)
	CountIncidentsByState(ctx context.Context) (map[types.IncidentState]int, error)
}

// humanAuth groups the browser sign-in configuration (password users
// directory and the optional OIDC provider).
type humanAuth struct {
	usersDir           string
	oidcIssuer         string
	oidcClientID       string
	oidcClientSecretF  string
	oidcRedirectURL    string
	oidcAllowedDomains string
}

// notifyFiles groups the file-based notification credentials. Every value
// is a path to a mounted Secret, never the credential itself.
type notifyFiles struct {
	slackWebhook        string
	webhookURL          string
	webhookToken        string
	pagerdutyRoutingKey string
}

// electionConfig carries the optional leader-election settings.
type electionConfig struct {
	enabled   bool
	namespace string
	name      string
}

func run(log *slog.Logger, listenAddr string, agentServer agentServerConfig, paths runtimeConfigPaths, dbPath, platformName, kubeconfig, inventoryPath, apiTokenFile string, apiAuthnKubernetes bool, auth humanAuth, webhookTokenFile string, notifyCfg notifyFiles, startPaused bool, storeRetention, auditRetention time.Duration, publicTLSCert, publicTLSKey, storeKind, postgresDSNFile string, election electionConfig, cloudProvider, cloudRegion string) error {
	if (publicTLSCert == "") != (publicTLSKey == "") {
		return fmt.Errorf("public TLS requires both -public-tls-cert and -public-tls-key")
	}
	cfg, err := config.Load(paths.policies)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	books, err := playbook.LoadDir(paths.playbooks)
	if err != nil {
		return fmt.Errorf("loading playbooks: %w", err)
	}
	policies := make([]playbook.Policy, 0, len(cfg.Policies))
	for _, p := range cfg.Policies {
		policies = append(policies, playbook.Policy{Class: p.Match.Class, Vendor: p.Match.Vendor, Playbook: p.Playbook, Params: p.Params})
	}
	engine, err := playbook.NewEngine(books, policies)
	if err != nil {
		return fmt.Errorf("building playbook engine: %w", err)
	}

	var st runtimeStore
	var sqliteBackup *sqlite.Store
	switch storeKind {
	case "sqlite":
		s, err := sqlite.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening store: %w", err)
		}
		st, sqliteBackup = s, s
	case "postgres":
		if postgresDSNFile == "" {
			return fmt.Errorf("-postgres-dsn-file is required for the postgres store")
		}
		dsn, err := readTokenFile(postgresDSNFile)
		if err != nil {
			return fmt.Errorf("postgres DSN: %w", err)
		}
		s, err := postgres.Open(context.Background(), dsn)
		if err != nil {
			return fmt.Errorf("opening postgres store: %w", err)
		}
		st = s
	default:
		return fmt.Errorf("unsupported store %q: use sqlite or postgres", storeKind)
	}
	defer func() { _ = st.Close() }()

	var plat platform.Platform
	var kubePlatform *kubernetes.Platform
	switch platformName {
	case "kubernetes":
		kubePlatform, err = kubernetes.New(kubeconfig)
		if err != nil {
			return fmt.Errorf("kubernetes platform: %w", err)
		}
		plat = kubePlatform
		if cloudProvider != "" {
			recycler, err := newInstanceRecycler(context.Background(), cloudProvider, cloudRegion)
			if err != nil {
				return fmt.Errorf("cloud provider %q: %w", cloudProvider, err)
			}
			kubePlatform.SetInstanceRecycler(recycler)
			log.Info("cloud node recycling enabled", "provider", cloudProvider)
		}
	case "baremetal":
		plat, err = baremetal.New(inventoryPath, baremetal.Hooks{})
		if err != nil {
			return fmt.Errorf("baremetal platform: %w", err)
		}
	default:
		return fmt.Errorf("unknown platform %q", platformName)
	}

	gate := safety.NewGate(safety.Limits{
		MaxConcurrentRemediations: cfg.Safety.MaxConcurrentRemediations,
		MaxConcurrentReboots:      cfg.Safety.MaxConcurrentReboots,
		DryRun:                    cfg.Safety.DryRun,
	})
	flap := safety.NewFlapDetector(cfg.Safety.Flap.Count, cfg.Safety.Flap.Window.Std())
	// Cooldowns and flap history survive restarts: a crash-looping
	// controller must not shed exactly the protections a signal storm needs.
	if err := gate.RestoreAndPersist(st, log); err != nil {
		return fmt.Errorf("restore gate cooldowns: %w", err)
	}
	if err := flap.RestoreAndPersist(st, log); err != nil {
		return fmt.Errorf("restore flap history: %w", err)
	}
	if startPaused {
		gate.Pause()
		log.Warn("starting with automated remediation PAUSED; resume via the operator API or kubeneuronctl resume")
	}

	// Wrapped unconditionally, deciding per call. Wrapping only when the gate
	// happened to say dry-run at startup froze the answer for the life of the
	// process, and configuration reloads in place — so enabling a running
	// installation left every agent step permanently simulated while the
	// controller-side platform steps went live. The wrapper is also the
	// janitor's only dry-run guard (restoreAcceleratorHost calls the actuator
	// outside executeStep), so it must stay in the chain either way.
	var act actuator.Actuator = &actuator.Chain{Actuators: []actuator.Actuator{agentrpc.New(st, 0)}}
	act = &actuator.DryRun{Inner: act, When: gate.DryRun}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if kubePlatform != nil {
		// Node inventory sits on the destructive-admission path; serve it from
		// a watch-maintained cache instead of a per-check apiserver List.
		kubePlatform.StartNodeCache(ctx)
	}

	// leading is true when this replica may write: always without leader
	// election, only while holding the Lease with it.
	var leading atomic.Bool
	leading.Store(!election.enabled)

	// Hourly retention pass: the store must not grow without bound. Audit
	// history is only pruned when the operator opted in via
	// -store-audit-retention.
	if storeRetention > 0 || auditRetention > 0 {
		go func() {
			tick := time.NewTicker(time.Hour)
			defer tick.Stop()
			for {
				if !leading.Load() {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
					continue
				}
				pruneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				stats, err := st.Prune(pruneCtx, storeRetention, auditRetention)
				cancel()
				if err != nil {
					log.Warn("store retention pass failed", "err", err)
				} else if stats != (sqlcore.PruneStats{}) {
					log.Info("store retention pass",
						"events", stats.Events, "outbox", stats.Outbox, "actions", stats.Actions,
						"incidents", stats.Incidents, "audit", stats.Audit, "approvals", stats.Approvals)
				}
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
	}

	notifier := notify.Multi{&notify.Log{Logger: log}}
	// Each external channel gets its own async worker with retry/backoff and
	// dead-lettering: one channel's outage never stalls another, and neither
	// can ever stall ingestion or the reconcile walk.
	addAsync := func(n notify.Notifier) {
		async := notify.NewAsync(n, log)
		async.Start(ctx)
		notifier = append(notifier, async)
	}
	if notifyCfg.slackWebhook != "" {
		webhookURL, err := readTokenFile(notifyCfg.slackWebhook)
		if err != nil {
			return fmt.Errorf("slack webhook: %w", err)
		}
		addAsync(slack.New(webhookURL))
	}
	if notifyCfg.webhookURL != "" {
		url, err := readTokenFile(notifyCfg.webhookURL)
		if err != nil {
			return fmt.Errorf("notification webhook URL: %w", err)
		}
		token := ""
		if notifyCfg.webhookToken != "" {
			if token, err = readTokenFile(notifyCfg.webhookToken); err != nil {
				return fmt.Errorf("notification webhook token: %w", err)
			}
		}
		addAsync(webhook.New(url, token))
	}
	if notifyCfg.pagerdutyRoutingKey != "" {
		key, err := readTokenFile(notifyCfg.pagerdutyRoutingKey)
		if err != nil {
			return fmt.Errorf("pagerduty routing key: %w", err)
		}
		addAsync(pagerduty.New(key))
	}
	ctrl := controller.New(st, st, engine, gate, flap, plat, act, notifier, log)
	api := httpapi.New(ctrl)
	// Install the full runtime configuration in place, and keep it current by
	// watching the mounted files rather than by rolling the Deployment — see
	// applyRuntimeConfig for why a rollout cannot work under leader election.
	if err := applyRuntimeConfig(ctx, ctrl, api, paths, log); err != nil {
		return err
	}
	go watchRuntimeConfig(ctx, ctrl, api, paths, log)
	api.SetMetricsHandler(metrics.Handler())
	// When the public listener serves plain HTTP it is expected to sit behind a
	// TLS-terminating load balancer, so honor X-Forwarded-Proto to mark panel
	// cookies Secure. When it terminates TLS itself, r.TLS already answers that.
	api.TrustProxyHeaders(publicTLSCert == "")
	if sqliteBackup != nil && dbPath != ":memory:" {
		// The snapshot endpoint is SQLite-only; PostgreSQL backups use
		// pg_dump/PITR (see operations).
		api.SetBackupStore(sqliteBackup, filepath.Dir(dbPath))
	}
	if dist, err := web.Dist(); err == nil {
		api.SetUI(http.FS(dist))
	} else {
		log.Warn("embedded web panel unavailable", "err", err)
	}
	// Store-derived gauges are collected by the LEADER only, under a deadline.
	//
	// Leader-only because the value describes the fleet, not this process: two
	// replicas reading the same rows publish the same number twice, so
	// `sum(kubeneuron_degraded_gpus)` reports double the degraded capacity on a
	// PostgreSQL HA pair. A standby publishing no series is the shape every
	// natural query already assumes. A standby also has nothing useful to say —
	// it is not walking incidents.
	//
	// Under a deadline because this runs on the SCRAPE path: DegradedGPUs
	// issues two ListIncidents plus a node lookup per node-scoped incident, and
	// with context.Background() a slow store held the scrape open with no
	// bound. Prometheus gives up on its own timeout and records the target as
	// down, which reads as "the controller is gone" rather than "the store is
	// slow" — and it does so for every metric on the endpoint, not just this
	// one.
	const scrapeCollectBudget = 5 * time.Second
	metrics.RegisterDegradedGPUs(func() map[metrics.DegradedKey]int {
		if !leading.Load() {
			return nil
		}
		collectCtx, cancel := context.WithTimeout(ctx, scrapeCollectBudget)
		defer cancel()
		degraded, err := ctrl.DegradedGPUs(collectCtx)
		if err != nil {
			log.Warn("degraded-GPU metric collection failed", "err", err)
			return nil
		}
		return degraded
	})
	// Same shape, same two reasons: fleet-wide counts read from the shared
	// store, so a standby would double them, and both queries ran unbounded on
	// the scrape path.
	metrics.RegisterIncidentStates(func() map[types.IncidentState]int {
		if !leading.Load() {
			// Zero, not the last value this process happened to see: a plain
			// gauge keeps whatever was last written, so a replica that loses
			// leadership would otherwise publish a frozen queue depth. The
			// state gauge needs no equivalent — a nil map makes its collector
			// emit nothing at all.
			metrics.ActionsPending.Set(0)
			return nil
		}
		collectCtx, cancel := context.WithTimeout(ctx, scrapeCollectBudget)
		defer cancel()
		counts, err := st.CountIncidentsByState(collectCtx)
		if err != nil {
			log.Warn("incident state metric collection failed", "err", err)
			return nil
		}
		if pending, err := st.CountPendingActions(collectCtx); err == nil {
			metrics.ActionsPending.Set(float64(pending))
		}
		return counts
	})
	if apiTokenFile != "" {
		token, err := readTokenFile(apiTokenFile)
		if err != nil {
			return fmt.Errorf("operator API token: %w", err)
		}
		// The recovery report reaches the API through an optional interface,
		// so a signature drift would degrade to a permanent 503 instead of a
		// build failure. Assert the wiring here, where both packages are in
		// scope.
		var _ httpapi.RecoveryReportBackend = ctrl
		api.EnableOperatorAPI(ctrl, token)
		api.SetOperatorTokenProvider(newCachedTokenFile(apiTokenFile, token, log).get)
		if auth.usersDir != "" {
			api.SetBasicUsersDir(auth.usersDir)
			log.Info("password sign-in enabled", "users_dir", auth.usersDir)
		}
		if auth.oidcIssuer != "" {
			if auth.oidcClientID == "" || auth.oidcClientSecretF == "" || auth.oidcRedirectURL == "" {
				return fmt.Errorf("-oidc-issuer requires -oidc-client-id, -oidc-client-secret-file, and -oidc-redirect-url")
			}
			clientSecret, err := readTokenFile(auth.oidcClientSecretF)
			if err != nil {
				return fmt.Errorf("oidc client secret: %w", err)
			}
			var domains []string
			for _, d := range strings.Split(auth.oidcAllowedDomains, ",") {
				if d = strings.TrimSpace(d); d != "" {
					domains = append(domains, d)
				}
			}
			if err := api.EnableOIDC(ctx, httpapi.OIDCConfig{
				IssuerURL: auth.oidcIssuer, ClientID: auth.oidcClientID,
				ClientSecret: clientSecret, RedirectURL: auth.oidcRedirectURL,
				AllowedEmailDomains: domains,
			}); err != nil {
				return err
			}
			log.Info("OIDC single sign-on enabled", "issuer", auth.oidcIssuer)
		}
		if apiAuthnKubernetes {
			if kubePlatform == nil {
				return fmt.Errorf("-api-authn-kubernetes requires the kubernetes platform")
			}
			if agentServer.InstallationName == "" {
				return fmt.Errorf("-api-authn-kubernetes requires -installation-name")
			}
			operatorAuth, err := agentauth.NewOperator(kubePlatform.Client(), agentServer.InstallationName)
			if err != nil {
				return fmt.Errorf("configure operator identity: %w", err)
			}
			api.SetOperatorAuthenticator(operatorAuth)
			log.Info("operator API accepts Kubernetes credentials",
				"authorization", "get/update kubeneurons/"+agentServer.InstallationName)
		}
	} else {
		log.Warn("operator API disabled: no -api-token-file configured")
	}
	if webhookTokenFile != "" {
		token, err := readTokenFile(webhookTokenFile)
		if err != nil {
			return fmt.Errorf("webhook token: %w", err)
		}
		api.SetWebhookToken(token)
		api.SetWebhookTokenProvider(newCachedTokenFile(webhookTokenFile, token, log).get)
	} else {
		// Fail-closed by default: without a token the webhook rejects every
		// caller, because a firing alert can drive cordon/drain. Opt into the
		// unauthenticated development mode explicitly so the security property
		// lives in the code path, not in whether a flag happened to be set.
		api.AllowInsecureWebhook()
		log.Warn("Alertmanager webhook is unauthenticated: no -webhook-token-file configured (development only)")
	}

	if kubePlatform == nil {
		return fmt.Errorf("authenticated agent API currently requires the kubernetes platform")
	}
	authenticator, err := agentauth.New(kubePlatform.Client(), agentauth.Config{
		Audience:         agentServer.TokenAudience,
		Namespace:        agentServer.Namespace,
		ServiceAccount:   agentServer.ServiceAccount,
		AgentDaemonSets:  splitNonEmpty(agentServer.DaemonSet, ","),
		InstallationName: agentServer.InstallationName,
		InstallationUID:  agentServer.InstallationUID,
	})
	if err != nil {
		return fmt.Errorf("configure agent identity: %w", err)
	}
	agentTLSConfig, err := loadAgentTLSConfig(agentServer.ClientCAFile)
	if err != nil {
		return err
	}
	if agentServer.TLSCertFile == "" || agentServer.TLSKeyFile == "" {
		return fmt.Errorf("agent TLS certificate and key files are required")
	}
	if serverCertPEM, err := os.ReadFile(agentServer.TLSCertFile); err == nil {
		metrics.RecordCertBundleExpiry("controller-server-leaf", serverCertPEM)
	}
	if listenAddr == agentServer.ListenAddr {
		return fmt.Errorf("public and agent listen addresses must differ")
	}

	publicServer := &http.Server{
		Addr:              listenAddr,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaxHeaders,
	}
	if publicTLSCert != "" {
		publicServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		if certPEM, err := os.ReadFile(publicTLSCert); err == nil {
			metrics.RecordCertBundleExpiry("public-server-leaf", certPEM)
		}
	}
	secureAgentServer := &http.Server{
		Addr:              agentServer.ListenAddr,
		Handler:           api.AgentRoutes(authenticator),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaxHeaders,
		TLSConfig:         agentTLSConfig,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = publicServer.Shutdown(shutdownCtx)
		_ = secureAgentServer.Shutdown(shutdownCtx)
	}()

	log.Info("kubeneuron-controller starting",
		"version", version, "listen", listenAddr, "agent_listen", agentServer.ListenAddr,
		"platform", plat.Name(), "dry_run", gate.DryRun(),
		"playbooks", len(books), "policies", len(policies))

	serverErr := make(chan error, 2)
	go func() {
		if publicTLSCert != "" {
			serverErr <- serve("public HTTPS", publicServer.ListenAndServeTLS(publicTLSCert, publicTLSKey))
		} else {
			serverErr <- serve("public HTTP", publicServer.ListenAndServe())
		}
		stop()
	}()
	go func() {
		serverErr <- serve("agent mTLS", secureAgentServer.ListenAndServeTLS(agentServer.TLSCertFile, agentServer.TLSKeyFile))
		stop()
	}()

	api.SetReadyCheck(leading.Load)

	if election.enabled {
		if kubePlatform == nil {
			return fmt.Errorf("leader election requires the kubernetes platform")
		}
		if election.namespace == "" {
			return fmt.Errorf("leader election requires -leader-election-namespace or POD_NAMESPACE")
		}
		leaseName := election.name
		if leaseName == "" {
			if agentServer.InstallationName == "" {
				return fmt.Errorf("leader election requires -leader-election-name or -installation-name")
			}
			leaseName = agentServer.InstallationName + "-controller"
		}
		identity := os.Getenv("POD_NAME")
		if identity == "" {
			identity, _ = os.Hostname()
		}
		lock := &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: election.namespace},
			Client:     kubePlatform.Client().CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
		}
		log.Info("waiting for leadership", "lease", leaseName, "identity", identity)
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leadCtx context.Context) {
					// Reload persisted cooldowns/flap history: the previous
					// leader may have written since this process started.
					if err := gate.RestoreAndPersist(st, log); err != nil {
						log.Error("reload gate state on leadership", "err", err)
						stop()
						return
					}
					if err := flap.RestoreAndPersist(st, log); err != nil {
						log.Error("reload flap state on leadership", "err", err)
						stop()
						return
					}
					leading.Store(true)
					log.Info("leadership acquired; starting the remediation walk")
					if err := ctrl.Run(leadCtx); err != nil {
						// Release leadership, like the two restore failures
						// above. client-go runs this callback in a goroutine
						// and keeps renewing the Lease regardless, so simply
						// logging would leave this replica holding the lock,
						// reporting Ready, and doing no work — with no standby
						// able to take over. Run returns nil on ctx.Done()
						// today, so this is unreachable; the invariant is one
						// edit away from being false and the cost is a line.
						log.Error("controller run; releasing leadership", "err", err)
						stop()
					}
				},
				OnStoppedLeading: func() {
					// Fail fast: a fresh process re-enters the election with
					// clean in-memory state instead of risking a stale writer.
					log.Error("leadership lost; exiting for a clean restart")
					leading.Store(false)
					stop()
				},
			},
		})
	} else {
		if err := ctrl.Run(ctx); err != nil {
			return err
		}
	}
	select {
	case err := <-serverErr:
		return err
	default:
		return nil
	}
}

// cachedTokenFile re-reads a mounted token Secret with a short TTL so
// rotation takes effect without a restart; the last good value survives a
// transient read error (kubelet atomically swaps projected volumes).
type cachedTokenFile struct {
	path string
	ttl  time.Duration

	mu     sync.Mutex
	value  string
	readAt time.Time
	log    *slog.Logger
}

func newCachedTokenFile(path, initial string, log *slog.Logger) *cachedTokenFile {
	return &cachedTokenFile{path: path, ttl: 10 * time.Second, value: initial, readAt: time.Now(), log: log}
}

func (c *cachedTokenFile) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.readAt) < c.ttl {
		return c.value
	}
	c.readAt = time.Now()
	token, err := readTokenFile(c.path)
	if err != nil {
		c.log.Warn("re-reading token file failed; keeping the previous token", "path", c.path, "err", err)
		return c.value
	}
	if token != c.value {
		c.log.Info("token rotated", "path", c.path)
		c.value = token
	}
	return c.value
}

func readTokenFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%s: token file is empty", path)
	}
	return token, nil
}

func loadAgentTLSConfig(clientCAFile string) (*tls.Config, error) {
	if clientCAFile == "" {
		return nil, fmt.Errorf("agent client CA file is required")
	}
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read agent client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("agent client CA file contains no certificates")
	}
	metrics.RecordCertBundleExpiry("agent-client-ca", caPEM)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
	}, nil
}

func serve(name string, err error) error {
	if err == nil || err == http.ErrServerClosed {
		return nil
	}
	return fmt.Errorf("%s server: %w", name, err)
}

// splitNonEmpty splits s on sep, trims whitespace, and drops empty entries.
func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
