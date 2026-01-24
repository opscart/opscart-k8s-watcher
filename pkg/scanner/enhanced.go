package scanner

import (
	"fmt"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TakeEnhancedSnapshot captures detailed cluster state including services, ingresses, PVCs
func (s *Scanner) TakeEnhancedSnapshot(namespace string) (*models.EnhancedClusterSnapshot, error) {
	snapshot := &models.EnhancedClusterSnapshot{
		ClusterSnapshot: models.ClusterSnapshot{
			ClusterName: s.clusterName,
			Timestamp:   time.Now(),
		},
	}

	// Get base snapshot data (pods, deployments, etc)
	baseSnapshot, err := s.TakeSnapshot(namespace)
	if err != nil {
		return nil, err
	}
	snapshot.ClusterSnapshot = *baseSnapshot

	// Get services
	services, err := s.getServiceDetails(namespace)
	if err == nil {
		snapshot.Services = services
	}

	// Get ingresses
	ingresses, err := s.getIngressDetails(namespace)
	if err == nil {
		snapshot.Ingresses = ingresses
	}

	// Get PVC details
	pvcDetails, err := s.getPVCDetails(namespace)
	if err == nil {
		snapshot.PVCDetails = pvcDetails
	}

	// Get ConfigMaps count
	configMaps, err := s.getResourceCounts(namespace, "configmaps")
	if err == nil {
		snapshot.ConfigMaps = configMaps
	}

	// Get Secrets count
	secrets, err := s.getResourceCounts(namespace, "secrets")
	if err == nil {
		snapshot.Secrets = secrets
	}

	// Get Network Policies
	networkPolicies, err := s.getNetworkPolicies(namespace)
	if err == nil {
		snapshot.NetworkPolicies = networkPolicies
	}

	return snapshot, nil
}

// getServiceDetails retrieves detailed service information
func (s *Scanner) getServiceDetails(namespace string) ([]models.ServiceDetail, error) {
	var services []models.ServiceDetail

	svcList, err := s.clientset.CoreV1().Services(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, svc := range svcList.Items {
		// Get endpoints to see if service has backends
		endpoints, _ := s.clientset.CoreV1().Endpoints(svc.Namespace).Get(s.ctx, svc.Name, metav1.GetOptions{})
		endpointCount := 0
		if endpoints != nil {
			for _, subset := range endpoints.Subsets {
				endpointCount += len(subset.Addresses)
			}
		}

		// Extract external IP for LoadBalancer services
		externalIP := ""
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			if len(svc.Status.LoadBalancer.Ingress) > 0 {
				if svc.Status.LoadBalancer.Ingress[0].IP != "" {
					externalIP = svc.Status.LoadBalancer.Ingress[0].IP
				} else if svc.Status.LoadBalancer.Ingress[0].Hostname != "" {
					externalIP = svc.Status.LoadBalancer.Ingress[0].Hostname
				}
			}
		}

		// Extract ports
		var ports []int32
		for _, port := range svc.Spec.Ports {
			ports = append(ports, port.Port)
		}

		services = append(services, models.ServiceDetail{
			ServiceInfo: models.ServiceInfo{
				Name:       svc.Name,
				Namespace:  svc.Namespace,
				Type:       string(svc.Spec.Type),
				ClusterIP:  svc.Spec.ClusterIP,
				ExternalIP: externalIP,
				Ports:      ports,
			},
			Endpoints: endpointCount,
			Age:       formatDuration(time.Since(svc.CreationTimestamp.Time)),
			Selector:  svc.Spec.Selector,
		})
	}

	return services, nil
}

// getIngressDetails retrieves detailed ingress information
func (s *Scanner) getIngressDetails(namespace string) ([]models.IngressDetail, error) {
	var ingresses []models.IngressDetail

	ingressList, err := s.clientset.NetworkingV1().Ingresses(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, ing := range ingressList.Items {
		// Extract hosts
		var hosts []string
		tlsEnabled := false

		for _, rule := range ing.Spec.Rules {
			if rule.Host != "" {
				hosts = append(hosts, rule.Host)
			}
		}

		if len(ing.Spec.TLS) > 0 {
			tlsEnabled = true
		}

		// Determine backend service (simplified - just first rule)
		backend := ""
		if len(ing.Spec.Rules) > 0 && ing.Spec.Rules[0].HTTP != nil {
			if len(ing.Spec.Rules[0].HTTP.Paths) > 0 {
				path := ing.Spec.Rules[0].HTTP.Paths[0]
				if path.Backend.Service != nil {
					backend = path.Backend.Service.Name
				}
			}
		}

		// Get ingress class
		ingressClass := ""
		if ing.Spec.IngressClassName != nil {
			ingressClass = *ing.Spec.IngressClassName
		}

		ingresses = append(ingresses, models.IngressDetail{
			IngressInfo: models.IngressInfo{
				Name:       ing.Name,
				Namespace:  ing.Namespace,
				Hosts:      hosts,
				TLSEnabled: tlsEnabled,
				Backend:    backend,
			},
			IngressClass: ingressClass,
			Age:          formatDuration(time.Since(ing.CreationTimestamp.Time)),
			Rules:        len(ing.Spec.Rules),
		})
	}

	return ingresses, nil
}

// getPVCDetails retrieves detailed PVC information
func (s *Scanner) getPVCDetails(namespace string) ([]models.PVCDetail, error) {
	var pvcDetails []models.PVCDetail

	pvcList, err := s.clientset.CoreV1().PersistentVolumeClaims(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	// Get all pods to find which ones use PVCs
	podList, _ := s.clientset.CoreV1().Pods(namespace).List(s.ctx, metav1.ListOptions{})
	pvcUsage := make(map[string]string)

	for _, pod := range podList.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				pvcUsage[volume.PersistentVolumeClaim.ClaimName] = pod.Name
			}
		}
	}

	for _, pvc := range pvcList.Items {
		// Get access mode
		accessMode := ""
		if len(pvc.Spec.AccessModes) > 0 {
			accessMode = string(pvc.Spec.AccessModes[0])
		}

		// Get size
		size := ""
		if storage, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			size = storage.String()
		}

		// Get storage class
		storageClass := ""
		if pvc.Spec.StorageClassName != nil {
			storageClass = *pvc.Spec.StorageClassName
		}

		// Find which pod uses this PVC
		usedBy := pvcUsage[pvc.Name]

		pvcDetails = append(pvcDetails, models.PVCDetail{
			PVCInfo: models.PVCInfo{
				Name:         pvc.Name,
				Namespace:    pvc.Namespace,
				Status:       string(pvc.Status.Phase),
				StorageClass: storageClass,
				Size:         size,
				VolumeName:   pvc.Spec.VolumeName,
			},
			Age:        formatDuration(time.Since(pvc.CreationTimestamp.Time)),
			AccessMode: accessMode,
			UsedBy:     usedBy,
		})
	}

	return pvcDetails, nil
}

