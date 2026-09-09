package controllers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// IngressReconciler is a minimal Kubernetes Ingress controller that
// renders matching Ingresses into a synapse legacy `upstreams.yaml`
// written to a local path. It is designed to run as a SIDECAR in the
// synapse-proxy pod, writing to a shared emptyDir that synapse
// inotify-hot-reloads (~2s) — fast enough to serve cert-manager's
// ephemeral HTTP-01 solver Ingresses, which a ConfigMap mount
// (~60s kubelet propagation) could not.
//
// Backends are addressed by in-cluster DNS
// (svc.namespace.svc.cluster.local:port), so no Service/Endpoint
// watching or ClusterIP resolution is needed. TLS termination certs
// are handled out-of-band (the synapse-proxy mounts the cert-manager
// Secret directly), so this controller only programs L7 routes.
//
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
type IngressReconciler struct {
	client.Client
	// IngressClassName: only Ingresses whose spec.ingressClassName
	// equals this are programmed. cert-manager sets it on its solver
	// Ingress from the issuer's http01.ingress.ingressClassName.
	IngressClassName string
	// UpstreamsOutPath: where the rendered legacy upstreams.yaml is
	// atomically written (the shared emptyDir synapse reads).
	UpstreamsOutPath string
	// CertsOutDir: when set, referenced Ingress/Gateway TLS Secrets
	// are projected here as <stem>.crt/<stem>.key (the operator-owned
	// dir synapse's `certificates` points at; synapse inotify-hot-
	// reloads it). Empty ⇒ multi-cert disabled (legacy static mount).
	CertsOutDir string
	// CertsOutSecret is the central-mode analogue of CertsOutDir: when
	// set, referenced Ingress/Gateway TLS Secrets are projected into this
	// Secret as <stem>.crt/<stem>.key data keys (mirrors
	// UpstreamsOutConfigMap). The separate synapse-proxy pod mounts this
	// Secret as its certificates dir, so certs are operator-owned and
	// auto-wired from Ingress TLS — no hand-maintained projected-volume
	// list. The Secret's Data is replaced on every changed render, so
	// removed certs are pruned. Takes precedence over CertsOutDir.
	CertsOutSecret types.NamespacedName
	// ClusterDomain for backend FQDNs (default cluster.local).
	ClusterDomain string
	// SignalReload: after a changed render, SIGHUP the co-located
	// synapse process to deterministically trigger an upstreams
	// re-read (requires the pod's shareProcessNamespace:true). Set
	// for the long-running sidecar; off for the --render-once
	// initContainer (synapse isn't running yet).
	SignalReload bool
	// GatewayAPI: also reconcile Gateway API (GatewayClass/Gateway/
	// HTTPRoute) into the same upstreams.yaml. Requires the Gateway
	// API CRDs to be installed.
	GatewayAPI bool
	// StatusAddresses, when non-empty, are published on every matched
	// Ingress's .status.loadBalancer.ingress (IP-parseable entries as
	// IP, others as Hostname) after a successful render. Empty ⇒ no
	// status is published (never a bogus address).
	StatusAddresses []string
	// ReloadProcessName is the argv0 basename of the co-located proxy
	// to SIGHUP (default "synapse").
	ReloadProcessName string
	// ReloadDebounce coalesces SIGHUP bursts: leading edge fires
	// immediately, further changes within the window collapse into a
	// single trailing fire so the final state is always applied. 0 ⇒
	// every changed render signals immediately (no debounce).
	ReloadDebounce time.Duration
	// Recorder emits Kubernetes Events on the Ingress/HTTPRoute
	// objects. nil in --render-once mode (no manager).
	Recorder record.EventRecorder
	// IsLeader gates writes to SHARED cluster status (GatewayClass/
	// Gateway/HTTPRoute status, Ingress .status.loadBalancer) so that
	// with >1 proxy replica only one sidecar churns those objects.
	// Per-pod work (render + SIGHUP) is NEVER gated. nil ⇒ always
	// leader (single-writer / --render-once / tests: unchanged).
	IsLeader func() bool
	// UpstreamsOutConfigMap, when set, switches the reconciler from
	// "write upstreams.yaml to UpstreamsOutPath + SIGHUP a co-located
	// synapse process" (sidecar mode) to "write upstreams.yaml to this
	// ConfigMap" (central-controller mode). The ConfigMap is created
	// if missing and the `upstreams.yaml` key is overwritten on every
	// changed render. SignalReload has no effect in central mode —
	// synapse reads through a ConfigMap mount and reloads via its own
	// inotify/poll machinery. Cannot be combined with `UpstreamsOutPath`.
	UpstreamsOutConfigMap types.NamespacedName
	// ResolveBackendClusterIPs swaps the `<svc>.<ns>.svc.<ClusterDomain>`
	// FQDN that `backendAddr` would normally emit for an Ingress
	// backend with the Service's `spec.clusterIP`. Cuts the DNS round-
	// trip that pingora's `HttpPeer::new` does on every connection.
	// Services without a ClusterIP (headless, ExternalName) pass through
	// as FQDNs — synapse-proxy's in-process DNS cache handles them.
	ResolveBackendClusterIPs bool
	// ResolveBackendEndpoints renders the READY pod IPs from EndpointSlices
	// instead of a single Service address, so the proxy load-balances
	// straight to pods and a pod replacement re-renders immediately rather
	// than waiting out synapse's DNS cache TTL. Falls back to ClusterIP/FQDN
	// whenever the endpoint set is missing or empty.
	ResolveBackendEndpoints bool

	ready      atomic.Bool
	reloadOnce sync.Once
	signaler   *ReloadSignaler
}

