package store

import "strings"

// Fingerprint builds a stable, human-readable incident identity.
// Pod-name suffixes are stripped by the caller passing ownerName.
func Fingerprint(namespace, ownerKind, ownerName, issueType string) string {
	return namespace + "/" + ownerKind + "/" + ownerName + "/" + issueType
}

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
