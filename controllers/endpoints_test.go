package controllers

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ptrStr(s string) *string { return &s }
func ptrI32(i int32) *int32   { return &i }
func ptrBool(b bool) *bool    { return &b }

// svc with port 80 -> targetPort 8080, named "http".
func testSvc() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "services", Name: "api"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.43.0.10",
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)},
			},
		},
	}
}

func testSlice(name string, addrType discoveryv1.AddressType, port int32, addrs ...string) *discoveryv1.EndpointSlice {
	eps := make([]discoveryv1.Endpoint, 0, len(addrs))
	for _, a := range addrs {
		eps = append(eps, discoveryv1.Endpoint{
			Addresses:  []string{a},
			Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)},
		})
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "services",
			Name:      name,
			Labels:    map[string]string{discoveryv1.LabelServiceName: "api"},
		},
		AddressType: addrType,
		Ports:       []discoveryv1.EndpointPort{{Name: ptrStr("http"), Port: ptrI32(port)}},
		Endpoints:   eps,
	}
}

func TestServicePortName_ByNumberAndByName(t *testing.T) {
	svc := testSvc()

	name, port, ok := servicePortName(svc, "", 80)
	if !ok || name != "http" || port != 80 {
		t.Errorf("by number = (%q,%d,%v), want (http,80,true)", name, port, ok)
	}
	name, port, ok = servicePortName(svc, "http", 0)
	if !ok || name != "http" || port != 80 {
		t.Errorf("by name = (%q,%d,%v), want (http,80,true)", name, port, ok)
	}
	if _, _, ok := servicePortName(svc, "", 9999); ok {
		t.Error("unknown port number should not resolve")
	}
	if _, _, ok := servicePortName(svc, "nope", 0); ok {
		t.Error("unknown port name should not resolve")
	}
}

// The headline correctness test: pods listen on targetPort, not the Service
// port. Rendering ":80" next to a pod IP would fail on every request.
func TestSliceEndpointAddrs_UsesTargetPortNotServicePort(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{
		*testSlice("api-abc", discoveryv1.AddressTypeIPv4, 8080, "10.42.1.5"),
	}
	got := sliceEndpointAddrs(slices, "http", 80)
	want := []string{"10.42.1.5:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (Service port 80 must not leak through)", got, want)
	}
}