// usesConfigMapOutput reports whether the reconciler is in central
// mode (writes to a ConfigMap instead of a file).
func (r *IngressReconciler) usesCertsSecretOutput() bool {
	return r.CertsOutSecret.Name != ""
}

func (r *IngressReconciler) usesConfigMapOutput() bool {
	return r.UpstreamsOutConfigMap.Name != ""
}

func (r *IngressReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithName("ingress")
	changed, n, h, err := r.render(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		logger.Info("rendered synapse upstreams",
			"ingresses", n, "hosts", h, "path", r.UpstreamsOutPath)
	}
	return ctrl.Result{}, nil
}

// RenderOnce does a single list+render+write then returns — used by
// the initContainer so synapse starts with the real upstreams.yaml
// already in place (avoids synapse's 2s startup file-watch debounce
// dropping the first dynamic render).
func (r *IngressReconciler) RenderOnce(ctx context.Context) error {
	_, n, h, err := r.render(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("render-once: ingresses=%d hosts=%d -> %s\n", n, h, r.UpstreamsOutPath)
	return nil
}

// render lists all matching Ingresses and rewrites upstreams.yaml.
// Returns (changed, matchedIngresses, hosts, err).
func (r *IngressReconciler) render(ctx context.Context) (bool, int, int, error) {
	mRenderTotal.Inc()
	var list networkingv1.IngressList
	if err := r.List(ctx, &list); err != nil {
		mRenderErrTotal.Inc()
		return false, 0, 0, fmt.Errorf("list ingresses: %w", err)
	}

	// Whole config is rebuilt from scratch every reconcile:
	// idempotent and self-healing. ACME HTTP-01 challenge paths are
	// special-cased into `internal_paths` (see routes.go) because
	// synapse registers a built-in default `/.well-known/
	// acme-challenge/* -> (empty) internal ACME server` matched BEFORE
	// host `upstreams:` routes; cert-manager's ephemeral solver lands
	// there. Per-Ingress annotations configure upstream settings.
	logger := ctrl.LoggerFrom(ctx).WithName("ingress")
	m := newRenderModel()

	// Deterministic source order: Ingresses sorted by ns/name and
	// processed BEFORE HTTPRoutes (first-writer-wins in addRoute), so
	// the rendered config is reproducible regardless of informer
	// ordering and Ingress beats Gateway on a host+path conflict.
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := &list.Items[i], &list.Items[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	defaultOurs := r.defaultClassIsOurs(ctx)

	matched := 0
	var matchedIngs []*networkingv1.Ingress
	for i := range list.Items {
		ing := &list.Items[i]
		if !r.isOurs(ing, defaultOurs) {
			continue
		}
		matched++
		matchedIngs = append(matchedIngs, ing)
		a := parseAnnotations(ing.Annotations)
		if a.sticky {
			m.sticky = true
		}
		// spec.tls → project each Secret. Empty Hosts ⇒ the cert
		// applies to this Ingress's rule hosts (Kubernetes semantics).
		for _, t := range ing.Spec.TLS {
			if t.SecretName == "" {
				continue
			}
			hosts := t.Hosts
			if len(hosts) == 0 {
				for _, rl := range ing.Spec.Rules {
					if rl.Host != "" {
						hosts = append(hosts, rl.Host)
					}
				}
			}
			if len(hosts) == 0 {
				stem, _ := certStem("", ing.Namespace, t.SecretName)
				m.addCert("", stem, ing.Namespace, t.SecretName)
				continue
			}
			for _, h := range hosts {
				if stem, bound := certStem(h, ing.Namespace, t.SecretName); bound {
					m.addCert(h, stem, ing.Namespace, t.SecretName)
				} else {
					m.addCert("", stem, ing.Namespace, t.SecretName)
				}
			}
		}
		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" || rule.HTTP == nil {
				continue
			}
			// ssl-passthrough: synapse routes the raw TLS stream to a
			// single TCP upstream (synapse sees no L7). Path-level
			// settings on the Ingress are ignored — passthrough is
			// SNI-only. Use the first resolvable path's backend as the
			// passthrough target; warn on conflicts.
			if a.passthrough {
				var ptAddr string
				for _, p := range rule.HTTP.Paths {
					if addr, ok := r.backendAddr(ctx, ing.Namespace, p.Backend); ok {
						ptAddr = addr
						break
					}
				}
				if ptAddr == "" {
					mBackendUnresolved.Inc()
					r.emit(ing, corev1.EventTypeWarning, "BackendUnresolved",
						"Ingress backend on passthrough host %q could not be resolved", host)
					continue
				}
				if !m.addPassthroughHost(host, ptAddr) {
					logger.Info("passthrough host claim ignored (first-writer-wins)",
						"host", host, "ingress", ing.Namespace+"/"+ing.Name)
					mRouteConflicts.Inc()
					r.emit(ing, corev1.EventTypeWarning, "RouteConflict",
						"passthrough host %s already claimed (first-writer-wins, terminate route or earlier passthrough); this rule is ignored", host)
				}
				continue
			}
			for _, p := range rule.HTTP.Paths {
				servers, ok := r.backendServers(ctx, ing.Namespace, p.Backend)
				if !ok {
					mBackendUnresolved.Inc()
					r.emit(ing, corev1.EventTypeWarning, "BackendUnresolved",
						"Ingress backend on host %q could not be resolved (no Service, or named port not found)", host)
					continue
				}
				path := p.Path
				if path == "" {
					path = "/"
				}
				// nginx `use-regex: "true"`: the path is a POSIX regex →
				// render a synapse match_expr regex route (under the primary host
				// and any server-aliases). Otherwise fall through to prefix/Exact.
				if a.useRegex {
					if !m.addRegexRoute(host, path, servers, a, nil, nil) {
						logger.Info("regex route conflict ignored (first-writer-wins)",
							"host", host, "regex", path, "ingress", ing.Namespace+"/"+ing.Name)
						mRouteConflicts.Inc()
						r.emit(ing, corev1.EventTypeWarning, "RouteConflict",
							"host %s regex %s already programmed by an earlier source (first-writer-wins); this rule is ignored", host, path)
					}
					for _, alias := range a.serverAliases {
						if alias == "" || alias == host {
							continue
						}
						if !m.addRegexRoute(alias, path, servers, a, nil, nil) {
							logger.Info("regex route conflict ignored on server-alias (first-writer-wins)",
								"host", alias, "regex", path, "ingress", ing.Namespace+"/"+ing.Name, "primary_host", host)
							mRouteConflicts.Inc()
							r.emit(ing, corev1.EventTypeWarning, "RouteConflict",
								"server-alias %s regex %s already programmed by an earlier source (first-writer-wins); this alias is ignored", alias, path)
						}
					}
					continue
				}
				if p.PathType != nil && *p.PathType == networkingv1.PathTypeExact {
					logger.Info("Ingress Exact pathType is approximated as a prefix (synapse v1 matches longest-prefix)",
						"ingress", ing.Namespace+"/"+ing.Name, "host", host, "path", path)
					mUnsupportedMatch.Inc()
					r.emit(ing, corev1.EventTypeWarning, "UnsupportedMatch",
						"Exact pathType on host %s path %s is approximated as a prefix (synapse v1 matches longest-prefix)", host, path)
				}
				if strings.HasPrefix(path, acmeChallengePrefix) {
					// m.acme is a single address, so the solver keeps
					// Service addressing rather than pinning one pod.
					if m.acme == "" {
						if a, ok := r.backendAddr(ctx, ing.Namespace, p.Backend); ok {
							m.acme = a
						}
					}
					continue
				}
				if !m.addRoute(host, path, servers, a, nil, nil) {
					logger.Info("route conflict ignored (first-writer-wins)",
						"host", host, "path", path, "ingress", ing.Namespace+"/"+ing.Name)
					mRouteConflicts.Inc()
					r.emit(ing, corev1.EventTypeWarning, "RouteConflict",
						"host %s path %s already programmed by an earlier source (first-writer-wins); this rule is ignored", host, path)
				}
				// server-alias: program the same route under each alias host.
				// Same FIRST-WRITER-WINS semantics on conflict; same backend
				// and annotation-derived settings as the primary host.
				for _, alias := range a.serverAliases {
					if alias == "" || alias == host {
						continue
					}
					if !m.addRoute(alias, path, servers, a, nil, nil) {
						logger.Info("route conflict ignored on server-alias (first-writer-wins)",
							"host", alias, "path", path, "ingress", ing.Namespace+"/"+ing.Name, "primary_host", host)
						mRouteConflicts.Inc()
						r.emit(ing, corev1.EventTypeWarning, "RouteConflict",
							"server-alias %s path %s already programmed by an earlier source (first-writer-wins); this alias is ignored", alias, path)
					}
				}
			}
		}
	}

	if r.GatewayAPI {
		matched += r.renderGateways(ctx, m)
	}

	// Project referenced TLS Secrets. Central mode (CertsOutSecret set)
	// writes them into a Secret the separate proxy pod mounts; sidecar
	// mode writes <stem>.crt/<stem>.key into the shared CertsOutDir. Both
	// are per-pod, never leader-gated (mirrors the upstreams file-vs-CM
	// split); synapse inotify-hot-reloads certs independently of SIGHUP.
	if r.usesCertsSecretOutput() {
		if _, _, cerr := r.projectCertsToSecret(ctx, m); cerr != nil {
			mRenderErrTotal.Inc()
			return false, matched, len(m.hosts), fmt.Errorf("project certs to secret: %w", cerr)
		}
	} else if _, _, cerr := r.projectCerts(ctx, m); cerr != nil {
		mRenderErrTotal.Inc()
		return false, matched, len(m.hosts), fmt.Errorf("project certs: %w", cerr)
	}

	hosts, routes := 0, 0
	for _, paths := range m.hosts {
		hosts++
		routes += len(paths)
	}
	hosts += len(m.passthroughHosts)

	// Schema selection: any SNI passthrough host forces v2 emission
	// (the v1 schema has no passthrough representation). Otherwise
	// keep emitting v1 so unrelated deployments see zero behaviour
	// change.
	var yaml string
	if len(m.passthroughHosts) > 0 {
		yaml = renderUpstreamsV2(m)
	} else {
		yaml = renderUpstreams(m)
	}
	var (
		changed bool
		err     error
	)
	if r.usesConfigMapOutput() {
		changed, err = r.writeConfigMapIfChanged(ctx, yaml)
		if err != nil {
			mRenderErrTotal.Inc()
			return false, matched, hosts, fmt.Errorf("write configmap %s/%s: %w",
				r.UpstreamsOutConfigMap.Namespace, r.UpstreamsOutConfigMap.Name, err)
		}
	} else {
		changed, err = writeIfChanged(r.UpstreamsOutPath, yaml)
		if err != nil {
			mRenderErrTotal.Inc()
			return false, matched, hosts, fmt.Errorf("write %s: %w", r.UpstreamsOutPath, err)
		}
	}

	mHosts.Set(float64(hosts))
	mRoutes.Set(float64(routes))
	mLastRenderTS.SetToCurrentTime()
	if changed {
		mRenderChangedTotal.Inc()
		// SignalReload only makes sense in sidecar mode — central mode
		// has no co-located synapse process to signal. The proxy that
		// mounts the rendered ConfigMap reloads via its own machinery.
		if r.SignalReload && !r.usesConfigMapOutput() {
			r.signalReload(ctx)
		}
		for _, ing := range matchedIngs {
			r.emit(ing, corev1.EventTypeNormal, "Programmed",
				"programmed into synapse upstreams (%d hosts, %d routes)", hosts, routes)
		}
	}
	r.publishStatus(ctx, matchedIngs)

	if r.ready.CompareAndSwap(false, true) {
		mReady.Set(1)
	}
	return changed, matched, hosts, nil
}

