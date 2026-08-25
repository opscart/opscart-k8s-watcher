package store

import "strings"

// Canonical issue-type identifiers. Producers and consumers must use these
// constants (or normalize raw input through CanonicalIssueType) instead of
// string literals, so the compiler catches vocabulary drift between the
// scanner, dashboard, CLI, and store layers.
const (
	IssueCrashLoop            = "crash_loop"
	IssueProbeFailure         = "probe_failure"
	IssueOOMKilled            = "oomkilled"
	IssueImagePullBackOff     = "image_pull_backoff"
	IssueHighRestartCount     = "high_restart_count"
	IssuePrivilegedContainer  = "privileged_container"
	IssueUnprotectedNamespace = "unprotected_namespace"
	IssueIdleNamespace        = "idle_namespace"
)

// CanonicalIssueType returns the durable issue type used by new writes.
// Unknown issue types are intentionally preserved verbatim.
func CanonicalIssueType(issueType string) string {
	switch issueType {
	case "CrashLoopBackOff", "crash_loop":
		return "crash_loop"
	case "ProbeFailure", "CrashLoopBackOff (ProbeFailure)", "probe_failure":
		return "probe_failure"
	case "OOMKilled", "oom_killed", "CrashLoopBackOff (OOMKilled)", "oomkilled":
		return "oomkilled"
	case "ImagePullBackOff", "ErrImagePull", "image_pull", "image_pull_backoff":
		return "image_pull_backoff"
	case "HighRestartCount", "high_restart_count":
		return "high_restart_count"
	default:
		return issueType
	}
}

func issueTypeAliases(issueType string) []string {
	switch CanonicalIssueType(issueType) {
	case "crash_loop":
		return []string{"crash_loop", "CrashLoopBackOff"}
	case "probe_failure":
		return []string{"probe_failure", "ProbeFailure", "CrashLoopBackOff (ProbeFailure)"}
	case "oomkilled":
		return []string{"oomkilled", "oom_killed", "OOMKilled", "CrashLoopBackOff (OOMKilled)"}
	case "image_pull_backoff":
		return []string{"image_pull_backoff", "ImagePullBackOff", "ErrImagePull", "image_pull"}
	case "high_restart_count":
		return []string{"high_restart_count", "HighRestartCount"}
	default:
		return []string{issueType}
	}
}

func normalizeFingerprint(fingerprint string) string {
	parts := strings.Split(fingerprint, "/")
	if len(parts) == 0 {
		return fingerprint
	}
	parts[len(parts)-1] = CanonicalIssueType(parts[len(parts)-1])
	return strings.Join(parts, "/")
}

func fingerprintAliases(fingerprint string) []string {
	parts := strings.Split(fingerprint, "/")
	if len(parts) == 0 {
		return []string{fingerprint}
	}
	aliases := issueTypeAliases(parts[len(parts)-1])
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		copyParts := append([]string(nil), parts...)
		copyParts[len(copyParts)-1] = alias
		out = append(out, strings.Join(copyParts, "/"))
	}
	return out
}
