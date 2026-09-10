package controllers

import (
	"testing"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Both reconcilers watch ConfigMaps. controller-runtime derives a controller's
// name from the watched kind unless told otherwise, so without an explicit
// Named() the second registration fails with "controller with name configmap
// already exists" — which crashlooped the sidecar in a live cluster and was
// invisible to every unit test, because none of them built a real manager.
func TestConfigSyncAndConfigHashControllersDoNotCollide(t *testing.T) {
	mgr, err := manager.New(&rest.Config{Host: "http://127.0.0.1:1"}, manager.Options{
		Scheme:  testScheme(t),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Skipf("manager unavailable in this environment: %v", err)
	}

	if err := (&ConfigMapReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		ConfigHashAnnotation: "x",
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("config-hash controller failed to register: %v", err)
	}

	cs := &ConfigSyncReconciler{Client: mgr.GetClient(), OutDir: t.TempDir()}
	if err := cs.SetupWithManager(mgr); err != nil {
		t.Fatalf("config-sync controller must register alongside config-hash: %v", err)
	}
	_ = ctrl.Log
}