// isOurs reports whether this Ingress is served by us. Precedence
// follows Kubernetes: an explicit spec.ingressClassName wins (must
// equal ours); otherwise the legacy kubernetes.io/ingress.class
// annotation is honored; otherwise it falls to the default
// IngressClass (defaultOurs, resolved once per render).
func (r *IngressReconciler) isOurs(ing *networkingv1.Ingress, defaultOurs bool) bool {
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName == r.IngressClassName
	}
	if v := strings.TrimSpace(ing.Annotations["kubernetes.io/ingress.class"]); v != "" {
		return v == r.IngressClassName
	}
	return defaultOurs
}

// defaultClassIsOurs is true when an IngressClass annotated
// ingressclass.kubernetes.io/is-default-class=true is controlled by
// us (spec.controller == ControllerName) — so Ingresses with neither
// spec.ingressClassName nor the legacy annotation are ours.
func (r *IngressReconciler) defaultClassIsOurs(ctx context.Context) bool {
	var icl networkingv1.IngressClassList
	if err := r.List(ctx, &icl); err != nil {
		return false
	}
	for i := range icl.Items {
		ic := &icl.Items[i]
		if ic.Spec.Controller != ControllerName {
			continue
		}
		if ic.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true" {
			return true
		}
	}
	return false
}

