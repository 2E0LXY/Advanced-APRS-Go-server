package main

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	igateHistoryFile         = "igate_history.json"
	igateHistoryRetention    = 24 * time.Hour
	maxHeardStationsPerIGate = 64
	igateHistorySaveInterval = 30 * time.Second
)

type IGateHeardEvent struct {
	Timestamp      int64   `json:"ts"`
	FirstTimestamp int64   `json:"first_ts,omitempty"`
	Packets        int     `json:"packets"`
	RSSI           int     `json:"rssi"`
	SNR            float64 `json:"snr"`
	FrequencyError int     `json:"freq_error"`
}

type IGateHeardState struct {
	MemberID          string            `json:"member_id"`
	DeviceCall        string            `json:"device_call"`
	Callsign          string            `json:"callsign"`
	LastDevicePackets int               `json:"last_device_packets"`
	LastDeviceUptime  int64             `json:"last_device_uptime"`
	LastHeard         int64             `json:"last_heard"`
	LastRSSI          int               `json:"last_rssi"`
	AverageRSSI       float64           `json:"avg_rssi"`
	MinimumRSSI       int               `json:"min_rssi"`
	MaximumRSSI       int               `json:"max_rssi"`
	LastSNR           float64           `json:"last_snr"`
	AverageSNR        float64           `json:"avg_snr"`
	MinimumSNR        float64           `json:"min_snr"`
	MaximumSNR        float64           `json:"max_snr"`
	FrequencyError    int               `json:"freq_error"`
	Events            []IGateHeardEvent `json:"events"`
}

type IGateHeardSummary struct {
	Callsign       string  `json:"callsign"`
	Packets        int     `json:"packets"`
	FirstHeard     int64   `json:"first_heard"`
	LastHeard      int64   `json:"last_heard"`
	LastRSSI       int     `json:"last_rssi"`
	AverageRSSI    float64 `json:"avg_rssi"`
	MinimumRSSI    int     `json:"min_rssi"`
	MaximumRSSI    int     `json:"max_rssi"`
	LastSNR        float64 `json:"last_snr"`
	AverageSNR     float64 `json:"avg_snr"`
	MinimumSNR     float64 `json:"min_snr"`
	MaximumSNR     float64 `json:"max_snr"`
	FrequencyError int     `json:"freq_error"`
	DistanceMiles  float64 `json:"distance_miles,omitempty"`
	HasDistance    bool    `json:"has_distance"`
}

var (
	igateHistoryMu       sync.RWMutex
	igateHistory         = make(map[string]*IGateHeardState)
	lastIGateHistorySave time.Time
)

func heardStateKey(memberID, deviceCall, stationCall string) string {
	return memberID + "|" + strings.ToUpper(deviceCall) + "|" + strings.ToUpper(stationCall)
}

func loadIGateHistory() {
	data, err := os.ReadFile(igateHistoryFile)
	if err != nil {
		return
	}
	var saved map[string]*IGateHeardState
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	cutoff := time.Now().Add(-igateHistoryRetention).Unix()
	for key, state := range saved {
		pruneHeardEvents(state, cutoff)
		if len(state.Events) == 0 && state.LastHeard < cutoff {
			delete(saved, key)
		}
	}
	igateHistoryMu.Lock()
	igateHistory = saved
	igateHistoryMu.Unlock()
}

func saveIGateHistoryLocked() {
	if !lastIGateHistorySave.IsZero() && time.Since(lastIGateHistorySave) < igateHistorySaveInterval {
		return
	}
	data, err := json.Marshal(igateHistory)
	if err != nil {
		return
	}
	tmp := igateHistoryFile + ".tmp"
	if os.WriteFile(tmp, data, 0600) != nil {
		return
	}
	if err := os.Rename(tmp, igateHistoryFile); err != nil {
		// Windows cannot replace an existing file. Production Linux uses the
		// atomic rename path; this fallback keeps local validation functional.
		_ = os.Remove(igateHistoryFile)
		_ = os.Rename(tmp, igateHistoryFile)
	}
	lastIGateHistorySave = time.Now()
}

