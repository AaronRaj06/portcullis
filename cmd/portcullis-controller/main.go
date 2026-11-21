// Command portcullis-controller is the Portcullis control plane: it watches
// Gateway API resources and PortcullisPolicies and programs the data planes
// through their admin API (internal/controller).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal"
	"github.com/AaronRaj06/portcullis/internal/admin"
	"github.com/AaronRaj06/portcullis/internal/controller"
	"github.com/AaronRaj06/portcullis/internal/controller/v1alpha1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "portcullis-controller:", err)
		os.Exit(1)
	}
}

func nsName(s string) (types.NamespacedName, error) {
	ns, name, ok := strings.Cut(s, "/")
	if !ok || ns == "" || name == "" {
		return types.NamespacedName{}, fmt.Errorf("%q: want namespace/name", s)
	}
	return types.NamespacedName{Namespace: ns, Name: name}, nil
}

func run() error {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		adminSvc    = flag.String("dataplane-admin-service", "portcullis-system/portcullis-admin", "namespace/name of the Service selecting data-plane pods (admin port)")
		adminPort   = flag.Int("dataplane-admin-port", 9901, "data-plane admin port")
		proxySvc    = flag.String("dataplane-service", "portcullis-system/portcullis", "namespace/name of the proxy Service, reported as the Gateway address")
		tokenFile   = flag.String("admin-token-file", "", "file holding the data-plane admin bearer token")
		leaderElect = flag.Bool("leader-elect", true, "use a Lease so only one replica reconciles")
		metricsAddr = flag.String("metrics-addr", ":8081", "controller metrics address")
		healthAddr  = flag.String("health-addr", ":8082", "liveness/readiness probe address")
	)
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if *showVersion {
		fmt.Println("portcullis-controller", internal.Version)
		return nil
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if *adminPort < 1 || *adminPort > 65535 {
		return fmt.Errorf("-dataplane-admin-port %d is not a TCP port", *adminPort)
	}
	adminKey, err := nsName(*adminSvc)
	if err != nil {
		return fmt.Errorf("-dataplane-admin-service: %w", err)
	}
	proxyKey, err := nsName(*proxySvc)
	if err != nil {
		return fmt.Errorf("-dataplane-service: %w", err)
	}
	token := ""
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("-admin-token-file: %w", err)
		}
		if token, err = admin.TokenFromFile(data); err != nil {
			return err
		}
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, gwv1.Install, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *healthAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "portcullis-controller.portcullis.dev",
	})
	if err != nil {
		return err
	}
	r := &controller.Reconciler{
		Client:       mgr.GetClient(),
		Pusher:       &controller.Pusher{Client: &http.Client{Timeout: 15 * time.Second}, Token: token},
		AdminService: adminKey, ProxyService: proxyKey,
		AdminPort: int32(*adminPort), //nolint:gosec // range-checked above
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.Info("starting", "version", internal.Version, "controllerName", controller.ControllerName)
	return mgr.Start(ctrl.SetupSignalHandler())
}