// leader reports whether this instance may write shared cluster
// status. nil gate ⇒ always leader (unchanged single-writer behavior).
func (r *IngressReconciler) leader() bool {
	return r.IsLeader == nil || r.IsLeader()
}

// emit records a Kubernetes Event on obj (no-op when there is no
// recorder, e.g. --render-once).
func (r *IngressReconciler) emit(obj runtime.Object, etype, reason, msgFmt string, args ...any) {
	if r.Recorder == nil || obj == nil {
		return
	}
	r.Recorder.Eventf(obj, etype, reason, msgFmt, args...)
}

// publishStatus writes StatusAddresses onto each matched Ingress's
// .status.loadBalancer.ingress (idempotent: only patches on change).
// No-op when StatusAddresses is empty.
func (r *IngressReconciler) publishStatus(ctx context.Context, ings []*networkingv1.Ingress) {
	if len(r.StatusAddresses) == 0 || !r.leader() {
		return
	}
	want := make([]networkingv1.IngressLoadBalancerIngress, 0, len(r.StatusAddresses))
	for _, a := range r.StatusAddresses {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if net.ParseIP(a) != nil {
			want = append(want, networkingv1.IngressLoadBalancerIngress{IP: a})
		} else {
			want = append(want, networkingv1.IngressLoadBalancerIngress{Hostname: a})
		}
	}
	logger := ctrl.LoggerFrom(ctx).WithName("ingress")
	for _, ing := range ings {
		if apiequality.Semantic.DeepEqual(ing.Status.LoadBalancer.Ingress, want) {
			continue
		}
		ing.Status.LoadBalancer.Ingress = want
		if err := r.Status().Update(ctx, ing); err != nil {
			logger.Error(err, "publish Ingress status", "ingress", ing.Namespace+"/"+ing.Name)
		}
	}
}

