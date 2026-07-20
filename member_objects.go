package main

// member_objects.go — Member-created APRS objects.
//
// Members can place named APRS objects on the map via the web UI.
// The server transmits them to APRS-IS every 10 minutes while active.
// Objects are private to the creating member and visible to all map viewers.
//
// APRS object format (per spec §11):
//   CALLSIGN>APNUK,TCPIP*:;OBJ_NAME_*DDHHMMzDDMM.hhN/DDDMM.hhW$Comment
//
// API:
//   GET    /api/member/objects         — list member's objects
//   POST   /api/member/objects         — create / update an object
//   DELETE /api/member/objects/{name}  — kill (remove) an object

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

type MemberObject struct {
	Name      string  `json:"name"`       // up to 9 chars, padded to 9 in packet
	MemberID  string  `json:"member_id"`
	Callsign  string  `json:"callsign"`   // transmitting callsign
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Symbol    string  `json:"symbol"`     // 2-char APRS symbol: table+code e.g. "/!"
	Comment   string  `json:"comment"`
	Active    bool    `json:"active"`
	CreatedAt int64   `json:"created_at"`
	LastTx    int64   `json:"last_tx,omitempty"`
}

var (
	memberObjMu      sync.RWMutex
	memberObjects    = make(map[string]*MemberObject) // key: memberID+"|"+name
)

func memberObjKey(memberID, name string) string {
	return memberID + "|" + strings.ToUpper(name)
}

// ── Transmit loop ──────────────────────────────────────────────────────────

func runMemberObjectBeacon() {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		beaconMemberObjects()
	}
}

func beaconMemberObjects() {
	memberObjMu.RLock()
	objs := make([]*MemberObject, 0, len(memberObjects))
	for _, o := range memberObjects {
		if o.Active {
			objs = append(objs, o)
		}
	}
	memberObjMu.RUnlock()

	now := time.Now().Unix()
	for _, o := range objs {
		pkt := buildObjectPacket(o)
		// Transmit via the upstream APRS-IS connection
		sendToAPRSIS(pkt)
		memberObjMu.Lock()
		if obj := memberObjects[memberObjKey(o.MemberID, o.Name)]; obj != nil {
			obj.LastTx = now
		}
		memberObjMu.Unlock()
	}
}

// buildObjectPacket formats an APRS object packet.
func buildObjectPacket(o *MemberObject) string {
	// Encode lat: DDMM.hhN
	latDeg := int(math.Abs(o.Lat))
	latMin := (math.Abs(o.Lat) - float64(latDeg)) * 60
	latHemi := 'N'
	if o.Lat < 0 {
		latHemi = 'S'
	}
	latStr := fmt.Sprintf("%02d%05.2f%c", latDeg, latMin, latHemi)

	// Encode lon: DDDMM.hhW
	lonDeg := int(math.Abs(o.Lon))
	lonMin := (math.Abs(o.Lon) - float64(lonDeg)) * 60
	lonHemi := 'E'
	if o.Lon < 0 {
		lonHemi = 'W'
	}
	lonStr := fmt.Sprintf("%03d%05.2f%c", lonDeg, lonMin, lonHemi)

	// Symbol table + symbol code (defaults to flag if invalid)
	sym := "/#"
	if len(o.Symbol) == 2 {
		sym = o.Symbol
	}

	// Name padded to exactly 9 chars
	name := o.Name
	if len(name) > 9 {
		name = name[:9]
	}
	for len(name) < 9 {
		name += " "
	}

	// Timestamp: current DDHHMM z (UTC)
	ts := time.Now().UTC().Format("021504") + "z"

	comment := o.Comment
	if len(comment) > 43 {
		comment = comment[:43]
	}

	// Object packet
	payload := fmt.Sprintf(";%s*%s%s%s%s%s%s",
		name, ts, latStr, sym[0:1], lonStr, sym[1:2], comment)

	return fmt.Sprintf("%s>APNUK,TCPIP*:%s", o.Callsign, payload)
}

// sendToAPRSIS queues a packet for transmission to the APRS-IS upstream.
func sendToAPRSIS(packet string) {
	select {
	case upstreamTx <- packet:
	default:
		// Channel full — drop silently (same behaviour as other upstream senders)
	}
}

// ── HTTP handlers ──────────────────────────────────────────────────────────

func handleMemberObjects(w http.ResponseWriter, r *http.Request) {
	m := getMemberFromRequest(r)
	if m == nil {
		w.WriteHeader(401)
		return
	}

	switch r.Method {
	case http.MethodGet:
		memberObjMu.RLock()
		var list []*MemberObject
		for _, o := range memberObjects {
			if o.MemberID == m.ID {
				cp := *o
				list = append(list, &cp)
			}
		}
		memberObjMu.RUnlock()
		if list == nil {
			list = []*MemberObject{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "objects": list})

	case http.MethodPost:
		var req struct {
			Name    string  `json:"name"`
			Lat     float64 `json:"lat"`
			Lon     float64 `json:"lon"`
			Symbol  string  `json:"symbol"`
			Comment string  `json:"comment"`
			Active  *bool   `json:"active"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "name required"})
			return
		}
		name := strings.ToUpper(strings.TrimSpace(req.Name))
		if len(name) > 9 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "name max 9 chars"})
			return
		}
		if req.Lat < -90 || req.Lat > 90 || req.Lon < -180 || req.Lon > 180 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid coordinates"})
			return
		}
		active := true
		if req.Active != nil {
			active = *req.Active
		}

		key := memberObjKey(m.ID, name)
		memberObjMu.Lock()
		// Cap at 10 objects per member
		ownCount := 0
		for _, o := range memberObjects {
			if o.MemberID == m.ID {
				ownCount++
			}
		}
		existing := memberObjects[key]
		if existing == nil && ownCount >= 10 {
			memberObjMu.Unlock()
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "maximum 10 objects per member"})
			return
		}
		if existing == nil {
			existing = &MemberObject{
				MemberID:  m.ID,
				Callsign:  m.Callsign,
				Name:      name,
				CreatedAt: time.Now().Unix(),
			}
			memberObjects[key] = existing
		}
		existing.Lat = req.Lat
		existing.Lon = req.Lon
		existing.Symbol = req.Symbol
		existing.Comment = req.Comment
		existing.Active = active
		memberObjMu.Unlock()

		// Immediately transmit if active
		if active {
			go func(o MemberObject) {
				pkt := buildObjectPacket(&o)
				sendToAPRSIS(pkt)
			}(*existing)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	case http.MethodDelete:
		name := strings.ToUpper(strings.TrimSpace(
			strings.TrimPrefix(r.URL.Path, "/api/member/objects/")))
		if name == "" {
			w.WriteHeader(400)
			return
		}
		key := memberObjKey(m.ID, name)
		memberObjMu.Lock()
		obj := memberObjects[key]
		if obj != nil {
			obj.Active = false
			// Send a "killed" object packet (use _ instead of *)
			killed := *obj
			delete(memberObjects, key)
			memberObjMu.Unlock()
			go func(o MemberObject) {
				// Kill packet: replace * with _
				pkt := buildObjectPacket(&o)
				pkt = strings.Replace(pkt, ";"+strings.TrimSpace(o.Name)+"*", ";"+strings.TrimSpace(o.Name)+"_", 1)
				sendToAPRSIS(pkt)
			}(killed)
		} else {
			memberObjMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	default:
		w.WriteHeader(405)
	}
}
