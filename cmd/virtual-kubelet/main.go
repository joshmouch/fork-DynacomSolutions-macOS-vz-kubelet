package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"
	"github.com/agoda-com/macOS-vz-kubelet/internal/nodeauth"
	"github.com/agoda-com/macOS-vz-kubelet/internal/nodehealth"
	"github.com/agoda-com/macOS-vz-kubelet/internal/vmcap"
	"github.com/agoda-com/macOS-vz-kubelet/internal/volumes"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/provider"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"

	docker "github.com/moby/moby/client"

	"github.com/bombsimon/logrusr/v4"
	"github.com/mitchellh/go-homedir"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	appIdentifier = "com.agoda.fleet.virtualization"
	buildVersion  = "dev"
	k8sVersion    = "v1.36.5" // This should follow the version of k8s.io we are importing

	taintKey    = envOrDefault("VKUBELET_TAINT_KEY", "virtual-kubelet.io/provider")
	taintEffect = envOrDefault("VKUBELET_TAINT_EFFECT", string(corev1.TaintEffectNoSchedule))
	taintValue  = envOrDefault("VKUBELET_TAINT_VALUE", "macos-vz")

	logLevel        = "info"
	traceSampleRate string

	// k8s
	kubeConfigPath  = os.Getenv("KUBECONFIG")
	startupTimeout  time.Duration
	disableTaint    bool
	numberOfWorkers               = 10
	resync          time.Duration = 1 * time.Minute
	providerID      string

	certPath       = os.Getenv("APISERVER_CERT_LOCATION")
	keyPath        = os.Getenv("APISERVER_KEY_LOCATION")
	clientCACert   string
	clientNoVerify bool

	webhookAuth                  bool
	webhookAuthnCacheTTL         time.Duration
	webhookAuthzUnauthedCacheTTL time.Duration
	webhookAuthzAuthedCacheTTL   time.Duration
	nodeName                     = "vk-macos-vz-test"
	listenPort                   = 10250
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "import-tart" {
		os.Exit(importTartMain(os.Args[2:]))
	}
	// build-image is a root-only maintenance command, not the provider: handle it before any
	// Kubernetes client is created.
	if len(os.Args) > 1 && os.Args[1] == "build-image" {
		os.Exit(buildImageMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "create-base" {
		os.Exit(createBaseMain(os.Args[2:]))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	binaryName := filepath.Base(os.Args[0])
	desc := binaryName + " implements a node on a Kubernetes cluster using Virtualization.framework to run macOS Virtual Machines as pods."

	if kubeConfigPath == "" {
		home, _ := homedir.Dir()
		if home != "" {
			kubeConfigPath = filepath.Join(home, ".kube", "config")
		}
	}
	k8sClient, err := nodeutil.ClientsetFromEnv(kubeConfigPath)
	if err != nil {
		log.G(ctx).Fatal(err)
	}

	cmd := &cobra.Command{
		Use:   binaryName,
		Short: desc,
		Long:  desc,
		Run: func(cmd *cobra.Command, args []string) {
			logger := logrus.StandardLogger()
			lvl, err := logrus.ParseLevel(logLevel)
			if err != nil {
				logrus.WithError(err).Fatal("Error parsing log level")
			}
			logger.SetLevel(lvl)

			log.L = logruslogger.FromLogrus(logrus.NewEntry(logger))

			// Set the default logger
			ctx := log.WithLogger(cmd.Context(), log.L)
			if err := run(ctx, k8sClient); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.L.Fatal(err)
				}
				log.L.Debug(err)
			}
		},
	}
	flags := cmd.Flags()

	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	klogFlags.VisitAll(func(f *flag.Flag) {
		f.Name = "klog." + f.Name
		flags.AddGoFlag(f)
	})

	hostName, err := os.Hostname()
	if err != nil {
		log.G(ctx).Fatal(err)
	}
	// lowercase RFC 1123 subdomain
	hostName = strings.ToLower(hostName)

	flags.StringVar(&nodeName, "nodename", hostName, "kubernetes node name")
	flags.StringVar(&providerID, "provider-id", providerID, "provider ID to report to the Kubernetes API server")
	flags.DurationVar(&startupTimeout, "startup-timeout", startupTimeout, "How long to wait for the virtual-kubelet to start")
	flags.BoolVar(&disableTaint, "disable-taint", disableTaint, "disable the node taint")
	flags.StringVar(&logLevel, "log-level", logLevel, "log level.")
	flags.IntVar(&numberOfWorkers, "pod-sync-workers", numberOfWorkers, `set the number of pod synchronization workers`)
	flags.DurationVar(&resync, "full-resync-period", resync, "how often to perform a full resync of pods between kubernetes and the provider")

	flags.StringVar(&clientCACert, "client-verify-ca", os.Getenv("APISERVER_CA_CERT_LOCATION"), "CA cert to use to verify client requests")
	flags.BoolVar(&clientNoVerify, "no-verify-clients", clientNoVerify, "Do not require client certificate validation")
	flags.BoolVar(&webhookAuth, "authentication-token-webhook", webhookAuth, ""+
		"Use the TokenReview API to determine authentication for bearer tokens.")
	flags.DurationVar(&webhookAuthnCacheTTL, "authentication-token-webhook-cache-ttl", webhookAuthnCacheTTL,
		"The duration to cache responses from the webhook token authenticator.")
	flags.DurationVar(&webhookAuthzAuthedCacheTTL, "authorization-webhook-cache-authorized-ttl", webhookAuthzAuthedCacheTTL,
		"The duration to cache 'authorized' responses from the webhook authorizer.")
	flags.DurationVar(&webhookAuthzUnauthedCacheTTL, "authorization-webhook-cache-unauthorized-ttl", webhookAuthzUnauthedCacheTTL,
		"The duration to cache 'unauthorized' responses from the webhook authorizer.")

	flags.StringVar(&traceSampleRate, "trace-sample-rate", traceSampleRate, "set probability of tracing samples")

	if err := cmd.ExecuteContext(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			logrus.WithError(err).Fatal("Error running command")
		}
	}
}

