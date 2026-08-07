package store

import (
	"testing"
	"time"
)

func TestDeriveTrendObservedBaselineAndResets(t *testing.T) {
	now := time.Now().Unix()
	tests := []struct {
		name    string
		input   trendInput
		current int
		want    string
	}{
		{
			name: "non-zero detected count accelerating",
			input: trendInput{hasDetected: true,
				firstDetected: trendEvent{occurredAt: now - 30*86400, restartCount: 8820},
				recent:        []trendEvent{{occurredAt: now - 3600, restartCount: 10631}, {occurredAt: now - 2*3600, restartCount: 10531}}},
			current: 10631, want: "accelerating",
		},
		{
			name: "steady high rate stable",
			input: trendInput{hasDetected: true,
				firstDetected: trendEvent{occurredAt: now - 10*86400, restartCount: 10000},
				recent:        []trendEvent{{occurredAt: now - 3600, restartCount: 11000}, {occurredAt: now - 25*3600, restartCount: 10900}}},
			current: 11000, want: "stable",
		},
		{
			name: "recent reset stable",
			input: trendInput{hasDetected: true,
				firstDetected: trendEvent{occurredAt: now - 10*86400, restartCount: 100},
				recent:        []trendEvent{{occurredAt: now - 3600, restartCount: 3}, {occurredAt: now - 2*3600, restartCount: 1000}}},
			current: 3, want: "stable",
		},
		{
			name: "insufficient observations stable",
			input: trendInput{hasDetected: true, firstDetected: trendEvent{occurredAt: now - 86400, restartCount: 50},
				recent: []trendEvent{{occurredAt: now - 3600, restartCount: 60}}},
			current: 60, want: "stable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveTrend(tt.input, tt.current); got != tt.want {
				t.Fatalf("deriveTrend() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRestartTrendAppliesStaticTypesAreInapplicable(t *testing.T) {
	for _, issueType := range []string{"pending", "image_pull_backoff", "privileged_container", "pvc_pending"} {
		if RestartTrendApplies(issueType) {
			t.Errorf("RestartTrendApplies(%q) = true", issueType)
		}
	}
}

func TestExtendedRestartMilestoneBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		prev, curr int
		want       int
	}{
		{"cross 10000", 9999, 10000, 10000},
		{"no duplicate 10000", 10000, 10001, 0},
		{"cross 25000", 10000, 25000, 25000},
		{"highest of multiple", 9999, 60000, 50000},
		{"nothing beyond maximum", 100000, 200000, 0},
		{"new maximum crossed", 99999, 200000, 100000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := highestMilestoneCrossed(tt.prev, tt.curr); got != tt.want {
				t.Fatalf("highestMilestoneCrossed(%d, %d) = %d, want %d", tt.prev, tt.curr, got, tt.want)
			}
		})
	}
}
