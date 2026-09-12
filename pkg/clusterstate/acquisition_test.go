package clusterstate

import "testing"

func TestAcquisitionStateTrustworthy(t *testing.T) {
	cases := []struct {
		state AcquisitionState
		want  bool
	}{
		{AcquisitionHealthy, true},
		{AcquisitionDegraded, false},
		{AcquisitionResyncing, false},
		{AcquisitionStale, false},
	}
	for _, c := range cases {
		if got := c.state.Trustworthy(); got != c.want {
			t.Errorf("%s.Trustworthy() = %v, want %v", c.state, got, c.want)
		}
	}
}