func withTaint(cfg *nodeutil.NodeConfig) error {
	if disableTaint {
		return nil
	}

	taint := corev1.Taint{
		Key:   taintKey,
		Value: taintValue,
	}
	switch taintEffect {
	case "NoSchedule":
		taint.Effect = corev1.TaintEffectNoSchedule
	case "NoExecute":
		taint.Effect = corev1.TaintEffectNoExecute
	case "PreferNoSchedule":
		taint.Effect = corev1.TaintEffectPreferNoSchedule
	default:
		return errdefs.InvalidInputf("taint effect %q is not supported", taintEffect)
	}
	cfg.NodeSpec.Spec.Taints = append(cfg.NodeSpec.Spec.Taints, taint)
	return nil
}

func withProviderID(cfg *nodeutil.NodeConfig) error {
	cfg.NodeSpec.Spec.ProviderID = providerID
	return nil
}

func withVersion(cfg *nodeutil.NodeConfig) error {
	cfg.NodeSpec.Status.NodeInfo.KubeletVersion = strings.Join([]string{k8sVersion, "vk-macos-vz", buildVersion}, "-")
	return nil
}

// attachRoutes is set by configureRoutes; nodeauth.NewNode calls it once the provider exists.
var attachRoutes nodeauth.RouteAttacher

func configureRoutes(cfg *nodeutil.NodeConfig) error {
	mux := http.NewServeMux()
	cfg.Handler = mux
	attachRoutes = nodeauth.ProviderRoutes(mux)
	return nil
}

