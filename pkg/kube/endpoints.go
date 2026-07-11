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

	count := 0
	for _, slice := range slices.Items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				count += len(ep.Addresses)
			}
		}
	}
	return count, nil
}

// ServiceHasReadyEndpoints reports whether the named service has at least one
// ready endpoint address across all of its EndpointSlices.
func ServiceHasReadyEndpoints(ctx context.Context, clientset kubernetes.Interface, namespace, svcName string) (bool, error) {
	count, err := ReadyEndpointAddressCount(ctx, clientset, namespace, svcName)
	return count > 0, err
}
