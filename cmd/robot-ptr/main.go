package main

import (
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/xnilsit/robot-ptr/internal/controller"
	"github.com/xnilsit/robot-ptr/internal/robot"
)

var (
	// Version is set at build time.
	Version = "dev"
	// Gitsha is set at build time.
	Gitsha = "dev"
)

func main() {
	var (
		metricsAddr      string
		probeAddr        string
		leaderElect      bool
		annotationPrefix string
		nodeSelector     string
		hccmManaged      bool
		defaultPTR       string
		resetDelay       time.Duration
		resyncInterval   time.Duration
		dryRun           bool
		robotURL         string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address the metrics endpoint binds to. Use 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election to allow running multiple replicas.")
	flag.StringVar(&annotationPrefix, "annotation-prefix", controller.DefaultAnnotationPrefix,
		"Prefix of the <prefix>/ptr pod annotation and the <prefix>/default-ptr and <prefix>/ip node annotations.")
	flag.StringVar(&nodeSelector, "node-selector", "", "Label selector restricting the managed nodes (empty = all nodes).")
	flag.BoolVar(&hccmManaged, "hccm-managed", false,
		"Only manage nodes that hcloud-cloud-controller-manager marked as Robot servers "+
			"(spec.providerID hrobot://... or the legacy hcloud://bm-...).")
	flag.StringVar(&defaultPTR, "default-ptr", "",
		"PTR applied when no annotated pod runs on a node. Empty deletes the entry, restoring Hetzner's default name.")
	flag.DurationVar(&resetDelay, "reset-delay", time.Minute,
		"How long to keep a pod's PTR after it left the node before resetting to the default.")
	flag.DurationVar(&resyncInterval, "resync-interval", 15*time.Minute,
		"How often each node's PTR is re-read from the Robot API to correct drift.")
	flag.BoolVar(&dryRun, "dry-run", false, "Log PTR changes without applying them.")
	flag.StringVar(&robotURL, "robot-url", robot.DefaultBaseURL, "Hetzner Robot webservice URL.")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	log.Info("starting robot-ptr", "version", Version, "gitsha", Gitsha, "dryRun", dryRun)

	if err := run(runOptions{
		metricsAddr:    metricsAddr,
		probeAddr:      probeAddr,
		leaderElect:    leaderElect,
		annotations:    controller.AnnotationsWithPrefix(strings.TrimSuffix(annotationPrefix, "/")),
		nodeSelector:   nodeSelector,
		hccmManaged:    hccmManaged,
		defaultPTR:     defaultPTR,
		resetDelay:     resetDelay,
		resyncInterval: resyncInterval,
		dryRun:         dryRun,
		robotURL:       robotURL,
	}); err != nil {
		log.Error(err, "fatal")
		os.Exit(1)
	}
}

type runOptions struct {
	metricsAddr    string
	probeAddr      string
	leaderElect    bool
	annotations    controller.Annotations
	nodeSelector   string
	hccmManaged    bool
	defaultPTR     string
	resetDelay     time.Duration
	resyncInterval time.Duration
	dryRun         bool
	robotURL       string
}

func run(o runOptions) error {
	user, pass := os.Getenv("ROBOT_USER"), os.Getenv("ROBOT_PASSWORD")
	if user == "" || pass == "" {
		return errors.New("ROBOT_USER and ROBOT_PASSWORD must be set")
	}
	if o.resyncInterval <= 0 {
		return errors.New("--resync-interval must be positive")
	}

	selector, err := labels.Parse(o.nodeSelector)
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: clientgoscheme.Scheme,
		Cache: cache.Options{
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {Transform: controller.PodTransform(o.annotations)},
			},
		},
		Metrics:                metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress: o.probeAddr,
		LeaderElection:         o.leaderElect,
		LeaderElectionID:       "robot-ptr.nilsit.dev",
	})
	if err != nil {
		return err
	}

	rc := robot.NewClient(user, pass)
	rc.BaseURL = o.robotURL

	if err := (&controller.NodeReconciler{
		Client:         mgr.GetClient(),
		Robot:          rc,
		Recorder:       mgr.GetEventRecorder("robot-ptr"),
		Annotations:    o.annotations,
		NodeSelector:   selector,
		HCCMManaged:    o.hccmManaged,
		DefaultPTR:     o.defaultPTR,
		ResetDelay:     o.resetDelay,
		ResyncInterval: o.resyncInterval,
		DryRun:         o.dryRun,
	}).SetupWithManager(mgr); err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
