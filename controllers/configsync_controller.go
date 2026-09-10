package controllers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ConfigSyncReconciler is the "thin reload sidecar": it watches one or more
// ConfigMaps THROUGH THE KUBERNETES API and projects their keys onto a
// shared volume beside a co-located synapse process, then SIGHUPs it.
//
// This exists because a ConfigMap *mount* is not a real-time delivery
// mechanism. kubelet re-projects a mounted ConfigMap on its own sync loop,
// so a write can take ~60s to become visible inside the pod — and in the
// operator's central (--upstreams-out-configmap) mode nothing signals the
// proxy either: SignalReload is disabled when writing to a ConfigMap, and
// upstreams.yaml sits in --ignore-configmap-keys so the config-hash
// controller deliberately does not roll the pods. Delivery therefore
// depends entirely on kubelet propagation.
//
// Watching the API instead collapses that to a single watch event:
//
//	ConfigMap write -> watch event -> write <out-dir>/<key> -> SIGHUP
//
// which lands well inside a second. Synapse itself is untouched: it still
// just reads a file and reloads, exactly as it does today.
//
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
type ConfigSyncReconciler struct {
	client.Client

	// Sources are the ConfigMaps to project (--watch-configmap, repeatable).
	Sources []types.NamespacedName
	// OutDir is the directory keys are written into (--out-dir).
	OutDir string
	// Keys restricts which ConfigMap data keys are projected. Empty = all.
	Keys map[string]struct{}
	// Signaler SIGHUPs the co-located synapse process. nil = never signal
	// (the one-shot prime, where synapse is not running yet).
	Signaler *ReloadSignaler

	sourceSet map[types.NamespacedName]struct{}
	ready     atomic.Bool
}

// wants reports whether a ConfigMap is one this sidecar projects.
func (r *ConfigSyncReconciler) wants(key types.NamespacedName) bool {
	if r.sourceSet == nil {
		r.indexSources()
	}
	_, ok := r.sourceSet[key]
	return ok
}

func (r *ConfigSyncReconciler) indexSources() {
	r.sourceSet = make(map[types.NamespacedName]struct{}, len(r.Sources))
	for _, s := range r.Sources {
		r.sourceSet[s] = struct{}{}
	}
}

