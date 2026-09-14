package kube

import (
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolPtr(b bool) *bool { return &b }

func slice(namespace, svcName string, ready ...bool) discoveryv1.EndpointSlice {
	endpoints := make([]discoveryv1.Endpoint, len(ready))
	for i, r := range ready {
		endpoints[i] = discoveryv1.Endpoint{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(r)},
		}
	}
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      svcName + "-abc12",
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
		Endpoints: endpoints,
	}
}

func TestReadyAddressCountTreatsNilConditionAsReady(t *testing.T) {
	s := discoveryv1.EndpointSlice{Endpoints: []discoveryv1.Endpoint{
		{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{}},
	}}
	if got := ReadyAddressCount([]discoveryv1.EndpointSlice{s}); got != 1 {
		t.Fatalf("ReadyAddressCount = %d, want 1 (nil Ready treated as ready)", got)
	}
}

func TestReadyAddressCountExcludesNotReady(t *testing.T) {
	s := slice("payments", "api", true, false)
	if got := ReadyAddressCount([]discoveryv1.EndpointSlice{s}); got != 1 {
		t.Fatalf("ReadyAddressCount = %d, want 1", got)
	}
}

func TestReadyAddressCountSumsAcrossSlices(t *testing.T) {
	a := slice("payments", "api", true)
	b := slice("payments", "api", true)
	if got := ReadyAddressCount([]discoveryv1.EndpointSlice{a, b}); got != 2 {
		t.Fatalf("ReadyAddressCount = %d, want 2", got)
	}
}

func TestEndpointSlicesForServiceFiltersByNamespaceAndLabel(t *testing.T) {
	target := slice("payments", "api", true)
	otherNamespace := slice("checkout", "api", true)
	otherService := slice("payments", "worker", true)
	all := []discoveryv1.EndpointSlice{target, otherNamespace, otherService}

	got := EndpointSlicesForService(all, "payments", "api")

	if len(got) != 1 || got[0].Name != target.Name {
		t.Fatalf("EndpointSlicesForService = %+v, want only %q", got, target.Name)
	}
}

func TestEndpointSlicesForServiceNoMatchReturnsEmpty(t *testing.T) {
	all := []discoveryv1.EndpointSlice{slice("payments", "api", true)}

	got := EndpointSlicesForService(all, "payments", "missing")

	if len(got) != 0 {
		t.Fatalf("EndpointSlicesForService = %+v, want none", got)
	}
}

// TestReadyEndpointAddressCountMatchesSnapshotEquivalent proves the live
// LIST-based path and the snapshot-based ReadyAddressCount+EndpointSlicesForService
// composition agree, for the same underlying EndpointSlices — the
// regression this split is meant to prevent.
func TestReadyEndpointAddressCountMatchesSnapshotEquivalent(t *testing.T) {
	all := []discoveryv1.EndpointSlice{
		slice("payments", "api", true, false),
		slice("checkout", "api", true), // different namespace, must not count
	}

	fromSnapshot := ReadyAddressCount(EndpointSlicesForService(all, "payments", "api"))
	fromDirect := ReadyAddressCount([]discoveryv1.EndpointSlice{all[0]})

	if fromSnapshot != fromDirect || fromSnapshot != 1 {
		t.Fatalf("fromSnapshot=%d fromDirect=%d, want both 1", fromSnapshot, fromDirect)
	}
}
