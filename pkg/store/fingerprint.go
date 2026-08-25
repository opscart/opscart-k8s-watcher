package store

import "strings"

// Fingerprint builds a stable, human-readable incident identity.
// Pod-name suffixes are stripped by the caller passing ownerName.
func Fingerprint(namespace, ownerKind, ownerName, issueType string) string {
	return namespace + "/" + ownerKind + "/" + ownerName + "/" + CanonicalIssueType(issueType)
}

// WorkloadFingerprintForPod is the single derivation of a workload incident
// fingerprint from a pod name. Incident WRITERS (scan persistence) and
// READERS (investigation history/timeline lookup) must both use this
// function. The correctness invariant for history lookup is write/read
// SYMMETRY of the derivation — not agreement with the pod's actual
// OwnerReferences. Resolving real ownership via the Kubernetes API (see
// resolveOwner in the dashboard) is correct for display, workload
// relationships, and blast-radius analysis, but must never be used to
// build an incident lookup fingerprint: it diverges from what was written
// (e.g. StatefulSet "prometheus" vs stored "prometheus-0") and the lookup
// silently misses.
func WorkloadFingerprintForPod(namespace, podName, issueType string) string {
	return Fingerprint(namespace, "Workload", OwnerNameFromPod(podName), issueType)
}

// OwnerNameFromPod derives the incident-identity owner segment from a pod
// name using string shape only.
//
// CONTRACT: StatefulSet pods (name-0, name-1, ...) intentionally keep their
// ordinal and remain INSTANCE-SCOPED — prometheus-0 and prometheus-1 carry
// separate incident histories (see "StatefulSet instance scoped" in the
// investigation UI and the {"prometheus-0", "prometheus-0"} store test).
// Do not "fix" this by stripping ordinals or substituting the owning
// StatefulSet's name: either change merges replica histories and orphans
// every previously stored fingerprint.
func OwnerNameFromPod(podName string) string {
	segments := strings.Split(podName, "-")

	// Deployment pods end in:
	// <replicaset-hash>-<five-character-pod-suffix>
	//
	// The final pod suffix may contain only letters, such as jxmtx.
	if len(segments) >= 3 {
		podSuffix := segments[len(segments)-1]
		replicaSetHash := segments[len(segments)-2]

		if looksLikePodSuffix(podSuffix) && looksLikeHash(replicaSetHash) {
			return strings.Join(segments[:len(segments)-2], "-")
		}
	}

	stripped := 0
	for stripped < 2 && len(segments) > 1 {
		last := segments[len(segments)-1]
		if !looksLikeHash(last) {
			break
		}
		segments = segments[:len(segments)-1]
		stripped++
	}

	if stripped == 0 {
		return podName
	}
	return strings.Join(segments, "-")
}

func looksLikePodSuffix(s string) bool {
	if len(s) != 5 {
		return false
	}

	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}

	return true
}

// looksLikeHash reports whether s looks like a ReplicaSet/pod hash
// suffix: 5-10 lowercase alphanumeric characters containing at least
// one digit.
func looksLikeHash(s string) bool {
	if len(s) < 5 || len(s) > 10 {
		return false
	}
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'z':
			// ok
		default:
			return false
		}
	}
	return hasDigit
}