func withWebhookAuth(ctx context.Context, cfg *nodeutil.NodeConfig) error {
	if !webhookAuth {
		cfg.Handler = api.InstrumentHandler(nodeutil.WithAuth(nodeutil.NoAuth(), cfg.Handler))
		return nil
	}

	auth, err := nodeutil.WebhookAuth(cfg.Client, nodeName,
		func(cfg *nodeutil.WebhookAuthConfig) error {
			var err error

			cfg.AuthzConfig.WebhookRetryBackoff = options.DefaultAuthWebhookRetryBackoff()

			if webhookAuthnCacheTTL > 0 {
				cfg.AuthnConfig.CacheTTL = webhookAuthnCacheTTL
			}
			if webhookAuthzAuthedCacheTTL > 0 {
				cfg.AuthzConfig.AllowCacheTTL = webhookAuthzAuthedCacheTTL
			}
			if webhookAuthzUnauthedCacheTTL > 0 {
				cfg.AuthzConfig.AllowCacheTTL = webhookAuthzUnauthedCacheTTL
			}
			if clientCACert != "" {
				ca, err := dynamiccertificates.NewDynamicCAContentFromFile("client-ca", clientCACert)
				if err != nil {
					return err
				}
				cfg.AuthnConfig.ClientCertificateCAContentProvider = ca
				go ca.Run(ctx, 1)
			}
			return err
		})

	if err != nil {
		return err
	}
	cfg.TLSConfig.ClientAuth = tls.RequestClientCert
	cfg.Handler = api.InstrumentHandler(nodeutil.WithAuth(auth, cfg.Handler))
	return nil
}

func withClient(c kubernetes.Interface, cfg *nodeutil.NodeConfig) error {
	return nodeutil.WithClient(c)(cfg)
}

func withCA(cfg *tls.Config) error {
	if clientCACert == "" {
		return nil
	}
	if err := nodeutil.WithCAFromPath(clientCACert)(cfg); err != nil {
		return fmt.Errorf("error getting CA from path: %w", err)
	}
	if clientNoVerify {
		cfg.ClientAuth = tls.NoClientCert
	}
	return nil
}

