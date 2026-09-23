package cmd

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	"github.com/openshift/hypershift/hypershift-operator/controllers/nodepool"
	"github.com/openshift/hypershift/ignition-server/controllers"
	hyperapi "github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/releaseinfo"
	"github.com/openshift/hypershift/support/supportedversion"
	"github.com/openshift/hypershift/support/util"

	librarycrypto "github.com/openshift/library-go/pkg/crypto"

	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"go.uber.org/zap/zapcore"
)

const namespaceEnvVariableName = "MY_NAMESPACE"

var (
	// We only match /ignition
	ignPathPattern                       = regexp.MustCompile("^/ignition[^/ ]*$")
	payloadStore                         = controllers.NewPayloadStore()
	getRequestsPerNodePool               = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ign_server_get_request"}, []string{"nodePool"})
	TokenSecretIgnitionReachedAnnotation = "hypershift.openshift.io/ignition-reached"
)

func init() {
	metrics.Registry.MustRegister(
		getRequestsPerNodePool,
	)
}

// NewPayloadStartCommand starts the serving-only half of the IgnitionPayload
// architecture. It deliberately does not register TokenSecretReconciler or a
// renderer: generation is leader-elected in the HyperShift operator.
func NewPayloadStartCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ignition-payload-server", Short: "Starts the serving-only IgnitionPayload server"}
	opts := Options{Addr: "0.0.0.0:9090", MetricsAddr: "0.0.0.0:8080", CertFile: "/var/run/secrets/ignition/serving-cert/tls.crt", KeyFile: "/var/run/secrets/ignition/serving-cert/tls.key"}
	cmd.Flags().StringVar(&opts.Addr, "addr", opts.Addr, "Listen address")
	cmd.Flags().StringVar(&opts.CertFile, "cert-file", opts.CertFile, "Path to the serving cert")
	cmd.Flags().StringVar(&opts.KeyFile, "key-file", opts.KeyFile, "Path to the serving key")
	cmd.Flags().StringVar(&opts.MetricsAddr, "metrics-addr", opts.MetricsAddr, "The address the metric endpoint binds to")
	cmd.Flags().StringVar(&opts.TLSMinVersion, "tls-min-version", "", "Minimum TLS version")
	cmd.Flags().StringSliceVar(&opts.TLSCipherSuites, "tls-cipher-suites", nil, "TLS cipher suites (comma-separated)")
	cmd.Run = func(_ *cobra.Command, _ []string) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		go func() { <-sigs; cancel() }()
		if err := runPayloadServer(ctx, opts); err != nil {
			log.Fatal(err)
		}
	}
	return cmd
}

// NewPayloadControllerCommand runs the singleton rendering half of the
// IgnitionPayload architecture. It intentionally lives in the ignition-server
// binary so it can be deployed per HostedControlPlane with the same image as
// the serving tier, while controller-runtime leader election ensures that only
// one replica performs expensive release-image work.
func NewPayloadControllerCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ignition-payload-controller", Short: "Starts the IgnitionPayload renderer"}
	opts := Options{MetricsAddr: "0.0.0.0:8080", WorkDir: "/payloads", RegistryOverrides: map[string]string{}}
	cmd.Flags().StringVar(&opts.WorkDir, "work-dir", opts.WorkDir, "Directory for release image extraction")
	cmd.Flags().StringVar(&opts.MetricsAddr, "metrics-addr", opts.MetricsAddr, "The address the metric endpoint binds to")
	cmd.Flags().StringToStringVar(&opts.RegistryOverrides, "registry-overrides", opts.RegistryOverrides, "Registry override map")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		return runPayloadController(ctrl.SetupSignalHandler(), opts)
	}
	return cmd
}