// backendAddr resolves an Ingress backend to a `host:port` string. The
// `host` portion is the Service's `spec.clusterIP` when
// `ResolveBackendClusterIPs` is set and a ClusterIP is available;
// otherwise it falls back to the in-cluster FQDN
// (`<svc>.<ns>.svc.<ClusterDomain>`). Port-by-name is resolved via a
// Service lookup; port-by-number is used directly.
func (r *IngressReconciler) backendAddr(ctx context.Context, ns string, b networkingv1.IngressBackend) (string, bool) {
	if b.Service == nil {
		return "", false
	}
	cd := r.ClusterDomain
	if cd == "" {
		cd = "cluster.local"
	}
	fqdn := fmt.Sprintf("%s.%s.svc.%s", b.Service.Name, ns, cd)

	// Whether we need a Service lookup at all: any of port-by-name or
	// ResolveBackendClusterIPs forces it. Port-by-number with no
	// resolution required can skip the API hit.
	needLookup := b.Service.Port.Name != "" || r.ResolveBackendClusterIPs
	var svc corev1.Service
	if needLookup {
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: b.Service.Name}, &svc); err != nil {
			// Service missing: port-by-name can't be resolved at all;
			// port-by-number can fall back to the FQDN. Either way,
			// no ClusterIP substitution is possible.
			if b.Service.Port.Number != 0 {
				return fmt.Sprintf("%s:%d", fqdn, b.Service.Port.Number), true
			}
			return "", false
		}
	}

	host := fqdn
	if r.ResolveBackendClusterIPs {
		// "None" is the headless-Service sentinel; "" is also possible
		// for ExternalName / not-yet-allocated. In both cases stick
		// with the FQDN — synapse-proxy's in-process DNS cache will
		// take it from there.
		if ip := svc.Spec.ClusterIP; ip != "" && ip != corev1.ClusterIPNone {
			host = ip
		}
	}

	if b.Service.Port.Number != 0 {
		return fmt.Sprintf("%s:%d", host, b.Service.Port.Number), true
	}
	if b.Service.Port.Name != "" {
		for _, sp := range svc.Spec.Ports {
			if sp.Name == b.Service.Port.Name {
				return fmt.Sprintf("%s:%d", host, sp.Port), true
			}
		}
	}
	return "", false
}

