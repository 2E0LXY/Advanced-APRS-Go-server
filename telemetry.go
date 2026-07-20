package main

// telemetry.go — APRS T# telemetry packet parsing, storage, and dashboard API.
//
// APRS telemetry format:
//   T#SEQ,CHAN1,CHAN2,CHAN3,CHAN4,CHAN5,BITS
//   PARAM callsign:PARM,Name1,Name2,Name3,Name4,Name5,D1,D2,D3,D4,D5,D6,D7,D8
//   UNIT  callsign:UNIT,Unit1,Unit2,Unit3,Unit4,Unit5
//   EQNS  callsign:EQNS,a1,b1,c1,...
//
// Stores a 24-hour ring of samples per callsign. Exposed at:
//   GET /api/telemetry/{callsign}  — returns samples + metadata for dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	telemMaxSamples  = 24 * 60 // one sample per minute for 24 h
	telemRetention   = 24 * time.Hour
)

type TelemSample struct {
	Timestamp int64      `json:"ts"`
	Seq       string     `json:"seq"`
	Channels  [5]float64 `json:"ch"`  // scaled values (EQNS applied)
	Bits      [8]bool    `json:"bits"`
}

type TelemMeta struct {
	Names [5]string  `json:"names"` // PARM names
	Units [5]string  `json:"units"` // UNIT strings
	// EQNS: value = a*raw^2 + b*raw + c
	A [5]float64 `json:"a"`
	B [5]float64 `json:"b"`
	C [5]float64 `json:"c"`
}

type TelemState struct {
	Callsign string       `json:"callsign"`
	Meta     TelemMeta    `json:"meta"`
	Samples  []TelemSample `json:"samples"`
	LastSeen int64         `json:"last_seen"`
}

var (
	telemMu    sync.RWMutex
	telemStore = make(map[string]*TelemState) // key: uppercase callsign
)

// parseTelemPacket extracts callsign and info from a raw APRS packet.
// Returns ("", "", false) if not a telemetry packet.
func parseTelemPacket(raw string) (callsign, info string, ok bool) {
	idx := strings.Index(raw, ">")
	if idx <= 0 {
		return "", "", false
	}
	call := strings.ToUpper(raw[:idx])
	rest := raw[idx+1:]

	// Find the payload after ':'
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return "", "", false
	}
	payload := rest[colon+1:]

	if strings.HasPrefix(payload, "T#") ||
		strings.HasPrefix(payload, "PARM.") ||
		strings.HasPrefix(payload, "UNIT.") ||
		strings.HasPrefix(payload, "EQNS.") ||
		strings.HasPrefix(payload, "BITS.") {
		return call, payload, true
	}
	return "", "", false
}

// ingestTelemPacket is called for every received APRS packet.
func ingestTelemPacket(raw string) {
	call, payload, ok := parseTelemPacket(raw)
	if !ok {
		return
	}

	telemMu.Lock()
	state := telemStore[call]
	if state == nil {
		state = &TelemState{
			Callsign: call,
			Meta:     TelemMeta{B: [5]float64{1, 1, 1, 1, 1}}, // default: no scaling
		}
		telemStore[call] = state
	}

	switch {
	case strings.HasPrefix(payload, "T#"):
		parseTelemetryValues(state, payload)
	case strings.HasPrefix(payload, "PARM."):
		parseTelemPARM(state, payload[5:])
	case strings.HasPrefix(payload, "UNIT."):
		parseTelemUNIT(state, payload[5:])
	case strings.HasPrefix(payload, "EQNS."):
		parseTelemEQNS(state, payload[5:])
	}
	state.LastSeen = time.Now().Unix()
	telemMu.Unlock()
}

