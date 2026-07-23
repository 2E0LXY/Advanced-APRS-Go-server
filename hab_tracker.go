package main

// hab_tracker.go — High Altitude Balloon (HAB) flight detection and tracking.
//
// Detection heuristics (any one triggers HAB mode for a callsign):
//   - Altitude > 1500 m  AND  vertical speed > +1 m/s
//   - Altitude > 10000 m (already very high, even if just appeared)
//   - Comment contains "HAB", "balloon", "BALLOON", "payload"
//
// Once in HAB mode a callsign gets:
//   - A dedicated flight record (ascent history, burst detection, descent)
//   - Landing prediction via CUSF API (http://predict.cusf.co.uk)
//   - Push notification to any member who watches the callsign
//   - Map overlay: trajectory polyline + predicted landing marker
//
// API:
//   GET /api/hab/flights     — all active (last 6 h) HAB flights
//   GET /api/hab/{callsign}  — full flight record for one callsign

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── Data types ─────────────────────────────────────────────────────────────

type HABPoint struct {
	Timestamp int64   `json:"ts"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	AltM      float64 `json:"alt_m"`
	SpeedKph  float64 `json:"speed_kph"`
	Course    float64 `json:"course"`
	Comment   string  `json:"comment,omitempty"`
}

type HABPrediction struct {
	LandingLat   float64   `json:"landing_lat"`
	LandingLon   float64   `json:"landing_lon"`
	LandingTime  time.Time `json:"landing_time"`
	BurstAlt     float64   `json:"burst_alt_m"`
	FetchedAt    int64     `json:"fetched_at"`
	Trajectory   []HABPoint `json:"trajectory,omitempty"`
}

type HABFlight struct {
	Callsign    string        `json:"callsign"`
	Phase       string        `json:"phase"`   // "ascending" | "burst" | "descending" | "landed"
	LaunchTime  int64         `json:"launch_time"`
	BurstTime   int64         `json:"burst_time,omitempty"`
	BurstAltM   float64       `json:"burst_alt_m,omitempty"`
	MaxAltM     float64       `json:"max_alt_m"`
	LastAltM    float64       `json:"last_alt_m"`
	AscentRate  float64       `json:"ascent_rate_ms"` // m/s, positive=up
	Track       []HABPoint    `json:"track"`
	Prediction  *HABPrediction `json:"prediction,omitempty"`
	LastUpdated int64         `json:"last_updated"`
}

var (
	habMu      sync.RWMutex
	habFlights = make(map[string]*HABFlight) // key: uppercase callsign
)

// ── Detection ──────────────────────────────────────────────────────────────

// isHABCallsign returns true for common HAB callsign patterns.
func isHABCallsign(call, comment string) bool {
	lc := strings.ToLower(comment)
	if strings.Contains(lc, "hab") ||
		strings.Contains(lc, "balloon") ||
		strings.Contains(lc, "payload") ||
		strings.Contains(lc, "cusf") ||
		strings.Contains(lc, "horus") {
		return true
	}
	// Common HAB project prefixes
	upper := strings.ToUpper(call)
	for _, prefix := range []string{"M0SBU", "G0TDJ", "HORUS", "PE2BZ", "DL7AD", "OE5REO"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// ingestHABPacket is called for every parsed position packet.
// It extracts altitude, speed, course, and comment from the raw APRS string.
func ingestHABPacket(call string, lat, lon float64, raw string) {
	// Extract altitude from /A=NNNNNN in the comment field
	altM := 0.0
	speedKph := 0.0
	course := 0.0
	comment := ""

	// Find the payload after the first ':'
	if ci := strings.Index(raw, ":"); ci >= 0 {
		info := raw[ci+1:]
		comment = info

		// Altitude: /A=NNNNNN (feet)
		if ai := strings.Index(info, "/A="); ai >= 0 && ai+9 <= len(info) {
			var feet float64
			if n, _ := fmt.Sscanf(info[ai+3:ai+9], "%f", &feet); n == 1 {
				altM = feet * 0.3048
			}
		}

		// Speed/course from position report: CSEspd (knots) in first 7 chars after symbol
		// Format: DDDsss or just try to pull from known offsets
		// Simplified: look for CSE/SPD in compressed or uncompressed
		if len(info) > 10 {
			var cse, spd int
			if n, _ := fmt.Sscanf(info[1:4]+info[4:7], "%3d%3d", &cse, &spd); n == 2 {
				course = float64(cse)
				speedKph = float64(spd) * 1.852 // knots to km/h
			}
		}
	}
	if altM < 100 && !isHABCallsign(call, comment) {
		return
	}
	call = strings.ToUpper(call)
	now := time.Now().Unix()

	habMu.Lock()
	flight := habFlights[call]

	if flight == nil {
		// Only start tracking if heuristics are met
		if altM < 1500 && !isHABCallsign(call, comment) {
			habMu.Unlock()
			return
		}
		flight = &HABFlight{
			Callsign:   call,
			Phase:      "ascending",
			LaunchTime: now,
			MaxAltM:    altM,
		}
		habFlights[call] = flight
		habMu.Unlock()
		// Notify watching members
		go notifyHABStart(call, altM)
		habMu.Lock()
		flight = habFlights[call]
		if flight == nil {
			habMu.Unlock()
			return
		}
	}

	pt := HABPoint{
		Timestamp: now,
		Lat:       lat,
		Lon:       lon,
		AltM:      altM,
		SpeedKph:  speedKph,
		Course:    course,
		Comment:   comment,
	}

	// Calculate ascent rate from last point
	if len(flight.Track) > 0 {
		prev := flight.Track[len(flight.Track)-1]
		dt := float64(now - prev.Timestamp)
		if dt > 0 {
			flight.AscentRate = (altM - prev.AltM) / dt
		}
	}

	flight.Track = append(flight.Track, pt)
	flight.LastAltM = altM
	flight.LastUpdated = now
	if altM > flight.MaxAltM {
		flight.MaxAltM = altM
	}

	// Burst detection: was ascending, now descending, altitude > 5000m
	if flight.Phase == "ascending" && flight.AscentRate < -2.0 && altM > 5000 {
		flight.Phase = "burst"
		flight.BurstTime = now
		flight.BurstAltM = altM
	} else if flight.Phase == "burst" && flight.AscentRate < -1.0 {
		flight.Phase = "descending"
	} else if flight.Phase == "descending" && altM < 500 && math.Abs(flight.AscentRate) < 0.5 {
		flight.Phase = "landed"
	}

	// Prune track to last 1000 points
	if len(flight.Track) > 1000 {
		flight.Track = flight.Track[len(flight.Track)-1000:]
	}

	phase := flight.Phase
	burstAlt := flight.BurstAltM
	habMu.Unlock()

	// Fetch CUSF prediction asynchronously when descending
	if phase == "descending" || phase == "burst" {
		go fetchCUSFPrediction(call, lat, lon, altM, burstAlt)
	}
}

// ── CUSF Prediction API ────────────────────────────────────────────────────

func fetchCUSFPrediction(call string, lat, lon, altM, burstAlt float64) {
	// Rate-limit: only fetch every 2 minutes per flight
	habMu.RLock()
	flight := habFlights[call]
	habMu.RUnlock()
	if flight == nil {
		return
	}
	if flight.Prediction != nil && time.Now().Unix()-flight.Prediction.FetchedAt < 120 {
		return
	}

	// Estimate descent rate (typical ~6 m/s for standard parachute)
	descentRate := 6.0
	if flight.AscentRate < -1.0 {
		descentRate = math.Abs(flight.AscentRate)
	}
	if descentRate < 3.0 {
		descentRate = 3.0
	}

	apiURL := fmt.Sprintf(
		"http://predict.cusf.co.uk/api/v1/?lat=%f&lon=%f&alt=%f&time=now&ascent_rate=5&burst_altitude=%f&descent_rate=%f",
		lat, lon, altM, math.Max(burstAlt, altM+100), descentRate,
	)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Prediction []struct {
			Stage  string `json:"stage"`
			Traj   []struct {
				Lat       float64 `json:"latitude"`
				Lon       float64 `json:"longitude"`
				Alt       float64 `json:"altitude"`
				Datetime  string  `json:"datetime"`
			} `json:"trajectory"`
		} `json:"prediction"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Prediction) == 0 {
		return
	}

	pred := &HABPrediction{FetchedAt: time.Now().Unix()}
	for _, stage := range result.Prediction {
		for _, pt := range stage.Traj {
			pred.Trajectory = append(pred.Trajectory, HABPoint{
				Lat:  pt.Lat,
				Lon:  pt.Lon,
				AltM: pt.Alt,
			})
		}
	}

	// Landing = last point in trajectory
	if len(pred.Trajectory) > 0 {
		last := pred.Trajectory[len(pred.Trajectory)-1]
		pred.LandingLat = last.Lat
		pred.LandingLon = last.Lon
	}

	habMu.Lock()
	if f := habFlights[call]; f != nil {
		f.Prediction = pred
	}
	habMu.Unlock()
}