// getResourceCounts gets count of resources by namespace
func (s *Scanner) getResourceCounts(namespace string, resourceType string) ([]models.ResourceCount, error) {
	counts := make(map[string]int)

	switch resourceType {
	case "configmaps":
		cmList, err := s.clientset.CoreV1().ConfigMaps(namespace).List(s.ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, cm := range cmList.Items {
			counts[cm.Namespace]++
		}

	case "secrets":
		secretList, err := s.clientset.CoreV1().Secrets(namespace).List(s.ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, secret := range secretList.Items {
			// Skip service account tokens
			if secret.Type != corev1.SecretTypeServiceAccountToken {
				counts[secret.Namespace]++
			}
		}
	}

	var result []models.ResourceCount
	for ns, count := range counts {
		result = append(result, models.ResourceCount{
			Namespace: ns,
			Count:     count,
		})
	}

	return result, nil
}

// getNetworkPolicies retrieves network policy information
func (s *Scanner) getNetworkPolicies(namespace string) ([]models.NetworkPolicyInfo, error) {
	var policies []models.NetworkPolicyInfo

	npList, err := s.clientset.NetworkingV1().NetworkPolicies(namespace).List(s.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, np := range npList.Items {
		podSelector := fmt.Sprintf("%v", np.Spec.PodSelector.MatchLabels)
		if podSelector == "map[]" {
			podSelector = "all pods"
		}

		var policyTypes []string
		for _, pt := range np.Spec.PolicyTypes {
			policyTypes = append(policyTypes, string(pt))
		}

		policies = append(policies, models.NetworkPolicyInfo{
			Name:        np.Name,
			Namespace:   np.Namespace,
			PodSelector: podSelector,
			PolicyTypes: policyTypes,
		})
	}

	return policies, nil
}
