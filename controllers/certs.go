package controllers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ingressCertsManagedLabel marks a Secret as written by the ingress
// controller's central-mode cert projection. Mirrors
// ingressUpstreamsManagedLabel so the operator's writers are
// distinguishable.
const ingressCertsManagedLabel = "synapse.gen0sec.com/ingress-certs"

// Multi-cert TLS: the operator projects each referenced Kubernetes TLS
// Secret (Ingress spec.tls / Gateway listener certificateRefs) into
// synapse's certificates directory as <stem>.crt / <stem>.key.
//
// synapse scans that dir for <stem>.crt+<stem>.key pairs and inotify-
// hot-reloads on change (acme disabled ⇒ no SIGHUP / restart needed;
// the cert watcher is independent of the upstreams reload). SNI then
// resolves via upstreams_cert_map (the per-host `certificate:` we also
// emit) then name_map exact (<host>.crt file-stem wins) then wildcard
// (cert SAN) then default. The dir is OPERATOR-OWNED: files no longer
// backed by a Secret are pruned, so cert rotation/removal converges
// with no pod restart. Projection is per-pod (never leader-gated, like
// the upstreams render).

// certStem picks the file-stem (== synapse cert name) for a host's
// Secret. A concrete host binds 1:1 (enables the strong name_map
// exact-match + upstreams_cert_map paths). Wildcard / no-host certs
// get a Secret-derived stem and rely on synapse's wildcard SAN match
// (host == "" ⇒ caller must not set a per-host upstreams binding).
func certStem(host, ns, secret string) (stem string, hostBound bool) {
	if host != "" && !strings.HasPrefix(host, "*.") {
		return sanitizeStem(host), true
	}
	return sanitizeStem(ns + "-" + secret), false
}

func sanitizeStem(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// projectCerts materializes m.certProjections into CertsOutDir and
// prunes operator-managed cert files no longer referenced. Returns
// (changed, projected, err). CertsOutDir == "" disables multi-cert
// (legacy single static-mount behavior). A missing / non-TLS Secret
// is skipped (logged + metric) without failing the whole render.
func (r *IngressReconciler) projectCerts(ctx context.Context, m *renderModel) (bool, int, error) {
	if r.CertsOutDir == "" {
		return false, 0, nil
	}
	logger := ctrl.LoggerFrom(ctx).WithName("certs")
	if err := os.MkdirAll(r.CertsOutDir, 0o755); err != nil {
		return false, 0, fmt.Errorf("certs dir %s: %w", r.CertsOutDir, err)
	}

	// Deterministic order so logs/behavior are reproducible.
	stems := make([]string, 0, len(m.certProjections))
	for s := range m.certProjections {
		stems = append(stems, s)
	}
	sort.Strings(stems)

	changed := false
	want := map[string]struct{}{} // stems successfully projected
	for _, stem := range stems {
		cp := m.certProjections[stem]
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: cp.ns, Name: cp.name}, &sec); err != nil {
			logger.Info("referenced TLS Secret unavailable; skipping cert (host will fall back to default)",
				"secret", cp.ns+"/"+cp.name, "stem", stem, "err", err.Error())
			mCertErrors.Inc()
			continue
		}
		crt := sec.Data[corev1.TLSCertKey]       // "tls.crt"
		key := sec.Data[corev1.TLSPrivateKeyKey] // "tls.key"
		if len(crt) == 0 || len(key) == 0 {
			logger.Info("referenced Secret is not a usable TLS Secret (missing tls.crt/tls.key); skipping",
				"secret", cp.ns+"/"+cp.name, "type", string(sec.Type), "stem", stem)
			mCertErrors.Inc()
			continue
		}
		// Write key BEFORE crt: synapse's dir scan keys on <stem>.crt
		// and only pairs when <stem>.key already exists, so the crt
		// write is the last step that makes the pair loadable.
		kch, err := writeFileIfChanged(filepath.Join(r.CertsOutDir, stem+".key"), key, 0o600)
		if err != nil {
			return changed, len(want), fmt.Errorf("write %s.key: %w", stem, err)
		}
		cch, err := writeFileIfChanged(filepath.Join(r.CertsOutDir, stem+".crt"), crt, 0o644)
		if err != nil {
			return changed, len(want), fmt.Errorf("write %s.crt: %w", stem, err)
		}
		if kch || cch {
			changed = true
			logger.Info("projected TLS cert", "stem", stem, "secret", cp.ns+"/"+cp.name)
		}
		want[stem] = struct{}{}
	}

	if pruned := pruneCerts(r.CertsOutDir, want); pruned > 0 {
		changed = true
		logger.Info("pruned stale operator-managed cert files", "count", pruned)
	}
	mCerts.Set(float64(len(want)))
	return changed, len(want), nil
}

