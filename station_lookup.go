package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Last-known station positions.
//
// The in-memory packet history is a small ring (maxHistory packets), so a station that beacons every
// 10-30 minutes falls out of it quickly. Deep links such as /?call=2E0LXY-13 need the last known
// position regardless, so keep a bounded per-callsign track (latest position + a short trail),
// pruned by age and persisted to disk so it survives restarts.

const (
	maxLastPos       = 50000
	lastPosTrailMax  = 20
	lastPosTTL       = 30 * 24 * time.Hour
	lastPosFile      = "lastpos.json"
	lastPosSaveEvery = 5 * time.Minute
)

type stationTrack struct {
	Call  string          `json:"call"`
	Trail []HistoryPacket `json:"trail"` // oldest -> newest, newest is the last known position
}

var (
	lastPosMu    sync.RWMutex
	lastPos      = map[string]*stationTrack{}
	lastPosDirty bool

	stationCallRe = regexp.MustCompile(`^[A-Z0-9/-]{1,15}$`)
)

// recordLastPos stores a position packet as the latest known position of its station.
func recordLastPos(p HistoryPacket) {
	key := strings.ToUpper(strings.TrimSpace(p.Callsign))
	if key == "" {
		return
	}
	lastPosMu.Lock()
	defer lastPosMu.Unlock()
	t := lastPos[key]
	if t == nil {
		t = &stationTrack{Call: p.Callsign}
		lastPos[key] = t
	}
	t.Call = p.Callsign
	// Skip exact duplicates (digipeated copies of the same beacon).
	if n := len(t.Trail); n > 0 && t.Trail[n-1].Timestamp == p.Timestamp && t.Trail[n-1].Lat == p.Lat && t.Trail[n-1].Lon == p.Lon {
		return
	}
	t.Trail = append(t.Trail, p)
	if len(t.Trail) > lastPosTrailMax {
		t.Trail = t.Trail[len(t.Trail)-lastPosTrailMax:]
	}
	lastPosDirty = true
	if len(lastPos) > maxLastPos+1000 {
		pruneLastPosLocked(time.Now())
	}
}

// pruneLastPosLocked drops expired stations, then the oldest ones above the cap. Caller holds lastPosMu.
func pruneLastPosLocked(now time.Time) {
	cutoff := now.Add(-lastPosTTL).Unix()
	for k, t := range lastPos {
		if len(t.Trail) == 0 || t.Trail[len(t.Trail)-1].Timestamp < cutoff {
			delete(lastPos, k)
		}
	}
	if len(lastPos) <= maxLastPos {
		return
	}
	type kv struct {
		k  string
		ts int64
	}
	all := make([]kv, 0, len(lastPos))
	for k, t := range lastPos {
		all = append(all, kv{k, t.Trail[len(t.Trail)-1].Timestamp})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ts < all[j].ts })
	for _, e := range all[:len(all)-maxLastPos] {
		delete(lastPos, e.k)
	}
}

// lookupStation finds a station by callsign (case-insensitive). An exact match wins; a call without an
// SSID falls back to the most recently heard SSID variant. Returns packets oldest -> newest.
func lookupStation(call string) (string, []HistoryPacket) {
	call = strings.ToUpper(strings.TrimSpace(call))
	lastPosMu.RLock()
	defer lastPosMu.RUnlock()
	t := lastPos[call]
	if t == nil && !strings.Contains(call, "-") {
		var best *stationTrack
		var bestTs int64
		prefix := call + "-"
		for k, c := range lastPos {
			if strings.HasPrefix(k, prefix) && len(c.Trail) > 0 {
				if ts := c.Trail[len(c.Trail)-1].Timestamp; best == nil || ts > bestTs {
					best, bestTs = c, ts
				}
			}
		}
		t = best
	}
	if t == nil || len(t.Trail) == 0 {
		return "", nil
	}
	out := make([]HistoryPacket, len(t.Trail))
	copy(out, t.Trail)
	return t.Call, out
}

// GET /api/station?call=2E0LXY-13 - public: the same positions are already broadcast over the WebSocket.
func handleStationLookup(w http.ResponseWriter, r *http.Request) {
	call := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("call")))
	w.Header().Set("Content-Type", "application/json")
	if !stationCallRe.MatchString(call) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid callsign"})
		return
	}
	name, pkts := lookupStation(call)
	if len(pkts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "station not heard recently"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"call": name, "packets": pkts})
}

// ── persistence ───────────────────────────────────────────────────────────────

func saveLastPos(path string) error {
	lastPosMu.Lock()
	pruneLastPosLocked(time.Now())
	list := make([]*stationTrack, 0, len(lastPos))
	for _, t := range lastPos {
		list = append(list, t)
	}
	b, err := json.Marshal(list)
	lastPosDirty = false
	lastPosMu.Unlock()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadLastPos(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var list []*stationTrack
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("lastpos: ignoring unreadable %s: %v", path, err)
		return
	}
	lastPosMu.Lock()
	defer lastPosMu.Unlock()
	for _, t := range list {
		if t != nil && t.Call != "" && len(t.Trail) > 0 {
			lastPos[strings.ToUpper(t.Call)] = t
		}
	}
	pruneLastPosLocked(time.Now())
	log.Printf("lastpos: loaded %d stations", len(lastPos))
}

// startLastPosPersistence loads the saved tracks and saves them periodically while dirty.
func startLastPosPersistence() {
	loadLastPos(lastPosFile)
	go func() {
		t := time.NewTicker(lastPosSaveEvery)
		defer t.Stop()
		for range t.C {
			lastPosMu.RLock()
			dirty := lastPosDirty
			lastPosMu.RUnlock()
			if dirty {
				if err := saveLastPos(lastPosFile); err != nil {
					log.Printf("lastpos: save failed: %v", err)
				}
			}
		}
	}()
}
