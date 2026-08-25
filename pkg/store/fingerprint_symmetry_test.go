package store

import "testing"

// These tests guard the incident-fingerprint symmetry contract:
// incident writers (scan persistence) and readers (investigation
// history/timeline lookup) must derive identity through the same
// function, WorkloadFingerprintForPod. See the v1.11.0 bug where the
// investigation read path preferred the OwnerReferences-resolved name
// ("prometheus") while the write path stored the instance identity
// ("prometheus-0"), so StatefulSet history lookups silently missed.

func TestWorkloadFingerprintStatefulSetInstancesStayDistinct(t *testing.T) {
	fp0 := WorkloadFingerprintForPod("monitoring", "prometheus-0", IssueCrashLoop)
	fp1 := WorkloadFingerprintForPod("monitoring", "prometheus-1", IssueCrashLoop)

	if fp0 != "monitoring/Workload/prometheus-0/crash_loop" {
		t.Errorf("prometheus-0 fingerprint = %q, want instance-scoped identity", fp0)
	}
	if fp0 == fp1 {
		t.Errorf("StatefulSet replicas must keep separate incident histories; got identical fingerprint %q", fp0)
	}
}

func TestWorkloadFingerprintDeploymentPodsCollapseToOwner(t *testing.T) {
	fpA := WorkloadFingerprintForPod("payments", "api-7f9c4d5b6-abc12", IssueOOMKilled)
	fpB := WorkloadFingerprintForPod("payments", "api-7f9c4d5b6-xyz89", IssueOOMKilled)

	if fpA != "payments/Workload/api/oomkilled" {
		t.Errorf("deployment pod fingerprint = %q, want owner-collapsed identity", fpA)
	}
	if fpA != fpB {
		t.Errorf("pods of the same Deployment must share one incident history: %q vs %q", fpA, fpB)
	}
}

// TestWorkloadFingerprintWriteReadSymmetryAcrossAliases pins that a
// writer persisting a raw status alias and a reader looking up with the
// canonical identifier land on the same fingerprint (Fingerprint
// canonicalizes the issue-type segment).
func TestWorkloadFingerprintWriteReadSymmetryAcrossAliases(t *testing.T) {
	written := WorkloadFingerprintForPod("web", "frontend-abc123-def45", "OOMKilled")
	lookedUp := WorkloadFingerprintForPod("web", "frontend-abc123-def45", IssueOOMKilled)

	if written != lookedUp {
		t.Errorf("write/read fingerprints diverge across issue-type aliases: %q vs %q", written, lookedUp)
	}
}