// projectCertsToSecret is the central-mode analogue of projectCerts: it
// materializes m.certProjections into the CertsOutSecret Secret as
// <stem>.crt/<stem>.key data keys, which the separate synapse-proxy pod
// mounts as its certificates dir. The Secret's Data is replaced wholesale
// every render, so stems no longer referenced are pruned automatically (no
// separate prune pass like the dir mode). A missing / non-TLS source
// Secret is skipped (logged + metric) without failing the render.
// CertsOutSecret unset ⇒ no-op. Returns (changed, projected, err).
func (r *IngressReconciler) projectCertsToSecret(ctx context.Context, m *renderModel) (bool, int, error) {
	if r.CertsOutSecret.Name == "" {
		return false, 0, nil
	}
	logger := ctrl.LoggerFrom(ctx).WithName("certs")

	// Deterministic order so logs are reproducible.
	stems := make([]string, 0, len(m.certProjections))
	for s := range m.certProjections {
		stems = append(stems, s)
	}
	sort.Strings(stems)

	data := map[string][]byte{}
	for _, stem := range stems {
		cp := m.certProjections[stem]
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: cp.ns, Name: cp.name}, &sec); err != nil {
			logger.Info("referenced TLS Secret unavailable; skipping cert (host will fall back to default)",
				"secret", cp.ns+"/"+cp.name, "stem", stem, "err", err.Error())
			mCertErrors.Inc()
			continue
		}
		crt := sec.Data[corev1.TLSCertKey]       // "tls.crt"
		key := sec.Data[corev1.TLSPrivateKeyKey] // "tls.key"
		if len(crt) == 0 || len(key) == 0 {
			logger.Info("referenced Secret is not a usable TLS Secret (missing tls.crt/tls.key); skipping",
				"secret", cp.ns+"/"+cp.name, "type", string(sec.Type), "stem", stem)
			mCertErrors.Inc()
			continue
		}
		data[stem+".crt"] = crt
		data[stem+".key"] = key
	}

	out := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      r.CertsOutSecret.Name,
		Namespace: r.CertsOutSecret.Namespace,
	}}
	var changed bool
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, out, func() error {
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		out.Labels[ingressCertsManagedLabel] = "true"
		out.Type = corev1.SecretTypeOpaque
		if !bytesMapEqual(out.Data, data) {
			out.Data = data
			changed = true
		}
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	if op == controllerutil.OperationResultCreated {
		changed = true
	}
	n := len(data) / 2
	if changed {
		logger.Info("projected TLS certs to secret",
			"secret", r.CertsOutSecret.Namespace+"/"+r.CertsOutSecret.Name, "certs", n)
	}
	mCerts.Set(float64(n))
	return changed, n, nil
}

// bytesMapEqual reports whether two map[string][]byte have identical
// keys and values.
func bytesMapEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		if vb, ok := b[k]; !ok || !bytes.Equal(va, vb) {
			return false
		}
	}
	return true
}

// writeFileIfChanged writes content IN PLACE (no tmp+rename) only when
// it differs. In-place is REQUIRED here, not a shortcut: synapse's
// cert watcher (synapse-utils tools.rs `watch_folder`) only re-scans on
// inotify Create / Modify(Data) / Remove and IGNORES rename/MOVED_TO.
// (The UPSTREAMS filewatch does NOT share this limitation — its
// is_reload_event matches Modify(Name) too, which is why
// writeIfChanged in ingress_controller.go can write atomically.)
// An atomic
// tmp+rename lands as Modify(Name) and would be invisible at runtime —
// the cert would never load until a process restart. A plain create
// fires Create; an overwrite fires Modify(Data); a prune fires Remove
// — all honored. Partial-write safety is the watcher's own job: it
// sleeps 500ms after an event before re-scanning ("so both .crt and
// .key are fully written") and only pairs files that parse, so a torn
// read is simply retried on the next event while existing SslContexts
// keep serving (no 502 — unlike the upstreams torn-read case). Writing
// only on a real content change avoids needless inotify churn /
// Certificates rebuilds.
func writeFileIfChanged(path string, content []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(content) {
		return false, nil
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return false, err
	}
	return true, nil
}

// pruneCerts removes <stem>.crt/<stem>.key (and stray .tmp) for stems
// not in want — the cert dir is operator-owned, so a deleted Ingress/
// Secret converges to no cert (SNI then falls back to default).
func pruneCerts(dir string, want map[string]struct{}) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		n := e.Name()
		var stem string
		switch {
		case strings.HasSuffix(n, ".crt"):
			stem = strings.TrimSuffix(n, ".crt")
		case strings.HasSuffix(n, ".key"):
			stem = strings.TrimSuffix(n, ".key")
		case strings.HasSuffix(n, ".tmp"):
			if err := os.Remove(filepath.Join(dir, n)); err == nil {
				removed++
			}
			continue
		default:
			continue
		}
		if _, ok := want[stem]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, n)); err == nil {
			removed++
		}
	}
	return removed
}
