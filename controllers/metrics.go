package controllers

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Prometheus metrics for the Ingress/Gateway controller, registered on
// controller-runtime's shared Registry so they are exposed on the same
// --metrics-bind-address endpoint as the built-in controller metrics.
var (
	mRenderTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_render_total",
		Help: "Total upstreams render passes attempted.",
	})
	mRenderErrTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_render_errors_total",
		Help: "Total upstreams render passes that failed (list or write error).",
	})
	mRenderChangedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_render_changed_total",
		Help: "Total renders that produced a changed upstreams.yaml.",
	})
	mReloadTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_reload_signals_total",
		Help: "Total SIGHUP reload signals delivered to the synapse process.",
	})
	// config-sync sidecar (--config-sync)
	mSyncTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_config_sync_total",
		Help: "Total config-sync projection passes attempted.",
	})
	mSyncChangedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_config_sync_changed_total",
		Help: "Total files whose contents changed during a config-sync projection.",
	})
	mSyncErrTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_config_sync_errors_total",
		Help: "Total config-sync projection passes that failed.",
	})
	mSyncSourceMissing = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_config_sync_source_missing_total",
		Help: "Total times a source ConfigMap was absent (existing files kept).",
	})
	mSyncLastTS = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "synapse_operator_config_sync_last_timestamp_seconds",
		Help: "Unix timestamp of the last successful config-sync projection.",
	})
	mEndpointsFallbackTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_endpoints_fallback_total",
		Help: "Total backends that fell back to Service addressing because no ready endpoints were found.",
	})
	mRouteConflicts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_route_conflicts_total",
		Help: "Total host+path route conflicts ignored (first-writer-wins).",
	})
	mUnsupportedMatch = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_unsupported_match_total",
		Help: "Total Ingress/HTTPRoute match features not representable in synapse v1.",
	})
	mBackendUnresolved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_backend_unresolved_total",
		Help: "Total backendRefs/Ingress backends that could not be resolved.",
	})
	mHosts = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "synapse_operator_hosts",
		Help: "Distinct hosts in the most recent rendered upstreams.yaml.",
	})
	mRoutes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "synapse_operator_routes",
		Help: "Distinct host+path routes in the most recent rendered upstreams.yaml.",
	})
	mLastRenderTS = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "synapse_operator_last_render_timestamp_seconds",
		Help: "Unix timestamp of the last successful render.",
	})
	mReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "synapse_operator_ready",
		Help: "1 once the first successful upstreams render has completed, else 0.",
	})
	mCerts = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "synapse_operator_certs",
		Help: "TLS Secrets currently projected into the certificates dir.",
	})
	mCertErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "synapse_operator_cert_errors_total",
		Help: "Referenced TLS Secrets that were missing or not usable.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		mRenderTotal, mRenderErrTotal, mRenderChangedTotal, mReloadTotal,
		mRouteConflicts, mUnsupportedMatch, mBackendUnresolved,
		mHosts, mRoutes, mLastRenderTS, mReady, mCerts, mCertErrors,
		mSyncTotal, mSyncChangedTotal, mSyncErrTotal, mSyncSourceMissing, mSyncLastTS,
		mEndpointsFallbackTotal,
	)
}
