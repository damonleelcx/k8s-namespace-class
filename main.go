package main

import (
	"flag"
	"os"
	"time"

	"github.com/joho/godotenv"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"namespace-class/internal/aiwatcher"
	"namespace-class/internal/controller"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var aiPollInterval time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.DurationVar(&aiPollInterval, "ai-poll-interval", 30*time.Second, "Polling interval for AI diff watcher.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// Best effort: load local .env for dev runs.
	if err := godotenv.Load(); err != nil {
		setupLog.Info("no .env file loaded; using process environment only")
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "namespaceclass.akuity.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.NamespaceClassReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create NamespaceClass controller")
		os.Exit(1)
	}
	if err := (&controller.NamespaceClassChangeRequestReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create NamespaceClassChangeRequest controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if os.Getenv("OPENAI_API_KEY") != "" {
		if err := mgr.Add(aiwatcher.New(cfg, aiPollInterval)); err != nil {
			setupLog.Error(err, "unable to add AI watcher")
			os.Exit(1)
		}
		setupLog.Info("AI watcher enabled")
	} else {
		setupLog.Info("OPENAI_API_KEY not set; AI watcher disabled")
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
