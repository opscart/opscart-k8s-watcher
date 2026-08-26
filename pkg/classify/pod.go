// Package classify contains canonical pod-issue classification shared by the
// CLI and dashboard scan paths.
package classify

import (
	"fmt"
	"regexp"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

var crashLoopContainerRE = regexp.MustCompile(`^Container (\S+) is crash looping`)

// IsPodFailureReason reports whether reason participates in the shared
// single-verdict pod-failure classification.
func IsPodFailureReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff", "OOMKilled", "ProbeFailure", "Error",
		"ImagePullBackOff", "ErrImagePull", "HighRestartCount":
		return true
	default:
		return false
	}
}

// PodFailure reduces the independent signals observed for one physical pod
// to exactly one deterministic verdict. All issues must describe the same
// namespace/name. probeFailure is event-derived evidence used by the CLI;
// callers that already represent that evidence as a ProbeFailure issue may
// leave it false.
//
// Priority is strongest/directest evidence first:
//
//  1. CrashLoopBackOff + OOMKilled
//  2. CrashLoopBackOff + probe-failure evidence
//  3. CrashLoopBackOff
//  4. OOMKilled
//  5. ProbeFailure
//  6. Error
//  7. ImagePullBackOff / ErrImagePull
//  8. HighRestartCount
func PodFailure(podIssues []models.EmergencyIssue, probeFailure bool) (models.EmergencyIssue, bool) {
	var crashLoop, oom, probe, containerError, imagePull, highRestart *models.EmergencyIssue
	for i := range podIssues {
		issue := &podIssues[i]
		switch issue.Reason {
		case "CrashLoopBackOff":
			crashLoop = preferredIssue(crashLoop, issue)
		case "OOMKilled":
			oom = preferredIssue(oom, issue)
		case "ProbeFailure":
			probe = preferredIssue(probe, issue)
		case "Error":
			containerError = preferredIssue(containerError, issue)
		case "ImagePullBackOff", "ErrImagePull":
			imagePull = preferredIssue(imagePull, issue)
		case "HighRestartCount":
			highRestart = preferredIssue(highRestart, issue)
		}
	}

	if crashLoop != nil && oom != nil {
		out := *crashLoop
		out.Reason = "CrashLoopBackOff (OOMKilled)"
		out.Message = "Container termination state reports OOMKilled; the pod is currently in CrashLoopBackOff."
		return out, true
	}
	if crashLoop != nil && (probeFailure || probe != nil) {
		out := *crashLoop
		out.Reason = "CrashLoopBackOff (ProbeFailure)"
		out.Message = fmt.Sprintf("Container %s: Kubernetes events show repeated startup/liveness probe failures followed by container restarts. Investigate probe configuration and actual startup time", crashLoopContainer(*crashLoop))
		return out, true
	}
	if crashLoop != nil {
		return *crashLoop, true
	}
	if oom != nil {
		return *oom, true
	}
	if probe != nil {
		return *probe, true
	}
	if containerError != nil {
		return *containerError, true
	}
	if imagePull != nil {
		return *imagePull, true
	}
	if highRestart != nil {
		return *highRestart, true
	}
	return models.EmergencyIssue{}, false
}

// preferredIssue makes the representative evidence stable when multiple
// containers report the same reason. Prefer the highest restart count, then
// the lexicographically smaller container and message as deterministic ties.
func preferredIssue(current, candidate *models.EmergencyIssue) *models.EmergencyIssue {
	if current == nil || candidate.Restarts > current.Restarts {
		return candidate
	}
	if candidate.Restarts < current.Restarts {
		return current
	}
	if candidate.Container != current.Container {
		if candidate.Container < current.Container {
			return candidate
		}
		return current
	}
	if candidate.Message < current.Message {
		return candidate
	}
	return current
}

func crashLoopContainer(issue models.EmergencyIssue) string {
	if issue.Container != "" {
		return issue.Container
	}
	if match := crashLoopContainerRE.FindStringSubmatch(issue.Message); len(match) == 2 {
		return match[1]
	}
	return "container"
}
