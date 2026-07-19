package main

import "testing"

func TestDownsamplePerformanceKeepsEndpoints(t *testing.T) {
	samples := make([]PerformanceSample, 1000)
	for i := range samples {
		samples[i].Timestamp = int64(i + 1)
	}
	got := downsamplePerformance(samples, 100)
	if len(got) != 100 {
		t.Fatalf("got %d samples, want 100", len(got))
	}
	if got[0].Timestamp != 1 || got[len(got)-1].Timestamp != 1000 {
		t.Fatalf("endpoints not preserved: first=%d last=%d", got[0].Timestamp, got[len(got)-1].Timestamp)
	}
}

func TestBuildPerformanceIssuesDetectsAndClosesQueuePressure(t *testing.T) {
	samples := []PerformanceSample{
		{Timestamp: 100, BroadcastQueue: 4000, BroadcastQueueCap: 5000, UpstreamConnected: true},
		{Timestamp: 160, BroadcastQueue: 100, BroadcastQueueCap: 5000, UpstreamConnected: true},
	}
	issues := buildPerformanceIssues(samples)
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	if issues[0].Key != "queue" || issues[0].Active || issues[0].Severity != "warning" {
		t.Fatalf("unexpected issue: %+v", issues[0])
	}
	if issues[0].EndedAt != 100 {
		t.Fatalf("ended_at=%d, want 100", issues[0].EndedAt)
	}
}

func TestBuildPerformanceIssuesDetectsRestart(t *testing.T) {
	samples := []PerformanceSample{
		{Timestamp: 100, UptimeSeconds: 500, UpstreamConnected: true},
		{Timestamp: 160, UptimeSeconds: 20, UpstreamConnected: true},
	}
	issues := buildPerformanceIssues(samples)
	if len(issues) != 1 || issues[0].Key != "restart" || issues[0].Severity != "info" {
		t.Fatalf("restart issue not detected: %+v", issues)
	}
}

func TestPublicIssueThresholdsRemainQuietUnderNormalLoad(t *testing.T) {
	s := PerformanceSample{
		HostCPUPercent:       12,
		SystemMemoryBytes:    1024,
		AvailableMemoryBytes: 700,
		DiskTotalBytes:       1000,
		DiskFreeBytes:        600,
		Goroutines:           50,
		UpstreamConnected:    true,
		BroadcastQueueCap:    5000,
		UpstreamQueueCap:     5000,
		UpstreamTxQueueCap:   5000,
	}
	if got := sampleConditions(s); len(got) != 0 {
		t.Fatalf("normal sample produced issues: %+v", got)
	}
}
