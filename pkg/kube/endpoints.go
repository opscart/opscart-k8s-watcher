// Package kube holds small Kubernetes API helpers shared across scanner and
// analyzer packages.
package kube

import (
	"context"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ReadyEndpointAddressCount returns the number of ready endpoint addresses for
// the named service, aggregated across all of its EndpointSlices. An endpoint
// counts as ready when Conditions.Ready is nil (unknown, treated as ready per
// EndpointSlice semantics) or explicitly true.
func ReadyEndpointAddressCount(ctx context.Context, clientset kubernetes.Interface, namespace, svcName string) (int, error) {
	slices, err := clientset.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + svcName,
	})
	if err != nil {
		return 0, err
	}
	return ReadyAddressCount(slices.Items), nil
}

// ReadyAddressCount sums ready endpoint addresses across already-scoped
// EndpointSlices — the same readiness rule ReadyEndpointAddressCount applies
// to its own live LIST result, extracted so a caller working from an
// already-observed EndpointSlice snapshot (e.g. a ClusterSnapshot
// generation) can reuse it instead of issuing a LIST of its own. Callers are
// responsible for scoping slices to one namespace+service first — see
// EndpointSlicesForService.
func ReadyAddressCount(slices []discoveryv1.EndpointSlice) int {
	count := 0
	for _, slice := range slices {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				count += len(ep.Addresses)
			}
		}
	}
	return count
}

// EndpointSlicesForService filters an already-observed, cluster-wide
// EndpointSlice list down to the slices for one namespace+service — the
// in-memory equivalent of ReadyEndpointAddressCount's namespaced LIST plus
// its discoveryv1.LabelServiceName label selector, for callers working from
// a snapshot instead of a live client.
func EndpointSlicesForService(slices []discoveryv1.EndpointSlice, namespace, svcName string) []discoveryv1.EndpointSlice {
	var scoped []discoveryv1.EndpointSlice
	for _, s := range slices {
		if s.Namespace == namespace && s.Labels[discoveryv1.LabelServiceName] == svcName {
			scoped = append(scoped, s)
		}
	}
	return scoped
}

// ServiceHasReadyEndpoints reports whether the named service has at least one
// ready endpoint address across all of its EndpointSlices.
func ServiceHasReadyEndpoints(ctx context.Context, clientset kubernetes.Interface, namespace, svcName string) (bool, error) {
	count, err := ReadyEndpointAddressCount(ctx, clientset, namespace, svcName)
	return count > 0, err
}