func runPayloadController(ctx context.Context, opts Options) error {
	namespace := os.Getenv(namespaceEnvVariableName)
	if namespace == "" {
		return fmt.Errorf("environment variable %s is empty, this is not supported", namespaceEnvVariableName)
	}
	restConfig := ctrl.GetConfigOrDie()
	restConfig.UserAgent = "ignition-payload-controller"
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  hyperapi.Scheme,
		Metrics:                 metricsserver.Options{BindAddress: opts.MetricsAddr},
		Cache:                   cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		LeaderElection:          true,
		LeaderElectionID:        "ignition-payload-controller.hypershift.openshift.io",
		LeaderElectionNamespace: namespace,
		HealthProbeBindAddress:  "0",
	})
	if err != nil {
		return fmt.Errorf("create payload controller manager: %w", err)
	}
	releaseProvider := &releaseinfo.ProviderWithOpenShiftImageRegistryOverridesDecorator{
		Delegate: &releaseinfo.RegistryMirrorProviderDecorator{
			Delegate:          &releaseinfo.CachedProvider{Inner: &releaseinfo.RegistryClientProvider{}, Cache: map[string]*releaseinfo.ReleaseImage{}},
			RegistryOverrides: opts.RegistryOverrides,
		},
		OpenShiftImageRegistryOverrides: util.ConvertImageRegistryOverrideStringToMap(os.Getenv("OPENSHIFT_IMG_OVERRIDES")),
	}
	metadataProvider := &util.RegistryClientImageMetadataProvider{OpenShiftImageRegistryOverrides: util.ConvertImageRegistryOverrideStringToMap(os.Getenv("OPENSHIFT_IMG_OVERRIDES"))}
	if err := (&ignitionpayload.Reconciler{
		Client: mgr.GetClient(),
		Renderer: &ignitionpayload.MCORenderer{
			Client: mgr.GetClient(), ReleaseProvider: releaseProvider, MetadataProvider: metadataProvider, WorkDir: opts.WorkDir,
		},
		Store: &ignitionpayload.SecretBackedStore{Client: mgr.GetClient(), Namespace: namespace},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up payload controller: %w", err)
	}
	return mgr.Start(ctx)
}

func runPayloadServer(ctx context.Context, opts Options) error {
	namespace := os.Getenv(namespaceEnvVariableName)
	if namespace == "" {
		return fmt.Errorf("environment variable %s is empty, this is not supported", namespaceEnvVariableName)
	}
	certWatcher, err := certwatcher.New(opts.CertFile, opts.KeyFile)
	if err != nil {
		return fmt.Errorf("failed to load serving cert: %w", err)
	}
	restConfig := ctrl.GetConfigOrDie()
	restConfig.UserAgent = "ignition-payload-server"
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 hyperapi.Scheme,
		Metrics:                metricsserver.Options{BindAddress: opts.MetricsAddr},
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return fmt.Errorf("create serving manager: %w", err)
	}
	if err := mgr.Add(certWatcher); err != nil {
		return fmt.Errorf("add certificate watcher: %w", err)
	}
	server := &ignitionpayload.Server{Client: mgr.GetClient(), Store: &ignitionpayload.SecretBackedStore{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: namespace}, Namespace: namespace}
	secretInformer, err := mgr.GetCache().GetInformer(ctx, &corev1.Secret{})
	if err != nil {
		return fmt.Errorf("get payload Secret informer: %w", err)
	}
	_, err = secretInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { updatePayloadServerCache(server, obj) },
		UpdateFunc: func(_, newObj interface{}) { updatePayloadServerCache(server, newObj) },
		DeleteFunc: func(obj interface{}) { deletePayloadServerCache(server, obj) },
	})
	if err != nil {
		return fmt.Errorf("add payload Secret informer handler: %w", err)
	}
	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Printf("payload serving manager stopped: %v", err)
		}
	}()
	httpServer := &http.Server{
		Addr:         opts.Addr,
		Handler:      server,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		TLSConfig:    buildTLSConfig(certWatcher, opts),
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
	go func() { <-ctx.Done(); _ = httpServer.Shutdown(context.Background()) }()
	if err := httpServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func updatePayloadServerCache(server *ignitionpayload.Server, obj interface{}) {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Labels["hypershift.openshift.io/ignition-payload-token"] == "" {
		return
	}
	if payload, ok := secret.Data["payload"]; ok {
		server.Put(secret.Labels["hypershift.openshift.io/ignition-payload-token"], payload)
	}
}

func deletePayloadServerCache(server *ignitionpayload.Server, obj interface{}) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
			secret, _ = tombstone.Obj.(*corev1.Secret)
		}
	}
	if secret != nil && secret.Labels["hypershift.openshift.io/ignition-payload-token"] != "" {
		server.Delete(secret.Labels["hypershift.openshift.io/ignition-payload-token"])
	}
}