func parseTelemetryValues(state *TelemState, payload string) {
	// T#SEQ,CH1,CH2,CH3,CH4,CH5,BITS
	payload = strings.TrimPrefix(payload, "T#")
	parts := strings.SplitN(payload, ",", 8)
	if len(parts) < 6 {
		return
	}
	seq := strings.TrimSpace(parts[0])
	var rawVals [5]float64
	for i := 1; i <= 5 && i < len(parts); i++ {
		v, _ := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
		rawVals[i-1] = v
	}

	var scaled [5]float64
	meta := state.Meta
	for i := 0; i < 5; i++ {
		r := rawVals[i]
		// EQNS: value = a*r^2 + b*r + c
		scaled[i] = meta.A[i]*r*r + meta.B[i]*r + meta.C[i]
		// Round to 3 decimal places
		scaled[i] = math.Round(scaled[i]*1000) / 1000
	}

	var bits [8]bool
	if len(parts) >= 7 {
		bitsStr := strings.TrimSpace(parts[6])
		for i := 0; i < 8 && i < len(bitsStr); i++ {
			bits[i] = bitsStr[i] == '1'
		}
	}

	sample := TelemSample{
		Timestamp: time.Now().Unix(),
		Seq:       seq,
		Channels:  scaled,
		Bits:      bits,
	}

	// Prune old samples
	cutoff := time.Now().Add(-telemRetention).Unix()
	kept := state.Samples[:0]
	for _, s := range state.Samples {
		if s.Timestamp >= cutoff {
			kept = append(kept, s)
		}
	}
	if len(kept) >= telemMaxSamples {
		kept = kept[1:]
	}
	state.Samples = append(kept, sample)
}

func parseTelemPARM(state *TelemState, csv string) {
	parts := splitCSV(csv, 13)
	for i := 0; i < 5 && i < len(parts); i++ {
		state.Meta.Names[i] = strings.TrimSpace(parts[i])
	}
}

func parseTelemUNIT(state *TelemState, csv string) {
	parts := splitCSV(csv, 13)
	for i := 0; i < 5 && i < len(parts); i++ {
		state.Meta.Units[i] = strings.TrimSpace(parts[i])
	}
}

func parseTelemEQNS(state *TelemState, csv string) {
	// 15 values: a1,b1,c1,a2,b2,c2,...
	parts := splitCSV(csv, 15)
	for i := 0; i < 5; i++ {
		base := i * 3
		if base+2 >= len(parts) {
			break
		}
		a, _ := strconv.ParseFloat(strings.TrimSpace(parts[base]), 64)
		b, _ := strconv.ParseFloat(strings.TrimSpace(parts[base+1]), 64)
		c, _ := strconv.ParseFloat(strings.TrimSpace(parts[base+2]), 64)
		state.Meta.A[i] = a
		state.Meta.B[i] = b
		state.Meta.C[i] = c
	}
}

func splitCSV(s string, maxParts int) []string {
	parts := strings.SplitN(s, ",", maxParts)
	return parts
}

// ── HTTP: GET /api/telemetry/{callsign} ────────────────────────────────────

func handleTelemetry(w http.ResponseWriter, r *http.Request) {
	call := strings.ToUpper(strings.TrimPrefix(r.URL.Path, "/api/telemetry/"))
	call = strings.TrimSpace(call)
	if call == "" {
		// Return list of all callsigns that have telemetry
		telemMu.RLock()
		var list []map[string]interface{}
		for _, s := range telemStore {
			if len(s.Samples) > 0 {
				list = append(list, map[string]interface{}{
					"callsign":  s.Callsign,
					"samples":   len(s.Samples),
					"last_seen": s.LastSeen,
				})
			}
		}
		telemMu.RUnlock()
		if list == nil {
			list = []map[string]interface{}{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
		return
	}

	telemMu.RLock()
	state := telemStore[call]
	telemMu.RUnlock()

	if state == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "no telemetry for " + call})
		return
	}

	telemMu.RLock()
	cp := *state
	cp.Samples = append([]TelemSample(nil), state.Samples...)
	telemMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cp)
}
