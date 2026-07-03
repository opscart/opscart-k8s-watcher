package store

import "strings"

// Fingerprint builds a stable, human-readable incident identity.
// Pod-name suffixes are stripped by the caller passing ownerName.
func Fingerprint(namespace, ownerKind, ownerName, issueType string) string {
	return namespace + "/" + ownerKind + "/" + ownerName + "/" + issueType
}

// OwnerNameFromPod strips ReplicaSet/pod hash suffixes:
// "fraud-detection-5fc85b778-8j6s5" -> "fraud-detection"
func OwnerNameFromPod(podName string) string {
	segments := strings.Split(podName, "-")
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