func buildTLSConfig(certWatcher *certwatcher.CertWatcher, opts Options) *tls.Config {
	cfg := &tls.Config{
		GetCertificate: certWatcher.GetCertificate,
	}

	if opts.TLSMinVersion != "" {
		minVersion, err := librarycrypto.TLSVersion(opts.TLSMinVersion)
		if err != nil {
			log.Fatalf("invalid TLS min version: %v", err)
		}
		cfg.MinVersion = minVersion
	}

	if len(opts.TLSCipherSuites) > 0 {
		cfg.CipherSuites = librarycrypto.CipherSuitesOrDie(opts.TLSCipherSuites)
	}

	return librarycrypto.SecureTLSConfig(cfg)
}

type Options struct {
	Addr                string
	CertFile            string
	KeyFile             string
	RegistryOverrides   map[string]string
	Platform            string
	WorkDir             string
	MetricsAddr         string
	FeatureGateManifest string
	TLSMinVersion       string
	TLSCipherSuites     []string
}

// This is a https server that enable us to satisfy
// 1 - 1 relation between clusters and ign endpoints.
// It runs a token Secret controller.
// The token Secret controller uses an IgnitionProvider provider implementation
// (e.g. machineConfigServerIgnitionProvider) to keep up to date a payload store in memory.
// The payload store has the structure "NodePool token": "payload".
// A token represents a given cluster version (and in the future also a machine Config) at any given point in time.
// For a request to succeed a token needs to be passed in the Header.
func NewStartCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ignition-server",
		Short: "Starts the ignition server",
	}

	opts := Options{
		Addr:              "0.0.0.0:9090",
		MetricsAddr:       "0.0.0.0:8080",
		CertFile:          "/var/run/secrets/ignition/serving-cert/tls.crt",
		KeyFile:           "/var/run/secrets/ignition/serving-cert/tls.key",
		WorkDir:           "/payloads",
		RegistryOverrides: map[string]string{},
	}

	cmd.Flags().StringVar(&opts.Addr, "addr", opts.Addr, "Listen address")
	cmd.Flags().StringVar(&opts.CertFile, "cert-file", opts.CertFile, "Path to the serving cert")
	cmd.Flags().StringVar(&opts.KeyFile, "key-file", opts.KeyFile, "Path to the serving key")
	cmd.Flags().StringToStringVar(&opts.RegistryOverrides, "registry-overrides", map[string]string{}, "registry-overrides contains the source registry string as a key and the destination registry string as value. Images before being applied are scanned for the source registry string and if found the string is replaced with the destination registry string. Format is: sr1=dr1,sr2=dr2")
	cmd.Flags().StringVar(&opts.Platform, "platform", "", "The cloud provider platform name")
	cmd.Flags().StringVar(&opts.WorkDir, "work-dir", opts.WorkDir, "Directory in which to store transient working data")
	cmd.Flags().StringVar(&opts.MetricsAddr, "metrics-addr", opts.MetricsAddr, "The address the metric endpoint binds to.")
	cmd.Flags().StringVar(&opts.FeatureGateManifest, "feature-gate-manifest", opts.FeatureGateManifest, "Path to a rendered featuregates.config.openshift.io/v1 file")
	cmd.Flags().StringVar(&opts.TLSMinVersion, "tls-min-version", "", "Minimum TLS version (e.g., VersionTLS12, VersionTLS13)")
	cmd.Flags().StringSliceVar(&opts.TLSCipherSuites, "tls-cipher-suites", nil, "TLS cipher suites (comma-separated)")

	cmd.Run = func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		go func() {
			<-sigs
			cancel()
		}()

		// TODO: Add an fsnotify watcher to cancel the context and trigger a restart
		// if any of the secret data has changed.
		if err := run(ctx, opts); err != nil {
			log.Fatal(err)
		}
	}

	return cmd
}

