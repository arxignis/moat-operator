package controllers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newConfigSync(t *testing.T, dir string, objs ...client.Object) *ConfigSyncReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	r := &ConfigSyncReconciler{
		Client:  c,
		Sources: []types.NamespacedName{{Namespace: "synapse-os", Name: "synapse-proxy"}},
		OutDir:  dir,
	}
	r.indexSources()
	return r
}

func proxyCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "synapse-os", Name: "synapse-proxy"},
		Data:       data,
	}
}

func TestConfigSync_ProjectsEveryKeyByDefault(t *testing.T) {
	dir := t.TempDir()
	r := newConfigSync(t, dir, proxyCM(map[string]string{
		"upstreams.yaml": "upstreams:\n",
		"config.yaml":    "mode: proxy\n",
	}))

	changed, err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}
	for name, want := range map[string]string{
		"upstreams.yaml": "upstreams:\n",
		"config.yaml":    "mode: proxy\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestConfigSync_KeyFilterProjectsOnlyRequestedKeys(t *testing.T) {
	dir := t.TempDir()
	r := newConfigSync(t, dir, proxyCM(map[string]string{
		"upstreams.yaml": "upstreams:\n",
		"config.yaml":    "mode: proxy\n",
	}))
	r.Keys = map[string]struct{}{"upstreams.yaml": {}}

	if _, err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("config.yaml should not have been projected (err=%v)", err)
	}
}

// An unchanged ConfigMap must not signal a reload. Without this the sidecar
// would SIGHUP synapse on every resync, and once endpoint-driven renders
// make reconciles frequent that becomes a continuous reload storm.
func TestConfigSync_UnchangedProjectionDoesNotSignal(t *testing.T) {
	dir := t.TempDir()
	r := newConfigSync(t, dir, proxyCM(map[string]string{"upstreams.yaml": "upstreams:\n"}))

	signals := 0
	r.Signaler = &ReloadSignaler{Debounce: 0}
	r.Signaler.once.Do(func() {
		r.Signaler.d = newReloadDebouncer(0, func() { signals++ })
	})

	req := ctrl.Request{NamespacedName: r.Sources[0]}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if signals != 1 {
		t.Fatalf("signals = %d, want 1 (only the first pass changes anything)", signals)
	}
}

// A deleted or not-yet-created ConfigMap must leave the last-good file in
// place. Truncating turns "stale routes" into "every route is gone".
func TestConfigSync_MissingConfigMapKeepsExistingFileAndSucceeds(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "upstreams.yaml")
	if err := os.WriteFile(existing, []byte("upstreams:\n  a: {}\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := newConfigSync(t, dir) // no ConfigMap objects at all

	changed, err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("a missing source must not be an error, got %v", err)
	}
	if changed != 0 {
		t.Fatalf("changed = %d, want 0", changed)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "upstreams:\n  a: {}\n" {
		t.Errorf("existing file was modified: %q", got)
	}
}

// synapse-proxy aborts its background service (and never installs a file
// watch) if the initial upstreams read fails, so the floor must exist and
// must parse.
func TestConfigSync_EnsureFilesCreatesAValidFloor(t *testing.T) {
	dir := t.TempDir()
	r := newConfigSync(t, dir)

	if err := r.EnsureFiles([]string{"upstreams.yaml"}); err != nil {
		t.Fatalf("EnsureFiles: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "upstreams.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := renderUpstreams(newRenderModel()); string(got) != want {
		t.Errorf("floor = %q, want %q", got, want)
	}
}

func TestConfigSync_EnsureFilesDoesNotClobberRealContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upstreams.yaml")
	if err := os.WriteFile(path, []byte("real content\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := newConfigSync(t, dir)

	if err := r.EnsureFiles([]string{"upstreams.yaml"}); err != nil {
		t.Fatalf("EnsureFiles: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "real content\n" {
		t.Errorf("EnsureFiles overwrote existing content: %q", got)
	}
}

func TestConfigSync_SourcePredicateAcceptsOnlyListedConfigMaps(t *testing.T) {
	r := newConfigSync(t, t.TempDir())
	if !r.wants(types.NamespacedName{Namespace: "synapse-os", Name: "synapse-proxy"}) {
		t.Error("listed source rejected")
	}
	for _, bad := range []types.NamespacedName{
		{Namespace: "synapse-os", Name: "synapse-agent"},
		{Namespace: "other", Name: "synapse-proxy"},
	} {
		if r.wants(bad) {
			t.Errorf("unlisted source %v accepted", bad)
		}
	}
}

// ConfigMap keys are attacker-influenceable in a shared cluster; a key
// containing a separator must never escape OutDir.
func TestConfigSync_RejectsPathTraversalKeys(t *testing.T) {
	for _, k := range []string{"../evil", "a/b", "..", ".", "", `a\b`, "x..y"} {
		if safeKey(k) {
			t.Errorf("safeKey(%q) = true, want false", k)
		}
	}
	for _, k := range []string{"upstreams.yaml", "config.yaml", "a.b.c", "_x-1"} {
		if !safeKey(k) {
			t.Errorf("safeKey(%q) = false, want true", k)
		}
	}
}

func TestConfigSync_WriteIsAtomicAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	r := newConfigSync(t, dir, proxyCM(map[string]string{"upstreams.yaml": "upstreams:\n"}))

	if _, err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestConfigSync_ReadyOnlyAfterFirstProjection(t *testing.T) {
	dir := t.TempDir()
	r := newConfigSync(t, dir, proxyCM(map[string]string{"upstreams.yaml": "upstreams:\n"}))

	if err := r.ReadyCheck(nil); err == nil {
		t.Error("ReadyCheck should fail before the first projection")
	}
	if _, err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := r.ReadyCheck(nil); err != nil {
		t.Errorf("ReadyCheck should pass after a projection: %v", err)
	}
}
