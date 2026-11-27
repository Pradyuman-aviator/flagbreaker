// Command flagbreaker runs the Flagbreaker controller.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"text/template"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/changes"
	"github.com/Pradyuman-aviator/flagbreaker/internal/controller"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
	"github.com/Pradyuman-aviator/flagbreaker/internal/requests"
	"github.com/Pradyuman-aviator/flagbreaker/internal/settings"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		flagsmithURL   string
		settingsKey    string
		defaultEnvKey  string
		workloadLink   string
		annotations    bool
		namespaceOptIn bool
		syncPeriod     time.Duration
		webhookAddr    string
		metricsAddr    string
		probeAddr      string
		leaderElection bool
	)
	flag.StringVar(&flagsmithURL, "flagsmith-url", envOr("FLAGSMITH_URL", "http://flagsmith-api.flagsmith:8000"),
		"Flagsmith API base URL, without /api/v1")
	flag.StringVar(&settingsKey, "settings-environment-key", os.Getenv("FLAGSMITH_SETTINGS_ENVIRONMENT_KEY"),
		"client-side key of the Flagsmith environment holding flagbreaker_enabled, flagbreaker_mode and flagbreaker_policy; "+
			"empty means each FlagGuard's spec.mode decides, and watch when it sets none")
	flag.StringVar(&defaultEnvKey, "default-environment-key", os.Getenv("FLAGSMITH_DEFAULT_ENVIRONMENT_KEY"),
		"Flagsmith environment key for workloads annotated with flagbreaker.dev/flags but not flagbreaker.dev/environment-key")
	flag.StringVar(&workloadLink, "workload-link-template", "",
		"Go template for a link to the workload in every Feature Health event, for example "+
			"https://headlamp.example.com/c/main/deployments/{{.Namespace}}/{{.Name}}; it gets .Namespace, .Kind, .Name and .Guard")
	flag.BoolVar(&annotations, "annotations", true, "create FlagGuards from flagbreaker.dev/flags annotations on workloads")
	flag.BoolVar(&namespaceOptIn, "require-namespace-opt-in", true,
		"act only on Flagsmith environments that a guard's namespace lists in its flagbreaker.dev/allowed-environments annotation")
	flag.DurationVar(&syncPeriod, "sync-period", 5*time.Second, "how often each FlagGuard is checked")
	flag.StringVar(&webhookAddr, "webhook-bind-address", ":8082", "address for Flagsmith FLAG_UPDATED webhooks; empty disables them")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address for Prometheus metrics; 0 disables them")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address for health probes")
	flag.BoolVar(&leaderElection, "leader-elect", true, "elect a leader, so only one replica ever changes flags")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	// Secrets come from the environment, never from flags, so they don't
	// show up in `ps` or the pod spec.
	apiKey := os.Getenv("FLAGSMITH_API_KEY")
	healthPath := os.Getenv("FLAGSMITH_HEALTH_WEBHOOK_PATH")
	webhookSecret := os.Getenv("FLAGSMITH_WEBHOOK_SECRET")
	if apiKey == "" || healthPath == "" {
		log.Error(errors.New("missing configuration"), "FLAGSMITH_API_KEY and FLAGSMITH_HEALTH_WEBHOOK_PATH must be set")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	must(v1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Pods, ReplicaSets and workloads are watched cluster-wide; managed
		// fields are most of their size and never read.
		Cache:                  cache.Options{DefaultTransform: cache.TransformStripManagedFields()},
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "flagbreaker.flagbreaker.dev",
		// Hand the lease over at shutdown, so a restart doesn't wait for it to
		// expire. Safe because the process exits right after.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		log.Error(err, "creating the manager")
		os.Exit(1)
	}

	var linkTemplate *template.Template
	if workloadLink != "" {
		linkTemplate, err = template.New("link").Option("missingkey=error").Parse(workloadLink)
		if err != nil {
			log.Error(err, "parsing --workload-link-template")
			os.Exit(1)
		}
	}

	store := changes.NewStore()
	reconciler := &controller.FlagGuardReconciler{
		Client:       mgr.GetClient(),
		APIReader:    mgr.GetAPIReader(),
		Recorder:     mgr.GetEventRecorder("flagbreaker"),
		Flagsmith:    &flagsmith.Client{BaseURL: flagsmithURL, APIKey: apiKey, HealthWebhookPath: healthPath},
		Settings:     settings.NewReader(flagsmithURL, settingsKey, 5*time.Second),
		Changes:      store,
		SyncPeriod:   syncPeriod,
		WorkloadLink: linkTemplate,
		// Reads the pods of guards with spec.requests: 2s per pod, no
		// redirects, no proxy.
		Requests: requests.NewTracker(nil),

		RequireNamespaceOptIn: namespaceOptIn,
	}
	if !namespaceOptIn {
		log.Info("--require-namespace-opt-in=false: a FlagGuard in any namespace may act on any Flagsmith environment the API key reaches")
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the controller")
		os.Exit(1)
	}
	if annotations {
		for _, kind := range []string{controller.KindDeployment, controller.KindStatefulSet, controller.KindDaemonSet} {
			ar := &controller.AnnotationReconciler{
				Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("flagbreaker"),
				Kind: kind, DefaultEnvironmentKey: defaultEnvKey,
			}
			if err := ar.SetupWithManager(mgr); err != nil {
				log.Error(err, "setting up the annotation controller", "kind", kind)
				os.Exit(1)
			}
		}
	}

	if webhookAddr != "" {
		if webhookSecret == "" {
			log.Info("FLAGSMITH_WEBHOOK_SECRET is not set, so webhooks are off and changes are found by polling only")
		} else {
			must(mgr.Add(webhookServer(webhookAddr, webhookSecret, store, reconciler)))
		}
	}

	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	log.Info("starting", "version", version, "flagsmith", flagsmithURL, "syncPeriod", syncPeriod, "annotations", annotations)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running the manager")
		os.Exit(1)
	}
}

// webhookServer receives FLAG_UPDATED webhooks. It runs on every replica
// from the start, not only once one becomes the leader, so the Service
// always has an endpoint and a webhook sent just after a restart still dates
// the change. The chart runs one replica, replaced with Recreate.
func webhookServer(addr, secret string, store *changes.Store, r *controller.FlagGuardReconciler) manager.Runnable {
	return anyReplica(func(ctx context.Context) error {
		mux := http.NewServeMux()
		mux.Handle("/webhooks/flagsmith", changes.Handler(secret, store, func(environment, feature int64) {
			r.FlagChanged(ctx, environment, feature)
		}))
		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
}

// anyReplica is a Runnable that doesn't wait for leader election.
type anyReplica func(ctx context.Context) error

func (f anyReplica) Start(ctx context.Context) error { return f(ctx) }
func (anyReplica) NeedLeaderElection() bool          { return false }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func must(err error) {
	if err != nil {
		ctrl.Log.Error(err, "setup failed")
		os.Exit(1)
	}
}
