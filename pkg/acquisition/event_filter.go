package acquisition

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// podWarningEventFieldSelector is the exact filter the Waste analyzer's
// probe-failure signal has always used (pkg/analyzer/waste.go,
// detectStalePodsWithClusterEvents): every Warning Event involving a Pod,
// nothing else. Phase 3 gives this the same filter, at the informer level,
// instead of caching every cluster Event — see docs/08 Phase 0 findings
// ("Scan-cycle Waste event evidence uses a filtered informer over
// Pod-involved Warning events").
const podWarningEventFieldSelector = "involvedObject.kind=Pod,type=Warning"

// tweakToPodWarningEvents is the ListOptions mutation applied to the
// dedicated Event informer factory so its single Event informer only ever
// lists/watches Pod-involved Warning events. It must not be reused for any
// other resource kind's factory — a filtered SharedInformerFactory applies
// its tweak to every informer it creates, which is exactly why the Event
// informer gets its own factory instance (see runtime.go).
func tweakToPodWarningEvents(options *metav1.ListOptions) {
	options.FieldSelector = podWarningEventFieldSelector
}