// safeKey rejects ConfigMap keys that would escape OutDir. ConfigMap keys
// are attacker-influenceable in a shared cluster and the API server's own
// key validation has been relaxed over time, so this is checked here rather
// than assumed.
func safeKey(k string) bool {
	if k == "" || k == "." || k == ".." {
		return false
	}
	return !strings.ContainsAny(k, `/\`) && !strings.Contains(k, "..")
}

// syncOne projects a single source ConfigMap and returns how many files
// actually changed on disk.
//
// A missing ConfigMap is NOT an error and NEVER prunes existing files. The
// difference matters: leaving the last-good upstreams.yaml in place degrades
// to stale routing, whereas truncating it takes every route down at once.
func (r *ConfigSyncReconciler) syncOne(ctx context.Context, key types.NamespacedName) (int, error) {
	log := ctrl.LoggerFrom(ctx)

	var cm corev1.ConfigMap
	if err := r.Get(ctx, key, &cm); err != nil {
		if errors.IsNotFound(err) {
			mSyncSourceMissing.Inc()
			log.Info("source ConfigMap not found; keeping existing files",
				"configmap", key.String())
			return 0, nil
		}
		return 0, err
	}

	// Deterministic order so logs and tests are stable.
	names := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		names = append(names, k)
	}
	sort.Strings(names)

	changed := 0
	for _, k := range names {
		if len(r.Keys) > 0 {
			if _, ok := r.Keys[k]; !ok {
				continue
			}
		}
		if !safeKey(k) {
			log.Info("skipping unsafe ConfigMap key", "key", k, "configmap", key.String())
			continue
		}
		// Atomic tmp+rename. Safe for synapse's upstreams watcher, whose
		// is_reload_event matches Modify(Name) as well as Modify(Data) —
		// and the SIGHUP below makes the re-read deterministic regardless.
		wrote, err := writeIfChanged(filepath.Join(r.OutDir, k), cm.Data[k])
		if err != nil {
			return changed, err
		}
		if wrote {
			changed++
			log.Info("projected ConfigMap key", "key", k, "configmap", key.String())
		}
	}
	return changed, nil
}

func (r *ConfigSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	mSyncTotal.Inc()
	changed, err := r.syncOne(ctx, req.NamespacedName)
	if err != nil {
		mSyncErrTotal.Inc()
		return ctrl.Result{}, err
	}
	if changed > 0 {
		mSyncChangedTotal.Add(float64(changed))
		if r.Signaler != nil {
			r.Signaler.Signal()
		}
	}
	mSyncLastTS.SetToCurrentTime()
	r.ready.Store(true)
	return ctrl.Result{}, nil
}

// SyncAll projects every configured source once. Used by the startup primer
// and by the one-shot --sync-once initContainer.
func (r *ConfigSyncReconciler) SyncAll(ctx context.Context) (int, error) {
	total := 0
	for _, s := range r.Sources {
		changed, err := r.syncOne(ctx, s)
		total += changed
		if err != nil {
			return total, err
		}
	}
	r.ready.Store(true)
	return total, nil
}

// EnsureFiles guarantees the named files exist, creating a minimal but
// SCHEMA-VALID upstreams document for any that do not.
//
// This is not defensive padding. synapse-proxy's background service does a
// blocking initial read of the upstreams file and, if that read fails,
// logs an error and RETURNS — killing the service outright, so no file
// watch is ever established and no later write is ever picked up. A pod
// that starts before the operator has written the ConfigMap would be
// permanently deaf rather than briefly empty. The rendered floor parses
// (provider defaults to "file" in synapse's serde model), so the watcher
// installs and the first real sync takes over sub-second.
func (r *ConfigSyncReconciler) EnsureFiles(names []string) error {
	floor := renderUpstreams(newRenderModel())
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !safeKey(n) {
			return fmt.Errorf("unsafe --ensure-files entry %q", n)
		}
		path := filepath.Join(r.OutDir, n)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if _, err := writeIfChanged(path, floor); err != nil {
			return fmt.Errorf("ensure %s: %w", path, err)
		}
	}
	return nil
}

// ReadyCheck reports healthy once at least one successful projection pass
// has completed, so the sidecar does not advertise readiness before the
// file it owns exists.
func (r *ConfigSyncReconciler) ReadyCheck(_ *http.Request) error {
	if !r.ready.Load() {
		return fmt.Errorf("config-sync has not completed an initial projection")
	}
	return nil
}

func (r *ConfigSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.indexSources()
	return ctrl.NewControllerManagedBy(mgr).
		// Distinct from the config-hash controller, which also watches
		// ConfigMaps: controller-runtime derives the name from the watched
		// kind, so both would register as "configmap" and the second fails.
		Named("configsync").
		For(&corev1.ConfigMap{}, builder.WithPredicates(
			predicate.NewPredicateFuncs(func(o client.Object) bool {
				return r.wants(types.NamespacedName{
					Namespace: o.GetNamespace(),
					Name:      o.GetName(),
				})
			}),
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// configSyncPrimer projects every source once as soon as the cache has
// synced, so a pod that starts while the ConfigMap is already current does
// not wait for the first watch event.
type configSyncPrimer struct{ r *ConfigSyncReconciler }

// NewConfigSyncPrimer mirrors the ingress controller's render primer.
func NewConfigSyncPrimer(r *ConfigSyncReconciler) manager.Runnable {
	return &configSyncPrimer{r: r}
}

func (p *configSyncPrimer) Start(ctx context.Context) error {
	if _, err := p.r.SyncAll(ctx); err != nil {
		ctrl.Log.WithName("config-sync").Error(err, "initial projection failed")
	}
	return nil
}
