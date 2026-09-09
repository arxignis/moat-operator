package controllers

import (
	"context"
	"net"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Endpoint-backed backend resolution.
//
// Without this the renderer emits a Service FQDN and synapse resolves it
// through its in-process dns_cache, which pins the FIRST A record for the
// cache TTL, never evicts on a failed re-resolve, and is configured once at
// startup — so a replaced pod can be addressed for a long time after it is
// gone. Reading EndpointSlices instead means the rendered file carries live
// pod IPs, which synapse treats as literals and never resolves at all, and
// a pod change re-renders immediately.
//
// EndpointSlice, not the legacy Endpoints object: Endpoints is deprecated,
// truncates at 1000 addresses per Service, and lacks the conditions and
// addressType fields relied on below.
//
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// servicePortName maps an Ingress/Gateway backend port — given by NAME or by
// NUMBER — through the Service spec to the port name that EndpointSlice
// ports are keyed on, plus the Service port itself for use as a fallback.
//
// This indirection is the sharpest trap in endpoint rendering. Today's
// renderer emits the SERVICE port, which is correct when addressing a
// ClusterIP or DNS name because kube-proxy rewrites it. Pods listen on
// targetPort. Pairing a pod IP with the Service port sends every request to
// a port nothing is bound to, and it fails uniformly — it looks like a total
// outage rather than a subtle bug.
func servicePortName(svc *corev1.Service, portName string, portNumber int32) (string, int32, bool) {
	for _, sp := range svc.Spec.Ports {
		switch {
		case portName != "" && sp.Name == portName:
			return sp.Name, sp.Port, true
		case portName == "" && portNumber != 0 && sp.Port == portNumber:
			return sp.Name, sp.Port, true
		}
	}
	return "", 0, false
}

// endpointUsable reports whether an endpoint should receive traffic.
//
// A nil Ready is "ready" per the EndpointSlice contract (it means readiness
// is unknown for a Service without a readiness probe); only an explicit
// false excludes. Treating nil as not-ready would silently drop every
// endpoint on some setups. Terminating pods are excluded so a rolling update
// stops sending them new requests.
func endpointUsable(ep discoveryv1.Endpoint) bool {
	if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
		return false
	}
	if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
		return false
	}
	return true
}

// sliceEndpointAddrs unions every slice for one Service into a sorted list of
// "ip:port" strings.
//
// Sorting is load-bearing, not cosmetic. Slice list order and intra-slice
// endpoint order are both unstable, so an unsorted result would differ
// between reconciles even when the endpoint set is identical — defeating the
// write/no-op suppression and turning every resync into a ConfigMap write
// plus a SIGHUP, forever.
//
// Dual-stack Services are rendered as IPv4 when any IPv4 endpoint exists,
// otherwise IPv6. Unioning both families would enter each pod twice and
// silently double its share of the round-robin.
func sliceEndpointAddrs(slices []discoveryv1.EndpointSlice, portName string, svcPort int32) []string {
	var v4, v6 []string

	for _, slice := range slices {
		// FQDN slices carry names, not addresses; synapse would have to
		// resolve them, which is the behaviour being replaced.
		if slice.AddressType != discoveryv1.AddressTypeIPv4 &&
			slice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}

		// Resolve the port for this slice. A slice with no ports entry means
		// "all ports"; a nil Port within an entry means unrestricted. Both
		// fall back to the Service port.
		port := svcPort
		if len(slice.Ports) > 0 {
			matched := false
			for _, p := range slice.Ports {
				name := ""
				if p.Name != nil {
					name = *p.Name
				}
				// nil and "" are the same thing for a single unnamed port.
				if name != portName {
					continue
				}
				if p.Port != nil {
					port = *p.Port
				}
				matched = true
				break
			}
			if !matched {
				continue
			}
		}
		if port == 0 {
			continue
		}

		for _, ep := range slice.Endpoints {
			if !endpointUsable(ep) {
				continue
			}
			for _, addr := range ep.Addresses {
				// JoinHostPort brackets IPv6 correctly.
				hp := net.JoinHostPort(addr, strconv.Itoa(int(port)))
				if slice.AddressType == discoveryv1.AddressTypeIPv6 {
					v6 = append(v6, hp)
				} else {
					v4 = append(v4, hp)
				}
			}
		}
	}

	out := v4
	if len(out) == 0 {
		out = v6
	}
	sort.Strings(out)
	return out
}

// endpointAddrs lists every EndpointSlice belonging to a Service and returns
// the ready pod addresses. Returns ok=false whenever the caller should fall
// back rather than render an empty server list.
func (r *IngressReconciler) endpointAddrs(
	ctx context.Context, ns string, b networkingv1.IngressServiceBackend,
) ([]string, bool) {
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: b.Name}, &svc); err != nil {
		return nil, false
	}
	// A headless or ExternalName Service has no endpoint set worth rendering
	// this way; the FQDN path already handles them.
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		return nil, false
	}

	portName, svcPort, ok := servicePortName(&svc, b.Port.Name, b.Port.Number)
	if !ok {
		return nil, false
	}

	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list,
		client.InNamespace(ns),
		client.MatchingLabels{discoveryv1.LabelServiceName: b.Name},
	); err != nil {
		return nil, false
	}

	addrs := sliceEndpointAddrs(list.Items, portName, svcPort)
	if len(addrs) == 0 {
		return nil, false
	}
	return addrs, true
}

// backendServers resolves one Ingress backend to the server list to render.
//
// Fallback chain, in order: ready endpoints -> ClusterIP (when
// --resolve-backend-cluster-ips) -> Service FQDN. The two flags therefore
// compose rather than conflict; endpoints simply win when they have data.
//
// It NEVER returns an empty list with ok=true. An empty server list is not a
// degraded route, it is a 502 for every request to that host, so scaling a
// backend to zero must leave the previous addressing in place rather than
// blanking the route.
func (r *IngressReconciler) backendServers(
	ctx context.Context, ns string, b networkingv1.IngressBackend,
) ([]backend, bool) {
	if r.ResolveBackendEndpoints && b.Service != nil {
		if addrs, ok := r.endpointAddrs(ctx, ns, *b.Service); ok {
			out := make([]backend, 0, len(addrs))
			for _, a := range addrs {
				out = append(out, backend{addr: a})
			}
			return out, true
		}
		mEndpointsFallbackTotal.Inc()
		ctrl.LoggerFrom(ctx).Info(
			"no ready endpoints; falling back to Service addressing",
			"namespace", ns, "service", b.Service.Name)
	}
	addr, ok := r.backendAddr(ctx, ns, b)
	if !ok {
		return nil, false
	}
	return []backend{{addr: addr}}, true
}
