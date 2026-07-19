package main

import (
	"os"
	"testing"
	"time"
)

func withIGateHistoryTempDir(t *testing.T) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		igateHistoryMu.Lock()
		igateHistory = make(map[string]*IGateHeardState)
		lastIGateHistorySave = time.Time{}
		igateHistoryMu.Unlock()
	})
	igateHistoryMu.Lock()
	igateHistory = make(map[string]*IGateHeardState)
	lastIGateHistorySave = time.Time{}
	igateHistoryMu.Unlock()
}

func heardTelemetry(call string, packets, firstUptime, lastUptime int) []interface{} {
	return []interface{}{map[string]interface{}{
		"callsign": call, "packets": float64(packets),
		"first_uptime": float64(firstUptime), "last_uptime": float64(lastUptime),
		"last_rssi": float64(-71), "avg_rssi": float64(-76.5),
		"min_rssi": float64(-91), "max_rssi": float64(-62),
		"last_snr": float64(7.5), "avg_snr": float64(5.25),
		"min_snr": float64(-1.0), "max_snr": float64(9.5),
		"freq_error": float64(315),
	}}
}

func TestIGateHeardRollingCounterDeltas(t *testing.T) {
	withIGateHistoryTempDir(t)
	updateIGateHeard("member-1", "2E0LXY-9", 600, heardTelemetry("M0ABC-7", 5, 120, 590))
	updateIGateHeard("member-1", "2E0LXY-9", 660, heardTelemetry("M0ABC-7", 7, 120, 650))

	summaries := buildIGateHeardSummaries("member-1", "2E0LXY-9", 0, 0)
	if len(summaries) != 1 {
		t.Fatalf("got %d stations, want 1", len(summaries))
	}
	got := summaries[0]
	if got.Packets != 7 {
		t.Fatalf("packets=%d, want 7", got.Packets)
	}
	if got.Callsign != "M0ABC-7" || got.LastRSSI != -71 || got.AverageSNR != 7.5 {
		t.Fatalf("unexpected summary: %+v", got)
	}
	if got.FirstHeard == 0 || got.LastHeard < got.FirstHeard {
		t.Fatalf("invalid heard times: %+v", got)
	}
	if got.LastHeard-got.FirstHeard < 400 {
		t.Fatalf("first-heard time did not use firmware uptime: %+v", got)
	}
}

func TestIGateHeardRebootStartsNewCounterSeries(t *testing.T) {
	withIGateHistoryTempDir(t)
	updateIGateHeard("member-1", "2E0LXY-10", 7200, heardTelemetry("G1XYZ", 8, 10, 7190))
	updateIGateHeard("member-1", "2E0LXY-10", 30, heardTelemetry("G1XYZ", 2, 5, 25))

	summaries := buildIGateHeardSummaries("member-1", "2E0LXY-10", 0, 0)
	if len(summaries) != 1 || summaries[0].Packets != 10 {
		t.Fatalf("unexpected reboot summary: %+v", summaries)
	}
}

func TestPruneHeardEventsDoesNotAssumeSortedInput(t *testing.T) {
	cutoff := time.Now().Unix()
	state := &IGateHeardState{Events: []IGateHeardEvent{
		{Timestamp: cutoff + 2, Packets: 1},
		{Timestamp: cutoff - 2, Packets: 9},
		{Timestamp: cutoff + 1, Packets: 2},
	}}
	pruneHeardEvents(state, cutoff)
	if len(state.Events) != 2 || state.Events[0].Packets != 1 || state.Events[1].Packets != 2 {
		t.Fatalf("unexpected events after prune: %+v", state.Events)
	}
}

func TestValidMQTTCall(t *testing.T) {
	for _, call := range []string{"2E0LXY-9", "GB7SR", "M0ABC-15"} {
		if !validMQTTCall(call) {
			t.Errorf("valid call rejected: %s", call)
		}
	}
	for _, call := range []string{"", "bad/call", "CALL<script>", "ABCDEFGHIJKLMNOP"} {
		if validMQTTCall(call) {
			t.Errorf("invalid call accepted: %s", call)
		}
	}
}

func TestEmptyIGateHistoryReturnsJSONArray(t *testing.T) {
	withIGateHistoryTempDir(t)
	if got := buildIGateHeardSummaries("member-1", "QA1ABC-1", 0, 0); got == nil || len(got) != 0 {
		t.Fatalf("empty history=%#v, want non-nil empty slice", got)
	}
}

func TestIGateHeardStationCountIsBounded(t *testing.T) {
	withIGateHistoryTempDir(t)
	entries := make([]interface{}, 0, maxHeardStationsPerIGate+10)
	for i := 0; i < maxHeardStationsPerIGate+10; i++ {
		entries = append(entries, map[string]interface{}{
			"callsign": "QA" + string(rune('A'+(i/10))) + string(rune('0'+(i%10))),
			"packets":  float64(1), "first_uptime": float64(1), "last_uptime": float64(2),
		})
	}
	updateIGateHeard("member-1", "QA1ABC-1", 3, entries)
	if got := len(buildIGateHeardSummaries("member-1", "QA1ABC-1", 0, 0)); got != maxHeardStationsPerIGate {
		t.Fatalf("station count=%d, want capped at %d", got, maxHeardStationsPerIGate)
	}
}
