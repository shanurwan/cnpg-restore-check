// Command cnpg-restore-check runs the RestoreCheck controller.
package main

import (
	"flag"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
	"github.com/shanurwan/cnpg-restore-check/internal/controller"
)

func main() {
	var (
		metricsAddr       string
		probeAddr         string
		leaderElect       bool
		maxConcurrentRuns int
		workers           int
		watchNamespaces   string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the Prometheus metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probes bind to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election, for running more than one replica.")
	flag.IntVar(&maxConcurrentRuns, "max-concurrent-runs", 1, "Maximum runs in progress across all RestoreChecks (0 = unlimited).")
	flag.IntVar(&workers, "workers", 4, "Number of RestoreChecks reconciled in parallel.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "", "Comma-separated namespaces to watch. Empty watches all namespaces.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	cacheOpts := cache.Options{}
	if watchNamespaces != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range strings.Split(watchNamespaces, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				cacheOpts.DefaultNamespaces[ns] = cache.Config{}
			}
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "cnpg-restore-check.restorecheck.shanurwan.github.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	r := &controller.Reconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorderFor("cnpg-restore-check"),
		MaxConcurrentRuns: maxConcurrentRuns,
	}
	if err := r.SetupWithManager(mgr, workers); err != nil {
		setupLog.Error(err, "unable to set up controller")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