// ── Member notifications ───────────────────────────────────────────────────

func notifyHABStart(call string, altM float64) {
	memberStoreMu.RLock()
	members := memberStore.Members
	memberStoreMu.RUnlock()
	for _, m := range members {
		for _, watched := range m.Watchlist {
			if strings.EqualFold(watched, call) {
				go pushToMember(m.ID, PushPayload{
					Type:  "hab_start",
					Title: "HAB Launch: " + call,
					Body:  fmt.Sprintf("%s is airborne at %.0f m — tracking active", call, altM),
					Tag:   "hab_" + call,
				})
				go pushAlertToMember(m.Callsign, "hab_start", call,
					fmt.Sprintf("HAB %s is airborne at %.0f m", call, altM))
				break
			}
		}
	}
}

// purgeOldHABFlights removes flights not updated in the last 6 hours.
func purgeOldHABFlights() {
	for {
		time.Sleep(30 * time.Minute)
		cutoff := time.Now().Add(-6 * time.Hour).Unix()
		habMu.Lock()
		for k, f := range habFlights {
			if f.LastUpdated < cutoff {
				delete(habFlights, k)
			}
		}
		habMu.Unlock()
	}
}

// ── HTTP handlers ──────────────────────────────────────────────────────────

func handleHABFlights(w http.ResponseWriter, r *http.Request) {
	habMu.RLock()
	var list []*HABFlight
	for _, f := range habFlights {
		cp := *f
		list = append(list, &cp)
	}
	habMu.RUnlock()
	if list == nil {
		list = []*HABFlight{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func handleHABFlight(w http.ResponseWriter, r *http.Request) {
	call := strings.ToUpper(strings.TrimPrefix(r.URL.Path, "/api/hab/"))
	habMu.RLock()
	f := habFlights[call]
	var cp *HABFlight
	if f != nil {
		x := *f
		cp = &x
	}
	habMu.RUnlock()
	if cp == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "no active HAB flight for " + call})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cp)
}

func handleHAB(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/hab")
	if path == "" || path == "/" {
		handleHABFlights(w, r)
	} else {
		handleHABFlight(w, r)
	}
}
