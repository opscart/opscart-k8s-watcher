package acquisition

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestTweakToPodWarningEventsSetsFieldSelector(t *testing.T) {
	options := &metav1.ListOptions{}
	tweakToPodWarningEvents(options)

	if options.FieldSelector != podWarningEventFieldSelector {
		t.Fatalf("FieldSelector = %q, want %q", options.FieldSelector, podWarningEventFieldSelector)
	}
}

func TestTweakToPodWarningEventsOnlyTouchesFieldSelector(t *testing.T) {
	options := &metav1.ListOptions{LabelSelector: "app=demo", Limit: 500}
	tweakToPodWarningEvents(options)

	if options.LabelSelector != "app=demo" || options.Limit != 500 {
		t.Fatalf("tweak modified unrelated ListOptions fields: %+v", options)
	}
}

// TestPodWarningEventInformerRequestsFilteredList proves the filter
// actually reaches the Kubernetes client for the Event informer's LIST
// call — not just that tweakToPodWarningEvents is correct in isolation,
// but that Runtime wires it to the right (and only the right) informer.
func TestPodWarningEventInformerRequestsFilteredList(t *testing.T) {
	client := fake.NewSimpleClientset()

	var gotEventFields, gotPodFields string
	sawEventList, sawPodList := false, false
	client.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listAction, ok := action.(k8stesting.ListActionImpl)
		if !ok {
			return false, nil, nil
		}
		switch action.GetResource().Resource {
		case "events":
			sawEventList = true
			gotEventFields = listAction.GetListRestrictions().Fields.String()
		case "pods":
			sawPodList = true
			gotPodFields = listAction.GetListRestrictions().Fields.String()
		}
		return false, nil, nil
	})

	rt := NewRuntime("cluster-a", client)
	startAndSync(t, rt)

	if !sawEventList {
		t.Fatal("Event informer never issued a LIST call")
	}
	if !sawPodList {
		t.Fatal("Pod informer never issued a LIST call")
	}

	wantEventFields := fields.ParseSelectorOrDie(podWarningEventFieldSelector).String()
	if gotEventFields != wantEventFields {
		t.Fatalf("Event LIST field selector = %q, want %q", gotEventFields, wantEventFields)
	}
	if gotPodFields != fields.Everything().String() {
		t.Fatalf("Pod LIST field selector = %q, want unfiltered — the Event filter must not leak onto other resources", gotPodFields)
	}
}