// setUpPayloadStoreReconciler sets up manager with a TokenSecretReconciler controller
// to keep the PayloadStore up to date.
func setUpPayloadStoreReconciler(ctx context.Context, registryOverrides map[string]string, cloudProvider hyperv1.PlatformType, cacheDir string, metricsAddr string, featureGateManifest string) (ctrl.Manager, error) {
	if os.Getenv(namespaceEnvVariableName) == "" {
		return nil, fmt.Errorf("environment variable %s is empty, this is not supported", namespaceEnvVariableName)
	}

	restConfig := ctrl.GetConfigOrDie()
	restConfig.UserAgent = "ignition-server-manager"
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: hyperapi.Scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: 9443,
		}),
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{os.Getenv(namespaceEnvVariableName): {}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("unable to start manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("unable to set up ready check: %w", err)
	}

	imageFileCache, err := controllers.NewImageFileCache(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("unable to create image file cache: %w", err)
	}

	imageMetaDataProvider := &util.RegistryClientImageMetadataProvider{
		OpenShiftImageRegistryOverrides: util.ConvertImageRegistryOverrideStringToMap(os.Getenv("OPENSHIFT_IMG_OVERRIDES")),
	}

	if err = (&controllers.TokenSecretReconciler{
		Client:       mgr.GetClient(),
		PayloadStore: payloadStore,
		IgnitionProvider: &controllers.LocalIgnitionProvider{
			ReleaseProvider: &releaseinfo.ProviderWithOpenShiftImageRegistryOverridesDecorator{
				Delegate: &releaseinfo.RegistryMirrorProviderDecorator{
					Delegate: &releaseinfo.CachedProvider{
						Inner: &releaseinfo.RegistryClientProvider{},
						Cache: map[string]*releaseinfo.ReleaseImage{},
					},
					RegistryOverrides: registryOverrides,
				},
				OpenShiftImageRegistryOverrides: util.ConvertImageRegistryOverrideStringToMap(os.Getenv("OPENSHIFT_IMG_OVERRIDES")),
			},
			Client:                mgr.GetClient(),
			Namespace:             os.Getenv(namespaceEnvVariableName),
			CloudProvider:         cloudProvider,
			WorkDir:               cacheDir,
			ImageFileCache:        imageFileCache,
			FeatureGateManifest:   featureGateManifest,
			ImageMetadataProvider: imageMetaDataProvider,
		},
	}).SetupWithManager(ctx, mgr); err != nil {
		return nil, fmt.Errorf("unable to create controller: %w", err)
	}

	return mgr, nil
}

