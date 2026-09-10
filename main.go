package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"synapse-operator/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(networkingv1.AddToScheme(scheme))
	utilruntime.Must(gwv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var watchedNamespace string
	var labelSelector string
	var configHashAnnotation string
	var ignoredConfigMapKeys string
	var ignoredSecretKeys string
	var ingressMode bool
	var upstreamsResolver bool
	var netvarsResolver bool
	var idsHotReloadHashExclude bool
	var perWorkloadConfigHash bool
	var renderOnce bool
	var configSync bool
	var watchConfigMaps repeatedString
	var outDir string
	var syncKeys string
	var syncOnce bool
	var ensureFiles string
	var ingressClass string
	var upstreamsOut string
	var upstreamsOutConfigMap string
	var resolveBackendClusterIPs bool
	var resolveBackendEndpoints bool
	var clusterDomain string
	var certsOut string
	var certsOutSecret string
	var gatewayAPI bool
	var publishStatusAddress string
	var reloadProcessName string
	var reloadDebounce time.Duration
	var statusLeaderElection bool
	var statusLeaderElectionID string
	var leaderElectionNamespace string
	var identityProducer bool
	var downloadAPIURL string
	var identityProducerInterval time.Duration
	var edgeProducer bool
	var edgeProducerInterval time.Duration
	var clusterID string

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&watchedNamespace, "namespace", "", "Namespace to watch. Defaults to all namespaces.")
	flag.StringVar(&labelSelector, "label-selector", "app.kubernetes.io/name=synapse", "Label selector for config sources and workloads.")
	flag.StringVar(&configHashAnnotation, "config-hash-annotation", "synapse.gen0sec.com/config-hash", "Annotation key to store the config hash.")
	flag.StringVar(&ignoredConfigMapKeys, "ignore-configmap-keys", "upstreams.yaml", "Comma-separated ConfigMap keys to ignore when hashing.")
	flag.StringVar(&ignoredSecretKeys, "ignore-secret-keys", "", "Comma-separated Secret keys to ignore when hashing.")
	flag.BoolVar(&ingressMode, "ingress-mode", false, "Run as a Kubernetes Ingress + Gateway API controller (sidecar) instead of the config-hash controller: render class-matched Ingresses/HTTPRoutes into a synapse upstreams.yaml.")
	flag.BoolVar(&upstreamsResolver, "upstreams-resolver", false, "Enable the UpstreamsResolverReconciler: watch ConfigMaps labelled synapse.gen0sec.com/resolve-upstreams=true, substitute backend Service DNS names with their ClusterIPs, write the result to a sibling ConfigMap. Composable with other modes.")
	flag.BoolVar(&netvarsResolver, "netvars-resolver", false, "Enable the NetVarsResolverReconciler: watch synapse agent config ConfigMaps labelled synapse.gen0sec.com/resolve-netvars=true and fill ids.address_vars.HOME_NET/EXTERNAL_NET from cluster Node IPs + PodCIDRs + LoadBalancer VIPs + RFC1918 supernets (so inline IDS blocking never bans an internal IP). Honours a manual HOME_NET in the config or the synapse.gen0sec.com/home-net annotation. Composable with other modes.")
	flag.BoolVar(&idsHotReloadHashExclude, "ids-hot-reload-hash-exclude", false, "Exclude the hot-reloadable thalamus IDS fields (ids.address_vars/enforce_block/rule_paths/port_vars/flow_timeout_secs/max_flows) from the config-hash, so a change to ONLY those does not roll the workload (the agent hot-reloads them in process). Other config.yaml changes still roll. Only safe once all agents run a synapse image that hot-reloads these fields (r32+).")
	flag.BoolVar(&perWorkloadConfigHash, "per-workload-config-hash", false, "Stamp each workload with a hash of ONLY the labelled ConfigMaps/Secrets it actually references (volumes/envFrom/env), instead of one combined hash over every labelled source in the namespace. Stops an unrelated config change (e.g. agent rules) from rolling other workloads (e.g. the proxy). A workload that references no labelled source is left untouched.")
	flag.BoolVar(&renderOnce, "render-once", false, "Ingress-mode one-shot: render upstreams.yaml from current Ingresses/HTTPRoutes and exit (initContainer; primes the file before synapse starts).")
	flag.StringVar(&ingressClass, "ingress-class", "synapse", "spec.ingressClassName this controller serves (ingress-mode).")
	flag.StringVar(&upstreamsOut, "upstreams-out", "/shared/upstreams.yaml", "Path to write the rendered synapse upstreams.yaml (ingress-mode, sidecar layout: a shared volume synapse inotify-reloads). Ignored when --upstreams-out-configmap is set.")
	flag.StringVar(&upstreamsOutConfigMap, "upstreams-out-configmap", "", "Ingress-mode central layout: write the rendered upstreams.yaml to this ConfigMap (format namespace/name) instead of a file path. Disables SIGHUP signalling — synapse-proxy reloads via its own machinery on the ConfigMap mount.")
	flag.BoolVar(&resolveBackendEndpoints, "resolve-backend-endpoints", false, "Ingress-mode: render the READY pod IPs from EndpointSlices as the server list, instead of a single Service address. Pod IPs are literals, so synapse never resolves DNS for them and a replaced pod is picked up on the next render instead of after its DNS-cache TTL. Falls back to ClusterIP/FQDN when the endpoint set is missing or empty.")
	flag.BoolVar(&resolveBackendClusterIPs, "resolve-backend-cluster-ips", false, "Ingress-mode: emit `<clusterIP>:port` instead of `<svc>.<ns>.svc.<cluster-domain>:port` for each backend, so synapse-proxy's HttpPeer skips DNS. Falls back to the FQDN for headless / ExternalName / not-yet-allocated Services.")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local", "Cluster DNS domain for backend FQDNs (ingress-mode).")
	flag.BoolVar(&identityProducer, "identity-producer", false, "Enable the IdentityProducerReconciler: build a workload-identity MMDB (pod IP -> workload/namespace/app) from cluster Pods and upload it to the download-api (--download-api-url) so agents can pull it for east-west detection. Needs an API key with identity:write (env SYNAPSE_API_KEY). Composable with other modes.")
	flag.StringVar(&downloadAPIURL, "download-api-url", "https://api.gen0sec.com/v1", "download-api base URL the identity producer uploads to (uses {url}/identity/upload). Only used with --identity-producer.")
	flag.StringVar(&clusterID, "cluster-id", "", "Cluster identifier that scopes this cluster's identity/edge baseline in the download-api, so multiple clusters sharing one workspace don't overwrite each other's MMDB (last-writer-wins). Empty = the legacy shared global path. To isolate, set a stable unique value here AND the matching platform.identity.cluster on every agent in this cluster.")
	flag.DurationVar(&identityProducerInterval, "identity-producer-interval", 5*time.Minute, "How often the identity producer re-uploads the full MMDB *baseline* (cold-start/resync source). Per-change freshness is delivered by event-driven deltas, so this can be infrequent. Only used with --identity-producer.")
	flag.BoolVar(&edgeProducer, "edge-producer", false, "Enable the EdgeProducerReconciler: compile cluster NetworkPolicy into a declared-edge allow-list (workload->workload:port + governed destinations) and upload it to the download-api (--download-api-url) for the agent's `edge.*` microsegmentation fields. Needs an API key with identity:write (env SYNAPSE_API_KEY). Composable with other modes.")
	flag.DurationVar(&edgeProducerInterval, "edge-producer-interval", 5*time.Minute, "How often the edge producer re-uploads the full declared-edge allow-list *baseline* (cold-start/resync). Per-change freshness is delivered by event-driven deltas, so this can be infrequent. Only used with --edge-producer.")
	flag.StringVar(&certsOut, "certs-out", "", "Ingress-mode: directory to project referenced Ingress/Gateway TLS Secrets into as <stem>.crt/<stem>.key (synapse's certificates dir; operator-owned, inotify-hot-reloaded). Empty = multi-cert disabled (legacy static mount).")
	flag.StringVar(&certsOutSecret, "certs-out-secret", "", "Ingress-mode central layout: project referenced Ingress/Gateway TLS Secrets into this Secret (format namespace/name) as <stem>.crt/<stem>.key data keys, instead of a local dir. The separate synapse-proxy pod mounts this Secret as its certificates dir, so certs are auto-wired from Ingress TLS (no hand-maintained projected-volume list). Takes precedence over --certs-out.")
	flag.BoolVar(&gatewayAPI, "gateway-api", false, "Also reconcile Gateway API (GatewayClass/Gateway/HTTPRoute) into the same upstreams.yaml (ingress-mode; requires the Gateway API CRDs).")
	flag.StringVar(&publishStatusAddress, "publish-status-address", "", "Comma-separated IPs/hostnames to publish on matched Ingresses' .status.loadBalancer.ingress (ingress-mode). Empty = do not publish.")
	flag.StringVar(&reloadProcessName, "reload-process-name", "synapse", "argv0 basename of the co-located proxy process to SIGHUP on a changed render (ingress-mode).")
	flag.DurationVar(&reloadDebounce, "reload-debounce", 500*time.Millisecond, "Coalesce SIGHUP reload bursts within this window (ingress-mode; 0 = signal immediately on every changed render).")
	flag.BoolVar(&statusLeaderElection, "status-leader-election", false, "Ingress-mode: with >1 proxy replica, only the Lease holder writes shared cluster status (Gateway/HTTPRoute status, Ingress .status.loadBalancer). Per-pod render+SIGHUP is never gated. Off ⇒ every replica writes (single-replica default).")
	flag.BoolVar(&configSync, "config-sync", false, "Thin reload-sidecar mode: watch --watch-configmap ConfigMaps via the Kubernetes API (NOT via a mounted volume, which kubelet only re-projects on its ~60s sync loop), project their keys into --out-dir, and SIGHUP the co-located synapse process. Mutually exclusive with --ingress-mode.")
	flag.Var(&watchConfigMaps, "watch-configmap", "config-sync: ConfigMap to project, as namespace/name. Repeatable.")
	flag.StringVar(&outDir, "out-dir", "/shared", "config-sync: directory to project ConfigMap keys into (the volume shared with synapse).")
	flag.StringVar(&syncKeys, "sync-keys", "", "config-sync: comma-separated ConfigMap data keys to project. Empty = every key.")
	flag.BoolVar(&syncOnce, "sync-once", false, "config-sync one-shot: project once, guarantee --ensure-files exist, and exit (initContainer; primes the files before synapse starts).")
	flag.StringVar(&ensureFiles, "ensure-files", "upstreams.yaml", "config-sync: comma-separated filenames that must exist in --out-dir before synapse starts. Missing ones get a minimal valid document, because synapse-proxy aborts its whole background service (and never establishes a file watch) if the initial upstreams read fails.")
	flag.StringVar(&statusLeaderElectionID, "status-leader-election-id", "synapse-ingress-status", "Lease name for the shared-status election (ingress-mode).")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "", "Namespace for the shared-status Lease (ingress-mode; defaults to $POD_NAMESPACE, then \"default\").")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	outCM, err := parseNamespacedName(upstreamsOutConfigMap)
	if err != nil {
		setupLog.Error(err, "--upstreams-out-configmap")
		os.Exit(1)
	}

	outCertsSecret, err := parseNamespacedName(certsOutSecret)
	if err != nil {
		setupLog.Error(err, "--certs-out-secret")
		os.Exit(1)
	}

	syncSources, err := parseNamespacedNameList(watchConfigMaps)
	if err != nil {
		setupLog.Error(err, "--watch-configmap")
		os.Exit(1)
	}

	if configSync {
		// config-sync is a per-pod sidecar, not a cluster controller. Every
		// conflicting mode below elects a leader or writes cluster state,
		// which a sidecar must never do.
		switch {
		case ingressMode:
			setupLog.Error(nil, "--config-sync cannot be combined with --ingress-mode")
			os.Exit(1)
		case upstreamsResolver || netvarsResolver:
			setupLog.Error(nil, "--config-sync cannot be combined with the resolver controllers")
			os.Exit(1)
		case enableLeaderElection:
			// Losing an election would silently stop delivering config to
			// THIS pod while the pod still reports healthy — an invisible
			// outage. Refuse rather than degrade.
			setupLog.Error(nil, "--config-sync must not use --leader-elect (it runs once per proxy pod)")
			os.Exit(1)
		case len(syncSources) == 0:
			setupLog.Error(nil, "--config-sync requires at least one --watch-configmap namespace/name")
			os.Exit(1)
		}
		// synapse binds :8080 for health and :9180 for internal services in
		// the same pod netns, and the operator's metrics default is :8080.
		// Disable both unless explicitly overridden.
		if !flagWasSet("metrics-bind-address") {
			metricsAddr = "0"
		}
		if !flagWasSet("health-probe-bind-address") {
			probeAddr = "0"
		}
		if len(syncSources) > 1 && strings.TrimSpace(syncKeys) == "" {
			setupLog.Info("WARNING: multiple --watch-configmap sources with no --sync-keys; " +
				"identically-named keys across sources overwrite each other (last write wins)")
		}
	}

	if configSync && syncOnce {
		cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			setupLog.Error(err, "sync-once: client")
			os.Exit(1)
		}
		cs := &controllers.ConfigSyncReconciler{
			Client:  cl,
			Sources: syncSources,
			OutDir:  outDir,
			Keys:    parseKeySetOrNil(syncKeys),
		}
		// A projection failure is not fatal: EnsureFiles still lays down a
		// valid floor so synapse can start and establish its watch, and the
		// long-running sidecar delivers the real content moments later.
		if _, err := cs.SyncAll(context.Background()); err != nil {
			setupLog.Error(err, "sync-once: initial projection failed; falling back to the ensure-files floor")
		}
		if err := cs.EnsureFiles(strings.Split(ensureFiles, ",")); err != nil {
			setupLog.Error(err, "sync-once: could not ensure required files")
			os.Exit(1)
		}
		os.Exit(0)
	}

	if ingressMode && renderOnce {
		cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			setupLog.Error(err, "render-once: client")
			os.Exit(1)
		}
		ir := &controllers.IngressReconciler{
			Client:                   cl,
			IngressClassName:         ingressClass,
			UpstreamsOutPath:         upstreamsOut,
			UpstreamsOutConfigMap:    outCM,
			ResolveBackendClusterIPs: resolveBackendClusterIPs,
			ResolveBackendEndpoints:  resolveBackendEndpoints,
			CertsOutDir:              certsOut,
			CertsOutSecret:           outCertsSecret,
			ClusterDomain:            clusterDomain,
			GatewayAPI:               gatewayAPI,
		}
		if err := ir.RenderOnce(context.Background()); err != nil {
			setupLog.Error(err, "render-once failed")
			os.Exit(1)
		}
		os.Exit(0)
	}

	if strings.TrimSpace(configHashAnnotation) == "" {
		setupLog.Error(nil, "config-hash-annotation cannot be empty")
		os.Exit(1)
	}

	selector, err := parseLabelSelector(labelSelector)
	if err != nil {
		setupLog.Error(err, "invalid label selector", "selector", labelSelector)
		os.Exit(1)
	}

	ignoredConfigMapSet := parseKeySet(ignoredConfigMapKeys)
	ignoredSecretSet := parseKeySet(ignoredSecretKeys)

	mgrOptions := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "86a223f3.synapse.gen0sec.com",
	}

	if watchedNamespace != "" {
		mgrOptions.Cache.DefaultNamespaces = map[string]cache.Config{
			watchedNamespace: {},
		}
	}

	if configSync {
		// Scope the ConfigMap informer to just the sources. This is a cache
		// optimisation, NOT a privilege boundary: RBAC resourceNames is
		// ignored for list/watch, so the sidecar's Role still grants read on
		// every ConfigMap in the namespace. It keeps a sidecar-per-proxy-pod
		// from each caching every ConfigMap in the cluster.
		byNS := map[string]cache.Config{}
		for _, src := range syncSources {
			if existing, seen := byNS[src.Namespace]; seen {
				// A field selector cannot express "name A OR name B", so
				// widen to the whole namespace once a second name appears.
				existing.FieldSelector = nil
				byNS[src.Namespace] = existing
				continue
			}
			byNS[src.Namespace] = cache.Config{
				FieldSelector: fields.OneTermEqualSelector("metadata.name", src.Name),
			}
		}
		mgrOptions.Cache.ByObject = map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {Namespaces: byNS},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if configSync {
		cs := &controllers.ConfigSyncReconciler{
			Client:  mgr.GetClient(),
			Sources: syncSources,
			OutDir:  outDir,
			Keys:    parseKeySetOrNil(syncKeys),
			Signaler: &controllers.ReloadSignaler{
				ProcessName: reloadProcessName,
				Debounce:    reloadDebounce,
			},
		}
		if err := cs.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create config-sync controller")
			os.Exit(1)
		}
		if err := mgr.Add(controllers.NewConfigSyncPrimer(cs)); err != nil {
			setupLog.Error(err, "unable to add config-sync primer")
			os.Exit(1)
		}
		setupLog.Info("config-sync sidecar enabled",
			"sources", watchConfigMaps.String(), "outDir", outDir,
			"keys", syncKeys, "reloadProcess", reloadProcessName)
	}

	var ingressReconciler *controllers.IngressReconciler
	if ingressMode {
		ingressReconciler = &controllers.IngressReconciler{
			Client:                   mgr.GetClient(),
			IngressClassName:         ingressClass,
			UpstreamsOutPath:         upstreamsOut,
			UpstreamsOutConfigMap:    outCM,
			ResolveBackendClusterIPs: resolveBackendClusterIPs,
			ResolveBackendEndpoints:  resolveBackendEndpoints,
			CertsOutDir:              certsOut,
			CertsOutSecret:           outCertsSecret,
			ClusterDomain:            clusterDomain,
			GatewayAPI:               gatewayAPI,
			// SignalReload is the sidecar-mode reload mechanism. The
			// reconciler also no-ops it internally in central mode (when
			// UpstreamsOutConfigMap is set), but we still gate the
			// constructor flag here for clarity: there's no co-located
			// synapse process to signal when we write to a ConfigMap.
			SignalReload:      outCM.Name == "",
			StatusAddresses:   parseCSV(publishStatusAddress),
			ReloadProcessName: reloadProcessName,
			ReloadDebounce:    reloadDebounce,
			Recorder:          mgr.GetEventRecorderFor("synapse-ingress"),
		}
		if statusLeaderElection {
			ns := leaderElectionNamespace
			if ns == "" {
				ns = os.Getenv("POD_NAMESPACE")
			}
			gate := &controllers.LeaderGate{}
			ingressReconciler.IsLeader = gate.IsLeader
			if err = mgr.Add(controllers.NewStatusLeaderElection(
				mgr.GetConfig(), ns, statusLeaderElectionID, os.Getenv("POD_NAME"), gate)); err != nil {
				setupLog.Error(err, "unable to add status leader election")
				os.Exit(1)
			}
		}
		if err = ingressReconciler.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Ingress")
			os.Exit(1)
		}
		if err = mgr.Add(controllers.NewRenderPrimer(ingressReconciler)); err != nil {
			setupLog.Error(err, "unable to add render primer")
			os.Exit(1)
		}
		ingressReconciler.LogStartup(setupLog)
	} else if configSync {
		// config-sync runs alone in its pod. The config-hash controller is a
		// cluster-scoped concern and would additionally collide on the
		// derived controller name "configmap".
		setupLog.Info("config-sync mode: config-hash controller not registered")
	} else if err = (&controllers.ConfigMapReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		LabelSelector:             selector,
		ConfigHashAnnotation:      configHashAnnotation,
		IgnoredConfigMapKeys:      ignoredConfigMapSet,
		IgnoredSecretKeys:         ignoredSecretSet,
		ExcludeHotReloadIdsFields: idsHotReloadHashExclude,
		PerWorkloadConfigHash:     perWorkloadConfigHash,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ConfigMap")
		os.Exit(1)
	}

	if upstreamsResolver {
		ur := &controllers.UpstreamsResolverReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			ClusterDomain: clusterDomain,
		}
		if err = ur.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "UpstreamsResolver")
			os.Exit(1)
		}
		ur.LogStartup(setupLog)
	}

	if netvarsResolver {
		nr := &controllers.NetVarsResolverReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}
		if err = nr.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "NetVarsResolver")
			os.Exit(1)
		}
		nr.LogStartup(setupLog)
	}

	if identityProducer {
		ip := &controllers.IdentityProducerReconciler{
			Client:    mgr.GetClient(),
			Cache:     mgr.GetCache(),
			Log:       ctrl.Log.WithName("identity-producer"),
			UploadURL: downloadAPIURL,
			APIKey:    os.Getenv("SYNAPSE_API_KEY"),
			Interval:  identityProducerInterval,
			ClusterID: clusterID,
		}
		if err = mgr.Add(ip); err != nil {
			setupLog.Error(err, "unable to add runnable", "controller", "IdentityProducer")
			os.Exit(1)
		}
		ip.LogStartup(setupLog)
	}

	if edgeProducer {
		ep := &controllers.EdgeProducerReconciler{
			Client:    mgr.GetClient(),
			Cache:     mgr.GetCache(),
			Log:       ctrl.Log.WithName("edge-producer"),
			UploadURL: downloadAPIURL,
			APIKey:    os.Getenv("SYNAPSE_API_KEY"),
			Interval:  edgeProducerInterval,
			ClusterID: clusterID,
		}
		if err = mgr.Add(ep); err != nil {
			setupLog.Error(err, "unable to add runnable", "controller", "EdgeProducer")
			os.Exit(1)
		}
		ep.LogStartup(setupLog)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	readyCheck := healthz.Ping
	if ingressReconciler != nil {
		readyCheck = ingressReconciler.ReadyCheck
	}
	if err := mgr.AddReadyzCheck("readyz", readyCheck); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func parseLabelSelector(value string) (labels.Selector, error) {
	if strings.TrimSpace(value) == "" {
		return labels.Everything(), nil
	}
	return labels.Parse(value)
}

// parseNamespacedName accepts "namespace/name". Returns the zero value
// for an empty input — callers gate on Name == "" to detect "no output
// ConfigMap configured" (sidecar / file-only mode).
// repeatedString collects a flag given more than once.
type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }

func (r *repeatedString) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// parseNamespacedNameList parses each "namespace/name" entry.
func parseNamespacedNameList(vals []string) ([]types.NamespacedName, error) {
	out := make([]types.NamespacedName, 0, len(vals))
	for _, v := range vals {
		nn, err := parseNamespacedName(v)
		if err != nil {
			return nil, err
		}
		if nn.Name == "" {
			return nil, fmt.Errorf("expected namespace/name, got %q", v)
		}
		out = append(out, nn)
	}
	return out, nil
}

// flagWasSet reports whether a flag was given explicitly, so config-sync can
// override a default without clobbering an operator's deliberate choice.
func flagWasSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// parseKeySetOrNil turns a comma-separated list into a set; empty = nil
// (meaning "no restriction").
func parseKeySetOrNil(csv string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, k := range strings.Split(csv, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out[k] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseNamespacedName(value string) (types.NamespacedName, error) {
	if strings.TrimSpace(value) == "" {
		return types.NamespacedName{}, nil
	}
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return types.NamespacedName{}, fmt.Errorf(
			"expected namespace/name, got %q", value)
	}
	return types.NamespacedName{
		Namespace: strings.TrimSpace(parts[0]),
		Name:      strings.TrimSpace(parts[1]),
	}, nil
}

func parseCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseKeySet(value string) map[string]struct{} {
	items := strings.Split(value, ",")
	if len(items) == 0 {
		return nil
	}
	entries := make(map[string]struct{})
	for _, item := range items {
		key := strings.TrimSpace(item)
		if key == "" {
			continue
		}
		entries[key] = struct{}{}
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}