// acmeChallengePrefix is the ACME HTTP-01 challenge path prefix.
// cert-manager's solver Ingress uses /.well-known/acme-challenge/<token>.
const acmeChallengePrefix = "/.well-known/acme-challenge"

// writeIfChanged writes ATOMICALLY (tmp file in the same dir +
// rename), so a concurrent synapse read can never observe a torn or
// empty file. Atomic is safe here: synapse's upstreams filewatch
// (synapse-utils filewatch.rs `is_reload_event`) matches
// Modify(Name) alongside Modify(Data)/Create/Remove, so a rename IS
// seen. (The CERT watcher is the one that ignores renames — see
// writeFileIfChanged in certs.go.) The reload is additionally driven
// explicitly by SIGHUP (see signalReload), which makes the re-read
// deterministic and bypasses the watcher's content-digest gate.
func writeIfChanged(path, content string) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// writeConfigMapIfChanged is the central-mode analogue of
// writeIfChanged: the rendered upstreams.yaml goes to the
// `upstreams.yaml` key of `r.UpstreamsOutConfigMap`. Returns
// (changed, error) — false-without-error when the existing value
// already matches the rendered content. The ConfigMap is created if
// missing and labelled so callers (and humans) can identify the
// operator-managed output at a glance.
func (r *IngressReconciler) writeConfigMapIfChanged(ctx context.Context, content string) (bool, error) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.UpstreamsOutConfigMap.Name,
			Namespace: r.UpstreamsOutConfigMap.Namespace,
		},
	}
	var changed bool
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[ingressUpstreamsManagedLabel] = "true"
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if cur, ok := cm.Data[UpstreamsKey]; ok && cur == content {
			return nil
		}
		cm.Data[UpstreamsKey] = content
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	// CreateOrUpdate reports Created/Updated/Unchanged; mirror that
	// into `changed` for the metrics + reload flow upstream.
	switch op {
	case controllerutil.OperationResultCreated:
		changed = true
	case controllerutil.OperationResultUpdated:
		// changed was already set inside the mutate fn
	}
	return changed, nil
}