func run(ctx context.Context, opts Options) error {
	logger := zap.New(zap.JSONEncoder(func(o *zapcore.EncoderConfig) {
		o.EncodeTime = zapcore.RFC3339TimeEncoder
	}))
	ctrl.SetLogger(logger)
	logger.Info("Starting ignition-server", "version", supportedversion.String())

	certWatcher, err := certwatcher.New(opts.CertFile, opts.KeyFile)
	if err != nil {
		return fmt.Errorf("failed to load serving cert: %w", err)
	}

	mgr, err := setUpPayloadStoreReconciler(ctx, opts.RegistryOverrides, hyperv1.PlatformType(opts.Platform), opts.WorkDir, opts.MetricsAddr, opts.FeatureGateManifest)
	if err != nil {
		return fmt.Errorf("error setting up manager: %w", err)
	}
	if err := mgr.Add(certWatcher); err != nil {
		return fmt.Errorf("failed to add certWatcher to manager: %w", err)
	}
	go func() {
		err := mgr.Start(ctx)
		if err != nil {
			logger.Error(err, "failed to start manager")
		}
	}()

	mgr.GetLogger().Info("Using opts", "opts", fmt.Sprintf("%+v", opts))
	eventRecorder := mgr.GetEventRecorderFor("ignition-server")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("User Agent: %s. Requested: %s", r.Header.Get("User-Agent"), r.URL.Path)

		tokenSecret := nodepool.TokenSecret(os.Getenv(namespaceEnvVariableName),
			util.ParseNamespacedName(r.Header.Get("NodePool")).Name,
			r.Header.Get("TargetConfigVersionHash"))

		if !ignPathPattern.MatchString(r.URL.Path) {
			// No pattern matched; send 404 response.
			log.Printf("Path not found: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		// Authorize the request against the token
		const bearerPrefix = "Bearer "
		auth := r.Header.Get("Authorization")
		n := len(bearerPrefix)
		if len(auth) < n || auth[:n] != bearerPrefix {
			log.Printf("Invalid Authorization header value prefix")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			eventRecorder.Event(tokenSecret, corev1.EventTypeWarning, "GetPayloadFailed", "Bad header")
			return
		}
		encodedToken := auth[n:]
		decodedToken, err := base64.StdEncoding.DecodeString(encodedToken)
		if err != nil {
			log.Printf("Invalid token value")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			eventRecorder.Event(tokenSecret, corev1.EventTypeWarning, "GetPayloadFailed", "Token invalid")
			return
		}

		value, ok := payloadStore.Get(string(decodedToken))
		if !ok {
			// We return a 5xx here to give ignition the chance to backoff and retry if the machine request happens
			// before the content is cached for this token.
			// https://coreos.github.io/ignition/operator-notes/#http-backoff-and-retry
			log.Printf("Token not found")
			http.Error(w, "Token not found", http.StatusNetworkAuthenticationRequired)
			eventRecorder.Event(tokenSecret, corev1.EventTypeWarning, "GetPayloadFailed", "Token not found in cache")
			return
		}

		if err := util.SanitizeIgnitionPayload(value.Payload); err != nil {
			log.Printf("Invalid ignition payload: %s", err)
			http.Error(w, "Invalid ignition payload", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(value.Payload)

		eventRecorder.Event(tokenSecret, corev1.EventTypeNormal, "GetPayload", "")
		getRequestsPerNodePool.WithLabelValues(r.Header.Get("NodePool")).Inc()

		// Annotate tokenSecret so NodePool controller can set a conditions based on it.
		if err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(tokenSecret), tokenSecret); err != nil {
			log.Printf("Failed to get tokenSecret resource: %q: %s", client.ObjectKeyFromObject(tokenSecret).String(), err)
		} else {
			tokenSecret.Annotations[TokenSecretIgnitionReachedAnnotation] = "True"
			if err := mgr.GetClient().Update(ctx, tokenSecret); err != nil {
				log.Printf("Failed to update tokenSecret: %q: %s", tokenSecret.Name, err)
			}
		}
	})
	mux.HandleFunc("/healthz", func(http.ResponseWriter, *http.Request) {})

	server := http.Server{
		Addr:         opts.Addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		TLSConfig:    buildTLSConfig(certWatcher, opts),
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	go func() {
		<-ctx.Done()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("error shutting down server: %s", err)
		}
	}()

	log.Printf("Listening on %s", opts.Addr)
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