func run(ctx context.Context, c kubernetes.Interface) error {
	service := nodeName
	if serviceName := os.Getenv("OTEL_SERVICE_NAME"); serviceName != "" {
		service = serviceName
	}

	if err := configureTracing(
		ctx,
		service,
		traceSampleRate,
		attribute.String("node.name", nodeName),
		attribute.String("taint.key", taintKey),
		attribute.String("taint.effect", taintEffect),
		attribute.String("taint.value", taintValue),
	); err != nil {
		return err
	}

	if _, err := vmcap.FromEnv(); err != nil {
		return err
	}

	// Route controller-runtime's internal logs (used by certwatcher) through logrus
	// so that all log output uses the same format.
	ctrllog.SetLogger(logrusr.New(logrus.StandardLogger()))

	// Set up certificate watcher for automatic TLS cert rotation.
	// Certs on disk are rotated by an external service; without this,
	// the process would keep serving expired in-memory certs until restarted.
	certWatcher, err := certwatcher.New(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("failed to create certificate watcher: %w", err)
	}
	certWatcher.RegisterCallback(func(cert tls.Certificate) {
		log.G(ctx).Info("TLS certificate rotated successfully")
	})
	certWatcherErrCh := make(chan error, 1)
	go func() {
		if err := certWatcher.Start(ctx); err != nil {
			certWatcherErrCh <- fmt.Errorf("certificate watcher failed: %w", err)
		}
	}()

	if port := os.Getenv("KUBELET_PORT"); port != "" {
		kubeletPort, err := strconv.ParseInt(port, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid KUBELET_PORT %q: %w", port, err)
		}
		listenPort = int(kubeletPort)
	}

	node, err := nodeauth.NewNode(nodeName, func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
		platform, _, _, err := host.PlatformInformationWithContext(ctx)
		if err != nil {
			return nil, nil, err
		}

		eventBroadcaster := record.NewBroadcaster()
		eventBroadcaster.StartLogging(log.G(ctx).Infof)
		eventBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: c.CoreV1().Events(corev1.NamespaceAll)})
		eventRecorder := event.NewKubeEventRecorder(
			eventBroadcaster.NewRecorder(
				scheme.Scheme,
				corev1.EventSource{
					Component: provider.ComponentName,
					Host:      nodeName,
				},
			),
		)

		// Create a containerd client to manage non-macOS containers
		// If unavailable - ignore, but warn the user that some features will be unavailable
		dockerCl, err := createDockerClient(ctx)
		if err != nil {
			log.G(ctx).Warnf("failed to create docker client: %v; some features (like non-macOS containers) will be unavailable", err)
		}

		cachePath, err := os.UserCacheDir()
		if err != nil {
			return nil, nil, err
		}
		cachePath = filepath.Join(cachePath, appIdentifier)

		podVolumesDir, err := volumes.ResolvePodVolumesDir(cachePath)
		if err != nil {
			return nil, nil, err
		}
		// remove stale pod volumes but keep the directory itself (it may be a virtiofs share)
		if err := volumes.PreparePodVolumesDir(podVolumesDir); err != nil {
			return nil, nil, err
		}

		if _, err := envcfg.AllowedImagePrefixes(); err != nil {
			return nil, nil, err
		}

		networkInterfaceIdentifier := os.Getenv("VZ_BRIDGE_INTERFACE")
		vzClient := client.NewVzClientAPIs(ctx, eventRecorder, networkInterfaceIdentifier, cachePath, podVolumesDir, dockerCl)

		providerConfig := provider.MacOSVZProviderConfig{
			NodeName:           nodeName,
			Platform:           platform,
			InternalIP:         os.Getenv("VKUBELET_POD_IP"),
			DaemonEndpointPort: int32(listenPort),

			K8sClient:     c,
			EventRecorder: eventRecorder,
			PodsLister:    cfg.Pods,
		}
		p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
		if err != nil {
			return nil, nil, err
		}
		err = p.ConfigureNode(ctx, cfg.Node)
		if err != nil {
			return nil, nil, err
		}
		// Ready follows the Docker side-car daemon (e.g. a stopped Colima shows up as NotReady).
		health := nodehealth.New(cfg.Node, dockerHealthCheck, dockerHealthInterval())
		return p, health, nil
	},
		func(p nodeutil.Provider, cfg nodeutil.NodeConfig, pods corev1listers.PodLister) {
			attachRoutes(p, cfg, pods)
		},
		func(cfg *nodeutil.NodeConfig) error {
			return withClient(c, cfg)
		},
		withTaint,
		withProviderID,
		withVersion,
		nodeutil.WithTLSConfig(
			func(cfg *tls.Config) error {
				cfg.GetCertificate = certWatcher.GetCertificate
				return nil
			},
			withCA,
		),
		func(cfg *nodeutil.NodeConfig) error {
			return withWebhookAuth(ctx, cfg)
		},
		configureRoutes,
		func(cfg *nodeutil.NodeConfig) error {
			cfg.InformerResyncPeriod = resync
			cfg.NumWorkers = numberOfWorkers
			cfg.HTTPListenAddr = fmt.Sprintf(":%d", listenPort)
			return nil
		},
	)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- node.Run(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("error running the node: %w", err)
		}
	case err := <-certWatcherErrCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := node.WaitReady(ctx, startupTimeout); err != nil {
		return fmt.Errorf("error waiting for node to be ready: %w", err)
	}

	<-node.Done()
	return node.Err()
}

// dockerHealthCheck pings the Docker daemon with a fresh client, so it also recovers
// when the daemon was down at startup.
func dockerHealthCheck(ctx context.Context) error {
	cl, err := newDockerClient()
	if err != nil {
		return err
	}
	defer cl.Close()
	_, err = cl.Ping(ctx)
	return err
}

func dockerHealthInterval() time.Duration {
	d, err := time.ParseDuration(os.Getenv("VZ_DOCKER_HEALTH_INTERVAL"))
	if err != nil || d <= 0 {
		return nodehealth.DefaultInterval
	}
	return d
}

func newDockerClient() (*docker.Client, error) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return docker.NewClientWithOpts(docker.WithHost(host))
	}
	return docker.NewClientWithOpts()
}

func createDockerClient(ctx context.Context) (dockerCl *docker.Client, err error) {
	dockerCl, err = newDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	// Validate that client can connect to the socket
	if _, err := dockerCl.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping docker client: %w", err)
	}

	return dockerCl, nil
}

func envOrDefault(key string, defaultValue string) string {
	v, set := os.LookupEnv(key)
	if set {
		return v
	}
	return defaultValue
}