// ingressUpstreamsManagedLabel marks a ConfigMap as written by the
// central-mode IngressReconciler. Mirrors `ResolvedOutputLabel` from
// upstreams_resolver.go so the two operator writers are distinguishable.
const ingressUpstreamsManagedLabel = "synapse.gen0sec.com/ingress-upstreams"

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Any Ingress change triggers a full rebuild; a single dummy
	// request key is enough (Reconcile ignores req and lists all).
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "rebuild"}}}
	})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("synapse-ingress").
		Watches(&networkingv1.Ingress{}, enqueueAll).
		Watches(&networkingv1.IngressClass{}, enqueueAll).
		// Backends are DNS-addressed, but a Service add/delete or a
		// named-port change must re-render (named ports are resolved
		// via a Service lookup).
		Watches(&corev1.Service{}, enqueueAll)

	if r.ResolveBackendEndpoints {
		// Gated: on a large cluster the EndpointSlice informer is the
		// biggest object in the process, and the RBAC for it does not exist
		// unless this mode is deliberately enabled.
		b = b.Watches(&discoveryv1.EndpointSlice{}, enqueueAll)
	}
	if r.CertsOutDir != "" || r.usesCertsSecretOutput() {
		// Re-project on TLS Secret changes (cert rotation/renewal)
		// without waiting for an unrelated Ingress event. Filtered
		// to kubernetes.io/tls to avoid watching every Secret.
		b = b.Watches(&corev1.Secret{}, enqueueAll, builder.WithPredicates(
			predicate.NewPredicateFuncs(func(o client.Object) bool {
				s, ok := o.(*corev1.Secret)
				return ok && s.Type == corev1.SecretTypeTLS
			})))
	}
	if r.GatewayAPI {
		b = b.Watches(gwHTTPRoute(), enqueueAll).
			Watches(gwGateway(), enqueueAll).
			Watches(gwGatewayClass(), enqueueAll)
	}
	return b.Complete(r)
}

// ReadyCheck gates readyz on the first successful render so the proxy
// is not advertised as ready before synapse has correct upstreams.
func (r *IngressReconciler) ReadyCheck(*http.Request) error {
	if r.ready.Load() {
		return nil
	}
	return errors.New("initial upstreams render not complete")
}

// renderPrimer is a manager Runnable that performs one render after
// the caches have synced (controller-runtime starts non-cache
// Runnables only post-sync), so readyz flips even on a cluster with
// zero Ingresses and the file is primed before traffic is admitted.
type renderPrimer struct{ r *IngressReconciler }

// NewRenderPrimer wraps an IngressReconciler as a manager Runnable.
func NewRenderPrimer(r *IngressReconciler) ctrlManagerRunnable { return renderPrimer{r: r} }

// ctrlManagerRunnable mirrors sigs.k8s.io/controller-runtime
// manager.Runnable without importing it here.
type ctrlManagerRunnable interface {
	Start(context.Context) error
}

func (p renderPrimer) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("ingress")
	if _, _, _, err := p.r.render(ctx); err != nil {
		// Don't crash the manager; the controller's reconciles retry.
		log.Error(err, "initial render failed (will retry on reconcile)")
		return nil
	}
	log.Info("initial upstreams render complete; ready")
	return nil
}

// LogStartup is a tiny helper so main can announce the mode.
func (r *IngressReconciler) LogStartup(log logr.Logger) {
	log.Info("synapse ingress controller",
		"ingressClass", r.IngressClassName, "out", r.UpstreamsOutPath)
}