func igateStationCountLocked(memberID, deviceCall string) int {
	count := 0
	for _, state := range igateHistory {
		if state.MemberID == memberID && strings.EqualFold(state.DeviceCall, deviceCall) {
			count++
		}
	}
	return count
}

func telemetryInt(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func telemetryFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func updateIGateHeard(memberID, deviceCall string, deviceUptime int64, raw interface{}) {
	entries, ok := raw.([]interface{})
	if !ok {
		return
	}
	now := time.Now().Unix()
	cutoff := now - int64(igateHistoryRetention.Seconds())
	igateHistoryMu.Lock()
	defer igateHistoryMu.Unlock()

	for index, entry := range entries {
		if index >= maxHeardStationsPerIGate {
			break
		}
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		call, _ := m["callsign"].(string)
		call = strings.ToUpper(strings.TrimSpace(call))
		if !validMQTTCall(call) {
			continue
		}
		packets := telemetryInt(m, "packets")
		firstUptime := int64(telemetryInt(m, "first_uptime"))
		lastUptime := int64(telemetryInt(m, "last_uptime"))
		lastHeard := now
		if lastUptime > 0 && deviceUptime >= lastUptime {
			lastHeard = now - (deviceUptime - lastUptime)
		}
		if lastHeard > now {
			lastHeard = now
		}

		key := heardStateKey(memberID, deviceCall, call)
		state := igateHistory[key]
		newState := state == nil
		if newState {
			if igateStationCountLocked(memberID, deviceCall) >= maxHeardStationsPerIGate {
				continue
			}
			state = &IGateHeardState{MemberID: memberID, DeviceCall: strings.ToUpper(deviceCall), Callsign: call}
			igateHistory[key] = state
		}

		delta := 0
		if newState {
			// When the station was first heard within this boot's 24-hour
			// window, every current counter increment belongs to the window.
			if deviceUptime <= int64(igateHistoryRetention.Seconds()) ||
				(firstUptime > 0 && deviceUptime >= firstUptime && deviceUptime-firstUptime <= int64(igateHistoryRetention.Seconds())) {
				delta = packets
			}
		} else if deviceUptime < state.LastDeviceUptime || packets < state.LastDevicePackets {
			// Device reboot or counter rollover.
			delta = packets
		} else {
			delta = packets - state.LastDevicePackets
		}
		if delta > 0 && lastHeard >= cutoff {
			firstHeard := lastHeard
			if firstUptime > 0 && deviceUptime >= firstUptime &&
				(newState || deviceUptime < state.LastDeviceUptime || packets < state.LastDevicePackets) {
				firstHeard = now - (deviceUptime - firstUptime)
			} else if !newState && state.LastHeard >= cutoff && state.LastHeard < lastHeard {
				firstHeard = state.LastHeard
			}
			if firstHeard < cutoff {
				firstHeard = cutoff
			}
			state.Events = append(state.Events, IGateHeardEvent{
				Timestamp: lastHeard, FirstTimestamp: firstHeard, Packets: delta,
				RSSI: telemetryInt(m, "last_rssi"), SNR: telemetryFloat(m, "last_snr"),
				FrequencyError: telemetryInt(m, "freq_error"),
			})
		}

		state.LastDevicePackets = packets
		state.LastDeviceUptime = deviceUptime
		state.LastHeard = lastHeard
		state.LastRSSI = telemetryInt(m, "last_rssi")
		state.AverageRSSI = telemetryFloat(m, "avg_rssi")
		state.MinimumRSSI = telemetryInt(m, "min_rssi")
		state.MaximumRSSI = telemetryInt(m, "max_rssi")
		state.LastSNR = telemetryFloat(m, "last_snr")
		state.AverageSNR = telemetryFloat(m, "avg_snr")
		state.MinimumSNR = telemetryFloat(m, "min_snr")
		state.MaximumSNR = telemetryFloat(m, "max_snr")
		state.FrequencyError = telemetryInt(m, "freq_error")
		pruneHeardEvents(state, cutoff)
	}

	for key, state := range igateHistory {
		pruneHeardEvents(state, cutoff)
		if len(state.Events) == 0 && state.LastHeard < cutoff {
			delete(igateHistory, key)
		}
	}
	saveIGateHistoryLocked()
}

func pruneHeardEvents(state *IGateHeardState, cutoff int64) {
	kept := state.Events[:0]
	for _, event := range state.Events {
		if event.Timestamp >= cutoff {
			kept = append(kept, event)
		}
	}
	state.Events = kept
}

func buildIGateHeardSummaries(memberID, deviceCall string, deviceLat, deviceLon float64) []IGateHeardSummary {
	cutoff := time.Now().Add(-igateHistoryRetention).Unix()
	igateHistoryMu.RLock()
	var out []IGateHeardSummary
	for _, state := range igateHistory {
		if state.MemberID != memberID || !strings.EqualFold(state.DeviceCall, deviceCall) || state.LastHeard < cutoff {
			continue
		}
		summary := IGateHeardSummary{Callsign: state.Callsign}
		var weightedRSSI, weightedSNR float64
		for _, event := range state.Events {
			if event.Timestamp < cutoff {
				continue
			}
			if summary.Packets == 0 {
				summary.MinimumRSSI, summary.MaximumRSSI = event.RSSI, event.RSSI
				summary.MinimumSNR, summary.MaximumSNR = event.SNR, event.SNR
			} else {
				if event.RSSI < summary.MinimumRSSI {
					summary.MinimumRSSI = event.RSSI
				}
				if event.RSSI > summary.MaximumRSSI {
					summary.MaximumRSSI = event.RSSI
				}
				if event.SNR < summary.MinimumSNR {
					summary.MinimumSNR = event.SNR
				}
				if event.SNR > summary.MaximumSNR {
					summary.MaximumSNR = event.SNR
				}
			}
			weightedRSSI += float64(event.RSSI * event.Packets)
			weightedSNR += event.SNR * float64(event.Packets)
			summary.Packets += event.Packets
			eventFirst := event.FirstTimestamp
			if eventFirst == 0 {
				eventFirst = event.Timestamp
			}
			if summary.FirstHeard == 0 || eventFirst < summary.FirstHeard {
				summary.FirstHeard = eventFirst
			}
			if event.Timestamp >= summary.LastHeard {
				summary.LastHeard = event.Timestamp
				summary.LastRSSI = event.RSSI
				summary.LastSNR = event.SNR
				summary.FrequencyError = event.FrequencyError
			}
		}
		if summary.Packets == 0 {
			continue
		}
		summary.AverageRSSI = weightedRSSI / float64(summary.Packets)
		summary.AverageSNR = weightedSNR / float64(summary.Packets)
		if deviceLat != 0 || deviceLon != 0 {
			if stationLat, stationLon, ok := latestStationPosition(state.Callsign); ok {
				summary.DistanceMiles = haversineKm(deviceLat, deviceLon, stationLat, stationLon) * 0.621371
				summary.HasDistance = true
			}
		}
		out = append(out, summary)
	}
	igateHistoryMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastHeard > out[j].LastHeard })
	if out == nil {
		return []IGateHeardSummary{}
	}
	return out
}

func latestStationPosition(callsign string) (float64, float64, bool) {
	historyMu.RLock()
	defer historyMu.RUnlock()
	var latest HistoryPacket
	found := false
	for _, packet := range history {
		if strings.EqualFold(packet.Callsign, callsign) && (!found || packet.Timestamp > latest.Timestamp) {
			latest = packet
			found = true
		}
	}
	if !found || (latest.Lat == 0 && latest.Lon == 0) {
		return 0, 0, false
	}
	return latest.Lat, latest.Lon, true
}