// Order from the API is unstable; an unsorted result would make every
// reconcile look like a change and produce a permanent write/SIGHUP storm.
func TestSliceEndpointAddrs_UnionsSlicesAndSorts(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{
		*testSlice("api-zzz", discoveryv1.AddressTypeIPv4, 8080, "10.42.3.9", "10.42.1.2"),
		*testSlice("api-aaa", discoveryv1.AddressTypeIPv4, 8080, "10.42.2.7"),
	}
	got := sliceEndpointAddrs(slices, "http", 80)
	want := []string{"10.42.1.2:8080", "10.42.2.7:8080", "10.42.3.9:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// nil Ready means "readiness unknown" and MUST be treated as ready; getting
// this backwards drops every endpoint for Services without a readiness probe.
func TestSliceEndpointAddrs_ReadinessFiltering(t *testing.T) {
	slice := testSlice("api-abc", discoveryv1.AddressTypeIPv4, 8080)
	slice.Endpoints = []discoveryv1.Endpoint{
		{Addresses: []string{"10.42.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
		{Addresses: []string{"10.42.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(false)}},
		{Addresses: []string{"10.42.0.3"}, Conditions: discoveryv1.EndpointConditions{}},
		{Addresses: []string{"10.42.0.4"}, Conditions: discoveryv1.EndpointConditions{
			Ready: ptrBool(true), Terminating: ptrBool(true)}},
	}
	got := sliceEndpointAddrs([]discoveryv1.EndpointSlice{*slice}, "http", 80)
	want := []string{"10.42.0.1:8080", "10.42.0.3:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (nil Ready is ready; false and Terminating are not)", got, want)
	}
}

func TestSliceEndpointAddrs_IPv6IsBracketed(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{
		*testSlice("api-v6", discoveryv1.AddressTypeIPv6, 8080, "fd00::5"),
	}
	got := sliceEndpointAddrs(slices, "http", 80)
	if len(got) != 1 || got[0] != "[fd00::5]:8080" {
		t.Errorf("got %v, want [[fd00::5]:8080]", got)
	}
}

// Unioning both families would enter each pod twice and double its share of
// the round-robin.
func TestSliceEndpointAddrs_PrefersIPv4OnDualStack(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{
		*testSlice("api-v4", discoveryv1.AddressTypeIPv4, 8080, "10.42.0.1"),
		*testSlice("api-v6", discoveryv1.AddressTypeIPv6, 8080, "fd00::5"),
	}
	got := sliceEndpointAddrs(slices, "http", 80)
	want := []string{"10.42.0.1:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSliceEndpointAddrs_SkipsFQDNAddressType(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{
		*testSlice("api-fqdn", discoveryv1.AddressTypeFQDN, 8080, "api.example.com"),
	}
	if got := sliceEndpointAddrs(slices, "http", 80); len(got) != 0 {
		t.Errorf("got %v, want empty (FQDN slices would reintroduce DNS)", got)
	}
}

func TestSliceEndpointAddrs_NilPortFallsBackToServicePort(t *testing.T) {
	slice := testSlice("api-abc", discoveryv1.AddressTypeIPv4, 0, "10.42.0.1")
	slice.Ports = []discoveryv1.EndpointPort{{Name: ptrStr("http"), Port: nil}}
	got := sliceEndpointAddrs([]discoveryv1.EndpointSlice{*slice}, "http", 80)
	want := []string{"10.42.0.1:80"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func ingBackend() networkingv1.IngressBackend {
	return networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{
			Name: "api",
			Port: networkingv1.ServiceBackendPort{Number: 80},
		},
	}
}

func TestBackendServers_RendersEveryReadyPod(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(testSvc(), testSlice("api-abc", discoveryv1.AddressTypeIPv4, 8080, "10.42.1.5", "10.42.2.6")).
		Build()
	r := &IngressReconciler{Client: c, ClusterDomain: "cluster.local", ResolveBackendEndpoints: true}

	got, ok := r.backendServers(context.Background(), "services", ingBackend())
	if !ok || len(got) != 2 {
		t.Fatalf("got %v ok=%v, want 2 servers", got, ok)
	}
	if got[0].addr != "10.42.1.5:8080" || got[1].addr != "10.42.2.6:8080" {
		t.Errorf("got %v, want sorted pod IPs on targetPort", got)
	}
}

// Scaling a backend to zero must not blank the route — an empty server list
// is a 502 for every request, not a degraded route.
func TestBackendServers_NoReadyEndpointsFallsBackToServiceAddressing(t *testing.T) {
	slice := testSlice("api-abc", discoveryv1.AddressTypeIPv4, 8080, "10.42.1.5")
	slice.Endpoints[0].Conditions.Ready = ptrBool(false)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(testSvc(), slice).Build()
	r := &IngressReconciler{Client: c, ClusterDomain: "cluster.local", ResolveBackendEndpoints: true}

	got, ok := r.backendServers(context.Background(), "services", ingBackend())
	if !ok || len(got) != 1 {
		t.Fatalf("got %v ok=%v, want the FQDN fallback", got, ok)
	}
	if got[0].addr != "api.services.svc.cluster.local:80" {
		t.Errorf("got %q, want the Service FQDN", got[0].addr)
	}
}

func TestBackendServers_MissingSlicesFallsBackToClusterIPWhenEnabled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testSvc()).Build()
	r := &IngressReconciler{
		Client: c, ClusterDomain: "cluster.local",
		ResolveBackendEndpoints: true, ResolveBackendClusterIPs: true,
	}

	got, ok := r.backendServers(context.Background(), "services", ingBackend())
	if !ok || len(got) != 1 || got[0].addr != "10.43.0.10:80" {
		t.Fatalf("got %v ok=%v, want the ClusterIP fallback", got, ok)
	}
}

// Regression guard: with the flag off the output must be byte-identical to
// what backendAddr produced before this change.
func TestBackendServers_FlagOffProducesSingleServiceAddress(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(testSvc(), testSlice("api-abc", discoveryv1.AddressTypeIPv4, 8080, "10.42.1.5")).
		Build()
	r := &IngressReconciler{Client: c, ClusterDomain: "cluster.local"}

	got, ok := r.backendServers(context.Background(), "services", ingBackend())
	if !ok || len(got) != 1 || got[0].addr != "api.services.svc.cluster.local:80" {
		t.Fatalf("got %v ok=%v, want the unchanged FQDN behaviour", got, ok)
	}
}
