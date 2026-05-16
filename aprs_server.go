package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"fmt"
	"log"
	"math"
	"net"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// AppVersion is the running server version, compared against GitHub releases.
const AppVersion = "1.4.0"

type AppConfig struct {
	ServerName     string  `json:"server_name"`
	SoftwareVers   string  `json:"software_vers"`
	Callsign       string  `json:"callsign"`
	Passcode       string  `json:"passcode"`
	UpstreamAddr   string  `json:"upstream_addr"`
	ServerFilter   string  `json:"server_filter"`
	DropPiStar     bool    `json:"drop_pistar"`
	DropDStar      bool    `json:"drop_dstar"`
	DropAPDesk     bool    `json:"drop_apdesk"`
	EnableGeofence bool    `json:"enable_geofence"`
	CenterLat      float64 `json:"center_lat"`
	CenterLon      float64 `json:"center_lon"`
	RadiusKm       float64 `json:"radius_km"`
	sync.RWMutex   `json:"-"`
}

// adminCreds holds the HTTP Basic Auth credentials for the admin panel.
// These are runtime-changeable without restarting.
var adminCreds struct {
	sync.RWMutex
	Username string
	Password string
}

var (
	config = AppConfig{
		ServerName:     "T2CUSTOM",
		SoftwareVers:   "AdvancedGoAPRS 12.0",
		Callsign:       "NOCALL",
		Passcode:       "-1",
		UpstreamAddr:   "rotate.aprs2.net:14580",
		ServerFilter:   "auto",
		DropPiStar:     true,
		DropDStar:      true,
		DropAPDesk:     true,
		EnableGeofence: false,
		CenterLat:      51.5,
		CenterLon:      -0.1,
		RadiusKm:       100.0,
	}

	upstreamConnected int32

	metrics = struct {
		StartTime   time.Time
		PktsRx      uint64
		PktsTx      uint64
		PktsDropped uint64
		BytesRx     uint64
		BytesTx     uint64
		sync.RWMutex
	}{StartTime: time.Now()}

	clients       = make(map[*wsClient]bool)
	clientsMu     sync.Mutex
	broadcast     = make(chan string, 5000)
	upstreamOut   = make(chan string, 5000)
	reconnectChan = make(chan struct{}, 1)

	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	dupes   = make(map[string]time.Time)
	dupesMu sync.Mutex

	history    []HistoryPacket
	historyMu  sync.RWMutex
	maxHistory = 10000

	// rawRing stores the last hour of all raw packets for replay to new clients
	rawRing   []RawEntry
	rawRingMu sync.RWMutex

	// msgStore stores the last hour of APRS messages for replay
	msgStore   []MsgEntry
	msgStoreMu sync.RWMutex

	// objectStore holds active APRS objects and items
	objectStore   map[string]*ObjectEntry
	objectStoreMu sync.RWMutex

	// Webhooks and API keys
	webhooks   []WebhookConfig
	webhooksMu sync.RWMutex
	apiKeys    map[string]APIKey
	apiKeysMu  sync.RWMutex

	// Metrics counters
	totalPackets    int64
	dupePackets     int64
	upstreamBytes   int64
	packetsLastMin  int64
	metricsStart    = time.Now()
)

var (
	posRegex = regexp.MustCompile(`[!\/=@\*](\d{2})(\d{2}\.\d{2})([NS])(.)(\d{3})(\d{2}\.\d{2})([EW])(.)`)
	// Object format: ;NAME_____*DDHHMMzDDMM.MMN/DDDMM.MMW symbol comment
	objRegex = regexp.MustCompile(`;([^\*]{9})[\*_](\d{6}[z\/])(\d{2})(\d{2}\.\d{2})([NS])(.)(\d{3})(\d{2}\.\d{2})([EW])(.)`)
	// Item format: )NAME!DDMM.MMN/DDDMM.MMW symbol comment
	itemRegex = regexp.MustCompile(`\)([^!]{3,9})[!_](\d{2})(\d{2}\.\d{2})([NS])(.)(\d{3})(\d{2}\.\d{2})([EW])(.)`)
	phgRegex = regexp.MustCompile(`PHG(\d)(\d)(\d)(\d)`)
)


type HistoryPacket struct {
	Timestamp int64   `json:"ts"`
	Callsign  string  `json:"call"`
	Path      string  `json:"path"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Symbol    string  `json:"sym"`
	PHG       string  `json:"phg,omitempty"`
	Raw       string  `json:"raw"`
}

type WebhookConfig struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Callsign  string `json:"callsign"`
	Events    []string `json:"events"`
	Secret    string `json:"secret,omitempty"`
	Enabled   bool   `json:"enabled"`
	LastFired int64  `json:"last_fired,omitempty"`
	LastStatus int   `json:"last_status,omitempty"`
}

type APIKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	Created   int64  `json:"created"`
	LastUsed  int64  `json:"last_used,omitempty"`
	ReadOnly  bool   `json:"read_only"`
}

type WebhookEvent struct {
	Event     string      `json:"event"`
	Timestamp int64       `json:"timestamp"`
	Callsign  string      `json:"callsign,omitempty"`
	Data      interface{} `json:"data"`
}

type ObjectEntry struct {
	Timestamp int64   `json:"ts"`
	Name      string  `json:"name"`
	Owner     string  `json:"owner"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Symbol    string  `json:"sym"`
	Comment   string  `json:"comment"`
	Killed    bool    `json:"killed"`
	Raw       string  `json:"raw"`
}

type RawEntry struct {
	Timestamp int64  `json:"ts"`
	Packet    string `json:"packet"`
}

type MsgEntry struct {
	Timestamp int64  `json:"ts"`
	From      string `json:"from"`
	To        string `json:"to"`
	Text      string `json:"text"`
	MsgID     string `json:"id,omitempty"`
	Packet    string `json:"packet"`
}

type wsClient struct {
	conn          *websocket.Conn
	send          chan []byte
	authenticated bool
	callsign      string
	software      string
	remoteAddr    string
	connectedAt   int64
	lastTx        time.Time
}

type wsMessage struct {
	Type     string      `json:"type"`
	Callsign string      `json:"callsign,omitempty"`
	Passcode string      `json:"passcode,omitempty"`
	Software string      `json:"software,omitempty"`
	Status   string      `json:"status,omitempty"`
	Packet   string      `json:"packet,omitempty"`
	Data     interface{} `json:"data,omitempty"`
	Minutes  int         `json:"minutes,omitempty"`
}


// handleTocalls serves the embedded APRS device identification database.
// Source: https://github.com/aprsorg/aprs-deviceid (CC BY-SA 2.0)
// Updated occasionally by re-embedding tocalls.json at build time.
func handleTocalls(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=86400") // 24h cache
	w.Write(embeddedTocallsJSON)
}

func main() {
	loadOrInitCreds()
	loadSavedConfig()
	objectStore = make(map[string]*ObjectEntry)
	apiKeys    = make(map[string]APIKey)
	loadWebhooksAndKeys()
	loadMemberStore()
	loadBanList()
	loadMOTD()
	go cleanExpiredSessions()

	go cleanDuplicateCache()
	go maintainUpstream()
	go handleBroadcasts()
	go listenUDP()
	go listenTCPClients()
	go keepaliveLoop()

	http.HandleFunc("/symbols/", http.StripPrefix("/symbols/", http.FileServer(http.Dir("symbols"))).ServeHTTP)
	http.HandleFunc("/demo", serveDemo)
	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/setup", handleSetup)
	http.HandleFunc("/ws", handleWS)
	http.HandleFunc("/api/config", basicAuth(handleConfig))
	http.HandleFunc("/api/config/demo", handleConfigDemo)
	http.HandleFunc("/api/history", apiKeyAuth(handleHistory))
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/password", basicAuth(handlePassword))
	http.HandleFunc("/api/whoami", basicAuth(handleWhoami))
	http.HandleFunc("/api/objects", handleObjects)
	// Member system
	http.HandleFunc("/api/member/register", handleMemberRegister)
	http.HandleFunc("/api/member/login",    handleMemberLogin)
	http.HandleFunc("/api/member/logout",   handleMemberLogout)
	http.HandleFunc("/api/member/profile",  handleMemberProfile)
	http.HandleFunc("/api/member/messages", handleMemberMessages)
	http.HandleFunc("/api/member/password", handleMemberPassword)
	// Public MOTD
	http.HandleFunc("/api/motd", handlePublicMOTD)
	// Admin features
	http.HandleFunc("/api/admin/members", basicAuth(handleAdminMembers))
	http.HandleFunc("/api/admin/members/", basicAuth(handleAdminMemberRouter))
	http.HandleFunc("/api/admin/bans", basicAuth(handleAdminBans))
	http.HandleFunc("/api/admin/motd", basicAuth(handleAdminMOTD))
	http.HandleFunc("/api/admin/audit", basicAuth(handleAdminAudit))
	http.HandleFunc("/api/admin/backup", basicAuth(handleAdminBackup))
	http.HandleFunc("/api/admin/restore", basicAuth(handleAdminRestore))
	http.HandleFunc("/metrics", basicAuth(handleMetrics))
	http.HandleFunc("/api/webhooks", basicAuth(handleWebhooks))
	http.HandleFunc("/api/webhooks/", basicAuth(handleWebhookDelete))
	http.HandleFunc("/api/keys", basicAuth(handleAPIKeys))
	http.HandleFunc("/api/keys/", basicAuth(handleAPIKeyDelete))
	http.HandleFunc("/api/export/geojson", handleGeoJSON)
	http.HandleFunc("/api/export/kml", handleKML)
	http.HandleFunc("/api/tle", handleTLE)
	http.HandleFunc("/api/ariss", handleARISS)
	http.HandleFunc("/api/version", handleVersion)
	http.HandleFunc("/api/tocalls", handleTocalls)
	http.HandleFunc("/api/messages", handleMessages)
	http.HandleFunc("/api/iss", handleISSPosition)
	http.HandleFunc("/api/update", basicAuth(handleUpdate))

	log.Printf("Advanced APRS Gateway active on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// serveIndex redirects to /setup if no credentials have been configured yet.
func serveIndex(w http.ResponseWriter, r *http.Request) {
	if !credsConfigured() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	http.ServeFile(w, r, "index.html")
}

// serveDemo serves the dashboard in read-only demo mode.
// Sets a cookie so the frontend knows to hide all write controls.
func serveDemo(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "aprs_demo",
		Value:    "1",
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
	http.ServeFile(w, r, "index.html")
}

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		adminCreds.RLock()
		wantUser, wantPass := adminCreds.Username, adminCreds.Password
		adminCreds.RUnlock()
		if !ok || user != wantUser || pass != wantPass {
			// Do NOT send WWW-Authenticate — that triggers the browser native dialog.
			// Our frontend handles auth with a custom login gate.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next(w, r)
	}
}

// handlePassword is defined below in the first-run setup section.

func verifyPasscode(callsign, passcode string) bool {
	if passcode == "-1" {
		return false
	}
	baseCall := strings.Split(strings.ToUpper(callsign), "-")[0]
	hash := 0x73e2
	for i := 0; i < len(baseCall); i += 2 {
		hash ^= int(baseCall[i]) << 8
		if i+1 < len(baseCall) {
			hash ^= int(baseCall[i+1])
		}
	}
	return fmt.Sprintf("%d", hash&0x7fff) == passcode
}


func injectQConstruct(packet, qType string) string {
	config.RLock()
	srv := config.ServerName
	config.RUnlock()
	gtIdx := strings.Index(packet, ">")
	if gtIdx == -1 {
		return packet
	}
	colIdx := strings.Index(packet[gtIdx:], ":")
	if colIdx == -1 {
		return packet
	}
	colIdx += gtIdx
	header := packet[:colIdx]
	if strings.Contains(header, ",qA") {
		return packet
	}
	return fmt.Sprintf("%s,%s,%s%s", header, qType, srv, packet[colIdx:])
}

func isDuplicate(packet string) bool {
	hashData := packet
	gtIdx := strings.Index(packet, ">")
	colIdx := strings.Index(packet, ":")
	if gtIdx != -1 && colIdx != -1 && gtIdx < colIdx {
		header := packet[:colIdx]
		if qIdx := strings.Index(header, ",qA"); qIdx != -1 {
			hashData = header[:qIdx] + packet[colIdx:]
		}
	}
	dupesMu.Lock()
	defer dupesMu.Unlock()
	if _, exists := dupes[hashData]; exists {
		return true
	}
	dupes[hashData] = time.Now()
	return false
}

func cleanDuplicateCache() {
	for {
		time.Sleep(1 * time.Minute)
		now := time.Now()
		dupesMu.Lock()
		for k, v := range dupes {
			if now.Sub(v) > 5*time.Minute {
				delete(dupes, k)
			}
		}
		dupesMu.Unlock()
	}
}

func maintainUpstream() {
	for {
		connectUpstream()
		atomic.StoreInt32(&upstreamConnected, 0)
		time.Sleep(5 * time.Second)
	}
}

func connectUpstream() {
	config.RLock()
	addr, call, pass, filter, vers := config.UpstreamAddr, config.Callsign, config.Passcode, config.ServerFilter, config.SoftwareVers
	cLat, cLon, rad := config.CenterLat, config.CenterLon, config.RadiusKm
	config.RUnlock()

	filter = strings.TrimSpace(filter)
	if strings.ToLower(filter) == "auto" || filter == "" {
		filter = fmt.Sprintf("r/%.4f/%.4f/%.0f", cLat, cLon, rad)
	}

	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		log.Printf("Upstream connect failed (%s): %v", addr, err)
		return
	}
	defer conn.Close()

	loginLine := fmt.Sprintf("user %s pass %s vers %s filter %s\r\n", call, pass, vers, filter)
	log.Printf("APRS-IS login: user=%s filter=%s", call, filter)
	if _, err := fmt.Fprint(conn, loginLine); err != nil {
		return
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	scanner := bufio.NewScanner(conn)
	verified := false
	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("APRS-IS banner: %s", line)
		if strings.Contains(line, "logresp") {
			if strings.Contains(line, "verified") && !strings.Contains(line, "unverified") {
				verified = true
			}
			break
		}
	}
	conn.SetReadDeadline(time.Time{})

	if !verified {
		log.Printf("APRS-IS login not verified for %s — check callsign/passcode", call)
	}
	atomic.StoreInt32(&upstreamConnected, 1)
	log.Printf("Connected to APRS-IS upstream: %s (verified=%v)", addr, verified)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasPrefix(text, "#") {
				log.Printf("APRS-IS: %s", text)
				continue
			}
			metrics.Lock()
			metrics.PktsRx++
			metrics.BytesRx += uint64(len(text))
			metrics.Unlock()
			if isAllowed(text) && !isDuplicate(text) {
				broadcast <- text
			} else {
				metrics.Lock()
				metrics.PktsDropped++
				metrics.Unlock()
			}

		}
	}()

	for {
		select {
		case <-done:
			atomic.StoreInt32(&upstreamConnected, 0)
			return
		case <-reconnectChan:
			return
		case pkt := <-upstreamOut:
			outBytes := []byte(pkt + "\r\n")
			conn.Write(outBytes)
			metrics.Lock()
			metrics.PktsTx++
			metrics.BytesTx += uint64(len(outBytes))
			metrics.Unlock()
		}
	}
}


func isAllowed(packet string) bool {
	gtIdx := strings.Index(packet, ">")
	colIdx := strings.Index(packet, ":")
	if gtIdx == -1 || colIdx == -1 || gtIdx > colIdx {
		return false
	}
	upper := strings.ToUpper(packet)
	config.RLock()
	dPi, dD, dDesk, geo, cLat, cLon, rad := config.DropPiStar, config.DropDStar, config.DropAPDesk, config.EnableGeofence, config.CenterLat, config.CenterLon, config.RadiusKm
	config.RUnlock()
	if dPi && (strings.Contains(upper, "PISTAR") || strings.Contains(upper, "MMDVM") || strings.Contains(upper, "APDPRS")) {
		return false
	}
	if dD && (strings.Contains(upper, "D-STAR") || strings.Contains(upper, "APDSTR")) {
		return false
	}
	if dDesk && strings.Contains(upper, "APDESK") {
		return false
	}
	if geo {
		if parsed, ok := parsePacket(packet); ok {
			dLat := (parsed.Lat - cLat) * 111.0
			dLon := (parsed.Lon - cLon) * 111.0 * math.Cos(cLat*math.Pi/180)
			if (dLat*dLat + dLon*dLon) > (rad * rad) {
				return false
			}
		}
	}
	return true
}

func parsePacket(packet string) (HistoryPacket, bool) {
	var hp HistoryPacket
	gtIdx := strings.Index(packet, ">")
	colIdx := strings.Index(packet, ":")
	if gtIdx == -1 || colIdx == -1 || gtIdx > colIdx {
		return hp, false
	}
	hp.Callsign = packet[:gtIdx]
	hp.Path = packet[gtIdx+1 : colIdx]
	hp.Raw = packet
	hp.Timestamp = time.Now().Unix()
	payload := packet[colIdx+1:]
	match := posRegex.FindStringSubmatch(payload)
	if match == nil {
		return hp, false
	}
	lDeg, _ := strconv.ParseFloat(match[1], 64)
	lMin, _ := strconv.ParseFloat(match[2], 64)
	hp.Lat = lDeg + lMin/60
	if match[3] == "S" {
		hp.Lat = -hp.Lat
	}
	lnDeg, _ := strconv.ParseFloat(match[5], 64)
	lnMin, _ := strconv.ParseFloat(match[6], 64)
	hp.Lon = lnDeg + lnMin/60
	if match[7] == "W" {
		hp.Lon = -hp.Lon
	}
	hp.Symbol = match[4] + match[8]
	if pMatch := phgRegex.FindStringSubmatch(payload); pMatch != nil {
		hp.PHG = pMatch[0]
	}
	return hp, true
}

// parseAPRSMessage extracts an APRS message packet into a MsgEntry.
// Returns nil if the packet is not a message.
func parseAPRSMessage(packet string) *MsgEntry {
	gtIdx := strings.Index(packet, ">")
	colIdx := strings.Index(packet, ":")
	if gtIdx == -1 || colIdx == -1 || gtIdx > colIdx {
		return nil
	}
	from := packet[:gtIdx]
	payload := packet[colIdx+1:]
	// APRS message format: :CALLSIGN :text{id}
	if len(payload) < 11 || payload[0] != ':' {
		return nil
	}
	to := strings.TrimSpace(payload[1:10])
	body := payload[10:]
	if len(body) < 1 || body[0] != ':' {
		return nil
	}
	body = body[1:]
	msgID := ""
	if idx := strings.LastIndex(body, "{"); idx != -1 {
		msgID = body[idx+1:]
		body = body[:idx]
		msgID = strings.TrimRight(msgID, "}")
	}
	// Skip acks
	if strings.HasPrefix(strings.ToLower(body), "ack") {
		return nil
	}
	return &MsgEntry{
		Timestamp: time.Now().Unix(),
		From:      from,
		To:        to,
		Text:      strings.TrimSpace(body),
		MsgID:     msgID,
		Packet:    packet,
	}
}


// ─── Object / Item parsing ────────────────────────────────────────────────────

func parseObject(packet string) (*ObjectEntry, bool) {
	gtIdx := strings.Index(packet, ">")
	colIdx := strings.Index(packet, ":")
	if gtIdx == -1 || colIdx == -1 || gtIdx > colIdx {
		return nil, false
	}
	owner := packet[:gtIdx]
	payload := packet[colIdx+1:]

	// Try object format
	if m := objRegex.FindStringSubmatch(payload); m != nil {
		name := strings.TrimRight(m[1], " ")
		killed := strings.Contains(packet, "_") && !strings.Contains(packet, "*")
		lDeg, _ := strconv.ParseFloat(m[3], 64)
		lMin, _ := strconv.ParseFloat(m[4], 64)
		lat := lDeg + lMin/60
		if m[5] == "S" { lat = -lat }
		lnDeg, _ := strconv.ParseFloat(m[7], 64)
		lnMin, _ := strconv.ParseFloat(m[8], 64)
		lon := lnDeg + lnMin/60
		if m[9] == "W" { lon = -lon }
		sym := m[6] + m[10]
		comment := ""
		if len(payload) > len(m[0]) {
			comment = strings.TrimSpace(payload[strings.Index(payload, m[0])+len(m[0]):])
		}
		return &ObjectEntry{
			Timestamp: time.Now().Unix(),
			Name: name, Owner: owner,
			Lat: lat, Lon: lon,
			Symbol: sym, Comment: comment,
			Killed: killed, Raw: packet,
		}, true
	}

	// Try item format
	if m := itemRegex.FindStringSubmatch(payload); m != nil {
		name := strings.TrimRight(m[1], " ")
		killed := payload[len(name)+1] == '_'
		lDeg, _ := strconv.ParseFloat(m[2], 64)
		lMin, _ := strconv.ParseFloat(m[3], 64)
		lat := lDeg + lMin/60
		if m[4] == "S" { lat = -lat }
		lnDeg, _ := strconv.ParseFloat(m[6], 64)
		lnMin, _ := strconv.ParseFloat(m[7], 64)
		lon := lnDeg + lnMin/60
		if m[8] == "W" { lon = -lon }
		sym := m[5] + m[9]
		return &ObjectEntry{
			Timestamp: time.Now().Unix(),
			Name: name, Owner: owner,
			Lat: lat, Lon: lon,
			Symbol: sym, Killed: killed, Raw: packet,
		}, true
	}
	return nil, false
}

// handleObjects returns all active (non-killed) objects and items.
func handleObjects(w http.ResponseWriter, r *http.Request) {
	objectStoreMu.RLock()
	defer objectStoreMu.RUnlock()
	result := make([]*ObjectEntry, 0, len(objectStore))
	for _, o := range objectStore {
		if !o.Killed {
			result = append(result, o)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func handleBroadcasts() {
	for packet := range broadcast {
		parsed, hasCoords := parsePacket(packet)
		if hasCoords {
			historyMu.Lock()
			history = append(history, parsed)
			if len(history) > maxHistory {
				history = history[1:]
			}
			historyMu.Unlock()
		}

		// Increment metrics
		atomic.AddInt64(&totalPackets, 1)
		atomic.AddInt64(&upstreamBytes, int64(len(packet)))

		// Fire webhooks for position packets
		if hasCoords {
			go fireWebhooks("position", parsed.Callsign, parsed)
		}

		// Store in raw ring buffer (trim entries older than 1 hour)
		now := time.Now().Unix()
		rawRingMu.Lock()
		rawRing = append(rawRing, RawEntry{Timestamp: now, Packet: packet})
		for len(rawRing) > 0 && now-rawRing[0].Timestamp > 3600 {
			rawRing = rawRing[1:]
		}
		rawRingMu.Unlock()

		// Parse and store APRS objects/items
		if obj, ok := parseObject(packet); ok {
			objectStoreMu.Lock()
			if obj.Killed {
				delete(objectStore, obj.Name)
			} else {
				objectStore[obj.Name] = obj
			}
			objectStoreMu.Unlock()
		}

		// Parse and store APRS messages
		// Check ban list - drop packets from banned callsigns
		if gtIdx := strings.Index(packet, ">"); gtIdx > 0 {
			src := packet[:gtIdx]
			if reason := isCallsignBanned(src); reason != "" {
				continue
			}
		}
		if msg := parseAPRSMessage(packet); msg != nil {
			go fireWebhooks("message", msg.From, msg)
			go storeMessageForMember(msg.To, msg.From, msg.Text, packet, time.Now().Unix())
			msgStoreMu.Lock()
			msgStore = append(msgStore, *msg)
			for len(msgStore) > 0 && now-msgStore[0].Timestamp > 3600 {
				msgStore = msgStore[1:]
			}
			msgStoreMu.Unlock()
		}
		msg := wsMessage{Type: "rx", Packet: packet}
		if hasCoords {
			msg.Data = parsed
		}
		// Check if this is an object/item and tag it
		payload := ""
		if ci := strings.Index(packet, ":"); ci >= 0 { payload = packet[ci+1:] }
		if len(payload) > 0 && (payload[0] == ';' || payload[0] == ')') {
			msg.Type = "obj"
		}
		data, _ := json.Marshal(msg)
		clientsMu.Lock()
		for c := range clients {
			select {
			case c.send <- data:
			default:
			}
		}
		clientsMu.Unlock()
		// Also push to TCP APRS-IS clients
		broadcastToTCPClients(packet)
	}
}

func (c *wsClient) writePump() {
	defer c.conn.Close()
	for data := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}
}


func handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{
		conn:        conn,
		send:        make(chan []byte, 1024),
		remoteAddr:  r.RemoteAddr,
		connectedAt: time.Now().Unix(),
		lastTx:      time.Now(),
	}
	clientsMu.Lock()
	clients[client] = true
	clientsMu.Unlock()
	go client.writePump()

	// Send replay of last hour to new client
	go func() {
		// Small delay so client JS is ready
		time.Sleep(300 * time.Millisecond)

		// Raw packet replay
		rawRingMu.RLock()
		replay := make([]RawEntry, len(rawRing))
		copy(replay, rawRing)
		rawRingMu.RUnlock()

		for _, e := range replay {
			msg := wsMessage{Type: "rx", Packet: e.Packet}
			if parsed, ok := parsePacket(e.Packet); ok {
				msg.Data = parsed
			}
			data, _ := json.Marshal(msg)
			select {
			case client.send <- data:
			default:
			}
		}

		// Message replay
		msgStoreMu.RLock()
		msgs := make([]MsgEntry, len(msgStore))
		copy(msgs, msgStore)
		msgStoreMu.RUnlock()

		for _, m := range msgs {
			data, _ := json.Marshal(wsMessage{Type: "msg_history", Packet: m.Packet,
				Callsign: m.From, Data: m})
			select {
			case client.send <- data:
			default:
			}
		}

		// Signal end of replay
		data, _ := json.Marshal(wsMessage{Type: "replay_done"})
		select {
		case client.send <- data:
		default:
		}
	}()

	defer func() {
		clientsMu.Lock()
		delete(clients, client)
		clientsMu.Unlock()
		close(client.send)
	}()
	for {
		_, msgData, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var in wsMessage
		if err := json.Unmarshal(msgData, &in); err != nil {
			continue
		}
		switch in.Type {
		case "replay_request":
			// Client wants historical packets - send up to in.Minutes minutes back
			minutes := in.Minutes
			if minutes <= 0 { minutes = 60 }
			if minutes > 120 { minutes = 120 }
			go sendReplayToClient(client, minutes)
		case "auth":
			if verifyPasscode(in.Callsign, in.Passcode) {
				client.authenticated = true
				client.callsign = strings.ToUpper(in.Callsign)
				client.software = in.Software
				ack, _ := json.Marshal(wsMessage{Type: "auth_ack", Status: "success", Callsign: client.callsign})
				client.send <- ack
			} else {
				ack, _ := json.Marshal(wsMessage{Type: "auth_ack", Status: "fail"})
				client.send <- ack
			}
		case "tx":
			if !client.authenticated {
				continue
			}
			if !strings.HasPrefix(strings.ToUpper(in.Packet), client.callsign+">") {
				continue
			}
			if time.Since(client.lastTx) < time.Second {
				continue
			}
			client.lastTx = time.Now()
			metrics.Lock()
			metrics.PktsRx++
			metrics.BytesRx += uint64(len(in.Packet))
			metrics.Unlock()
			routed := injectQConstruct(in.Packet, "qAC")
			if isAllowed(routed) && !isDuplicate(routed) {
				broadcast <- routed
				upstreamOut <- routed
			} else {
				metrics.Lock()
				metrics.PktsDropped++
				metrics.Unlock()
			}
		}
	}
}

// sendReplayToClient streams the last N minutes of packets to a single client
// in response to a replay_request message. Non-blocking on the send channel.
func sendReplayToClient(client *wsClient, minutes int) {
	cutoff := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()

	// Raw packet replay - filter by timestamp
	rawRingMu.RLock()
	replay := make([]RawEntry, 0, len(rawRing))
	for _, e := range rawRing {
		if e.Timestamp >= cutoff {
			replay = append(replay, e)
		}
	}
	rawRingMu.RUnlock()

	// Tell client how many to expect
	startData, _ := json.Marshal(wsMessage{
		Type: "replay_start",
		Data: map[string]int{"total": len(replay), "minutes": minutes},
	})
	select {
	case client.send <- startData:
	default:
	}

	for _, e := range replay {
		msg := wsMessage{Type: "rx", Packet: e.Packet}
		if parsed, ok := parsePacket(e.Packet); ok {
			msg.Data = parsed
		}
		data, _ := json.Marshal(msg)
		select {
		case client.send <- data:
		case <-time.After(2 * time.Second):
			// Client too slow - abort this replay
			return
		}
	}

	// Message replay (also filtered)
	msgStoreMu.RLock()
	msgs := make([]MsgEntry, 0, len(msgStore))
	for _, m := range msgStore {
		if m.Timestamp >= cutoff {
			msgs = append(msgs, m)
		}
	}
	msgStoreMu.RUnlock()

	for _, m := range msgs {
		data, _ := json.Marshal(wsMessage{Type: "msg_history", Packet: m.Packet,
			Callsign: m.From, Data: m})
		select {
		case client.send <- data:
		default:
		}
	}

	// Signal end of replay
	doneData, _ := json.Marshal(wsMessage{Type: "replay_done",
		Data: map[string]int{"packets": len(replay)}})
	select {
	case client.send <- doneData:
	default:
	}
}


func listenUDP() {
	addr, err := net.ResolveUDPAddr("udp", ":14580")
	if err != nil {
		log.Printf("UDP resolve failed: %v", err)
		return
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Printf("UDP listen failed (port 14580): %v", err)
		return
	}
	defer conn.Close()
	buf := make([]byte, 1024)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		p := strings.TrimSpace(string(buf[:n]))
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		metrics.Lock()
		metrics.PktsRx++
		metrics.BytesRx += uint64(n)
		metrics.Unlock()
		routed := injectQConstruct(p, "qAU")
		if isAllowed(routed) && !isDuplicate(routed) {
			broadcast <- routed
			upstreamOut <- routed
		} else {
			metrics.Lock()
			metrics.PktsDropped++
			metrics.Unlock()
		}
	}
}


// ─── TCP APRS-IS Client Listener (port 14580) ────────────────────────────────
// Allows standard APRS clients (APRSDroid, Xastir, YAAC, Direwolf) to connect
// directly to this server using the standard APRS-IS protocol.

type tcpClient struct {
	conn        net.Conn
	callsign    string
	verified    bool
	filter      string
	remoteAddr  string
	connectedAt int64
}

var (
	tcpClients   = make(map[*tcpClient]bool)
	tcpClientsMu sync.Mutex
)

func listenTCPClients() {
	ln, err := net.Listen("tcp", ":14580")
	if err != nil {
		log.Printf("TCP client listener failed: %v", err)
		return
	}
	defer ln.Close()
	log.Printf("TCP APRS-IS client listener active on :14580")
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleTCPClient(conn)
	}
}

func handleTCPClient(conn net.Conn) {
	defer conn.Close()
	client := &tcpClient{
		conn:        conn,
		remoteAddr:  conn.RemoteAddr().String(),
		connectedAt: time.Now().Unix(),
	}

	config.RLock()
	srvName := config.ServerName
	srvVers := config.SoftwareVers
	config.RUnlock()

	// Send server banner
	fmt.Fprintf(conn, "# %s %s\r\n", srvName, srvVers)

	tcpClientsMu.Lock()
	tcpClients[client] = true
	tcpClientsMu.Unlock()
	defer func() {
		tcpClientsMu.Lock()
		delete(tcpClients, client)
		tcpClientsMu.Unlock()
		if client.callsign != "" {
			log.Printf("TCP client disconnected: %s (%s)", client.callsign, client.remoteAddr)
		}
	}()

	scanner := bufio.NewScanner(conn)
	loggedIn := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Login line: user CALLSIGN pass PASSCODE vers SOFTWARE filter FILTER
		if !loggedIn && strings.HasPrefix(strings.ToLower(line), "user ") {
			parts := strings.Fields(line)
			call, pass := "", ""
			for i, p := range parts {
				if strings.ToLower(p) == "user" && i+1 < len(parts) {
					call = strings.ToUpper(parts[i+1])
				}
				if strings.ToLower(p) == "pass" && i+1 < len(parts) {
					pass = parts[i+1]
				}
				if strings.ToLower(p) == "filter" && i+1 < len(parts) {
					client.filter = strings.Join(parts[i+1:], " ")
				}
			}
			client.callsign = call
			client.verified = verifyPasscode(call, pass)

			status := "unverified"
			if client.verified {
				status = "verified"
			}
			config.RLock()
			srv := config.ServerName
			config.RUnlock()
			fmt.Fprintf(conn, "# logresp %s %s, server %s\r\n", call, status, srv)
			log.Printf("TCP client login: %s %s from %s", call, status, client.remoteAddr)
			loggedIn = true

			metrics.Lock()
			metrics.PktsRx++
			metrics.Unlock()
			continue
		}

		if !loggedIn {
			continue
		}

		// Ignore comment lines from client
		if strings.HasPrefix(line, "#") {
			continue
		}

		// Incoming packet from client — must start with their callsign
		if !strings.HasPrefix(strings.ToUpper(line), client.callsign+">") {
			continue
		}
		if !client.verified {
			// Read-only clients cannot inject packets
			continue
		}

		metrics.Lock()
		metrics.PktsRx++
		metrics.BytesRx += uint64(len(line))
		metrics.Unlock()

		routed := injectQConstruct(line, "qAC")
		if isAllowed(routed) && !isDuplicate(routed) {
			broadcast <- routed
			upstreamOut <- routed
		} else {
			metrics.Lock()
			metrics.PktsDropped++
			metrics.Unlock()
		}
	}
}

// broadcastToTCPClients sends a packet to all connected TCP clients.
// Called from handleBroadcasts.
func broadcastToTCPClients(packet string) {
	line := packet + "\r\n"
	// Snapshot the client list so we don't hold the mutex during writes
	tcpClientsMu.Lock()
	snap := make([]*tcpClient, 0, len(tcpClients))
	for c := range tcpClients {
		if c.callsign != "" {
			snap = append(snap, c)
		}
	}
	tcpClientsMu.Unlock()
	for _, c := range snap {
		go func(cl *tcpClient, l string) {
			cl.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := fmt.Fprint(cl.conn, l)
			cl.conn.SetWriteDeadline(time.Time{})
			if err != nil {
				log.Printf("TCP write error %s: %v", cl.callsign, err)
			}
		}(c, line)
	}
}

func keepaliveLoop() {
	for {
		time.Sleep(20 * time.Second)
		config.RLock()
		msg := fmt.Sprintf("# %s %s", config.ServerName, config.SoftwareVers)
		config.RUnlock()

		// WebSocket clients
		data, _ := json.Marshal(wsMessage{Type: "sys", Packet: msg})
		clientsMu.Lock()
		for c := range clients {
			select {
			case c.send <- data:
			default:
			}
		}
		clientsMu.Unlock()

		// TCP APRS-IS clients — raw keepalive comment
		tcpClientsMu.Lock()
		snap := make([]*tcpClient, 0, len(tcpClients))
		for c := range tcpClients {
			snap = append(snap, c)
		}
		tcpClientsMu.Unlock()
		for _, c := range snap {
			go func(cl *tcpClient, m string) {
				cl.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				fmt.Fprintf(cl.conn, "%s\r\n", m)
				cl.conn.SetWriteDeadline(time.Time{})
			}(c, msg)
		}
	}
}


func handleHistory(w http.ResponseWriter, r *http.Request) {
	historyMu.RLock()
	defer historyMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	metrics.RLock()
	uptime := time.Since(metrics.StartTime).Round(time.Second).String()
	res := map[string]interface{}{
		"uptime":   uptime,
		"pkts_rx":  metrics.PktsRx,
		"pkts_tx":  metrics.PktsTx,
		"dropped":  metrics.PktsDropped,
		"bytes_rx": metrics.BytesRx,
		"bytes_tx": metrics.BytesTx,
	}
	metrics.RUnlock()
	config.RLock()
	res["upstream_addr"] = config.UpstreamAddr
	config.RUnlock()
	res["upstream_connected"] = atomic.LoadInt32(&upstreamConnected) == 1
	rawRingMu.RLock()
	res["ring_size"] = len(rawRing)
	rawRingMu.RUnlock()
	active := []map[string]interface{}{}
	clientsMu.Lock()
	for c := range clients {
		if c.authenticated {
			active = append(active, map[string]interface{}{
				"call": c.callsign, "soft": c.software, "type": "websocket",
				"addr": c.remoteAddr, "since": c.connectedAt,
			})
		}
	}
	clientsMu.Unlock()
	tcpClientsMu.Lock()
	for c := range tcpClients {
		if c.callsign != "" {
			active = append(active, map[string]interface{}{
				"call": c.callsign, "soft": "TCP/APRS-IS", "type": "tcp",
				"addr": c.remoteAddr, "since": c.connectedAt,
				"verified": c.verified,
			})
		}
	}
	res["tcp_clients"] = len(tcpClients)
	tcpClientsMu.Unlock()
	res["clients"] = active
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" {
		config.RLock()
		defer config.RUnlock()
		json.NewEncoder(w).Encode(config)
		return
	}
	var n AppConfig
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	config.Lock()
	config.ServerName = n.ServerName
	config.SoftwareVers = n.SoftwareVers
	config.Callsign = n.Callsign
	config.Passcode = n.Passcode
	config.UpstreamAddr = n.UpstreamAddr
	config.ServerFilter = strings.TrimSpace(n.ServerFilter)
	config.DropPiStar = n.DropPiStar
	config.DropDStar = n.DropDStar
	config.DropAPDesk = n.DropAPDesk
	config.EnableGeofence = n.EnableGeofence
	config.CenterLat = n.CenterLat
	config.CenterLon = n.CenterLon
	config.RadiusKm = n.RadiusKm
	config.Unlock()
	if err := saveConfig(); err != nil {
		log.Printf("Warning: config applied but failed to persist: %v", err)
	}
	select {
	case <-reconnectChan:
	default:
	}
	reconnectChan <- struct{}{}
	w.WriteHeader(http.StatusOK)
}




// handleVersion returns the running server version.
// The frontend polls GitHub releases to compare and show an update badge.
func handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"version":"` + AppVersion + `"}`))
}



// handleARISS proxies the aprs.fi API for ARISS packets to avoid browser CORS issues.
func handleARISS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	url := "https://api.aprs.fi/api/get?name=RS0ISS,RS0ISS-4,NA1SS&what=loc&apikey=104710.mxTOVTFLMl6la5&format=json&howmany=50"
	resp, err := fetchURL(url)
	if err != nil {
		http.Error(w, `{"error":"` + err.Error() + `"}`, http.StatusBadGateway)
		return
	}
	w.Write(resp)
}

// handleISSPosition proxies wheretheiss.at for live ISS position.
func handleISSPosition(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	resp, err := fetchURL("https://api.wheretheiss.at/v1/satellites/25544")
	if err != nil {
		http.Error(w, `{"error":"` + err.Error() + `"}`, http.StatusBadGateway)
		return
	}
	w.Write(resp)
}

// fetchURL makes a simple GET request and returns the body bytes.
func fetchURL(url string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 65536)
	buf2 := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf2)
		if n > 0 { buf = append(buf, buf2[:n]...) }
		if err != nil { break }
	}
	return buf, nil
}

// handleMessages returns the last hour of APRS messages.
func handleMessages(w http.ResponseWriter, r *http.Request) {
	msgStoreMu.RLock()
	defer msgStoreMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgStore)
}


// handleARISS proxies ARISS packet data from aprs.fi API server-side.
// This avoids CORS issues with direct browser requests to aprs.fi.


// handleConfigDemo returns server config with sensitive fields redacted.
// No authentication required — safe to expose publicly for demo mode.
func handleConfigDemo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	config.RLock()
	redacted := map[string]interface{}{
		"server_name":     config.ServerName,
		"software_vers":   config.SoftwareVers,
		"callsign":        config.Callsign,
		"passcode":        "••••••",
		"upstream_addr":   config.UpstreamAddr,
		"server_filter":   config.ServerFilter,
		"drop_pistar":     config.DropPiStar,
		"drop_dstar":      config.DropDStar,
		"drop_apdesk":     config.DropAPDesk,
		"enable_geofence": config.EnableGeofence,
		"center_lat":      config.CenterLat,
		"center_lon":      config.CenterLon,
		"radius_km":       config.RadiusKm,
	}
	config.RUnlock()
	json.NewEncoder(w).Encode(redacted)
}


// handleTLE proxies TLE data from Celestrak for ISS and ARISS satellites.
// Caches for 6 hours since TLEs change slowly.
var (
	tleCache    string
	tleCacheAt  time.Time
	tleCacheMu  sync.Mutex
)

func handleTLE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=3600")

	tleCacheMu.Lock()
	defer tleCacheMu.Unlock()

	// Return cache if fresh (< 6 hours)
	if tleCache != "" && time.Since(tleCacheAt) < 6*time.Hour {
		w.Write([]byte(tleCache))
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Primary: live.ariss.org - always has current ISS TLE in 3-line format
	if resp, err := client.Get("https://live.ariss.org/iss.txt"); err == nil {
		if body, err := io.ReadAll(resp.Body); err == nil && len(body) > 50 {
			resp.Body.Close()
			tleCache = string(body)
			tleCacheAt = time.Now()
			w.Write(body)
			return
		}
		resp.Body.Close()
	}

	// Secondary: wheretheiss.at JSON API - extract TLE fields
	if resp, err := client.Get("https://api.wheretheiss.at/v1/satellites/25544/tles?units=miles"); err == nil {
		var result map[string]interface{}
		if err2 := json.NewDecoder(resp.Body).Decode(&result); err2 == nil {
			resp.Body.Close()
			name, _ := result["name"].(string)
			l1, _   := result["line1"].(string)
			l2, _   := result["line2"].(string)
			if l1 != "" && l2 != "" {
				text := name + "\n" + l1 + "\n" + l2 + "\n"
				tleCache = text
				tleCacheAt = time.Now()
				w.Write([]byte(text))
				return
			}
		} else {
			resp.Body.Close()
		}
	}

	// Tertiary: Celestrak (may be slow/blocked from some servers)
	for _, url := range []string{
		"https://celestrak.org/SATCAT/tle.php?CATNR=25544&FORMAT=TLE",
		"https://celestrak.com/SATCAT/tle.php?CATNR=25544&FORMAT=TLE",
	} {
		if resp, err := client.Get(url); err == nil {
			if body, err := io.ReadAll(resp.Body); err == nil && len(body) > 50 {
				resp.Body.Close()
				tleCache = string(body)
				tleCacheAt = time.Now()
				w.Write(body)
				return
			}
			resp.Body.Close()
		}
	}

	http.Error(w, "Could not fetch TLE from any source", 502)
}

// handleWhoami returns the authenticated username — used by the frontend
// to verify credentials are correct before showing the admin panel.
func handleWhoami(w http.ResponseWriter, r *http.Request) {
	adminCreds.RLock()
	user := adminCreds.Username
	adminCreds.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"user":"` + user + `"}`))
}



// handleUpdate performs a live update: git pull, go build, systemctl restart.
// Streams log lines as newline-delimited JSON so the browser can show progress.
// Protected by basicAuth.
func handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, canFlush := w.(http.Flusher)

	send := func(msg, status string) {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]string{"msg": msg, "status": status}))
		if canFlush {
			flusher.Flush()
		}
		log.Printf("[update] %s", msg)
	}

	appDir := "/opt/aprs-gateway"
	gobin := "/usr/local/go/bin/go"

	send("Starting update...", "running")

	// Step 1: git pull
	send("Running git pull...", "running")
	out, err := runCmd(appDir, "git", "pull", "origin", "main")
	if err != nil {
		send("git pull failed: "+out, "error")
		return
	}
	send("git pull: "+strings.TrimSpace(out), "running")

	// Step 2: go build
	send("Building binary (this takes ~30s)...", "running")
	out, err = runCmd(appDir, gobin, "build", "-o", "aprs_server", "aprs_server.go")
	if err != nil {
		send("Build failed: "+out, "error")
		return
	}
	send("Build successful", "running")

	// Step 3: systemctl restart (runs async — connection will drop)
	send("Restarting service... reconnect in 5 seconds", "restarting")
	if canFlush {
		flusher.Flush()
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("systemctl", "restart", "aprs").Run()
	}()
}

func runCmd(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ─── Server config persistence ───────────────────────────────────────────────

// saveConfig writes the current AppConfig to server_config.json.
func saveConfig() error {
	config.RLock()
	data, err := json.MarshalIndent(config, "", "  ")
	config.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(configFile, data, 0600)
}

// loadSavedConfig reads server_config.json if it exists and applies it.
// Called once at startup before the upstream connection is established.
func loadSavedConfig() {
	data, err := os.ReadFile(configFile)
	if err != nil {
		log.Printf("No server_config.json found — using defaults")
		return
	}
	var saved AppConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		log.Printf("server_config.json parse error: %v — using defaults", err)
		return
	}
	config.Lock()
	config.ServerName     = saved.ServerName
	config.SoftwareVers   = saved.SoftwareVers
	config.Callsign       = saved.Callsign
	config.Passcode       = saved.Passcode
	config.UpstreamAddr   = saved.UpstreamAddr
	config.ServerFilter   = strings.TrimSpace(saved.ServerFilter)
	config.DropPiStar     = saved.DropPiStar
	config.DropDStar      = saved.DropDStar
	config.DropAPDesk     = saved.DropAPDesk
	config.EnableGeofence = saved.EnableGeofence
	config.CenterLat      = saved.CenterLat
	config.CenterLon      = saved.CenterLon
	config.RadiusKm       = saved.RadiusKm
	config.Unlock()
	log.Printf("Server config loaded: callsign=%s upstream=%s filter=%s",
		saved.Callsign, saved.UpstreamAddr, saved.ServerFilter)
}

// ─── First-run credential persistence ────────────────────────────────────────

const credsFile  = "creds.json"
const configFile = "server_config.json"

type storedCreds struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// credsConfigured returns true if creds.json exists and has non-empty values.
func credsConfigured() bool {
	adminCreds.RLock()
	defer adminCreds.RUnlock()
	return adminCreds.Username != "" && adminCreds.Password != ""
}

// loadOrInitCreds loads creds.json if it exists; otherwise sets empty creds
// so the server redirects to /setup on first visit.
func loadOrInitCreds() {
	data, err := os.ReadFile(credsFile)
	if err != nil {
		// First run — no creds file yet
		log.Printf("No creds.json found — first-run setup required at /setup")
		adminCreds.Lock()
		adminCreds.Username = ""
		adminCreds.Password = ""
		adminCreds.Unlock()
		return
	}
	var sc storedCreds
	if err := json.Unmarshal(data, &sc); err != nil || sc.Username == "" || sc.Password == "" {
		log.Printf("creds.json invalid — first-run setup required at /setup")
		adminCreds.Lock()
		adminCreds.Username = ""
		adminCreds.Password = ""
		adminCreds.Unlock()
		return
	}
	adminCreds.Lock()
	adminCreds.Username = sc.Username
	adminCreds.Password = sc.Password
	adminCreds.Unlock()
	log.Printf("Admin credentials loaded: %s", sc.Username)
}

// saveCreds writes current credentials to creds.json.
func saveCreds(username, password string) error {
	data, err := json.MarshalIndent(storedCreds{Username: username, Password: password}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(credsFile, data, 0600)
}

// handleSetup serves the first-run setup page and processes the form POST.
// Once creds are set it redirects to / permanently.
func handleSetup(w http.ResponseWriter, r *http.Request) {
	// If already configured, redirect away — setup is one-time only.
	if credsConfigured() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		confirm := r.FormValue("confirm")

		var errMsg string
		switch {
		case username == "":
			errMsg = "Username cannot be empty."
		case len(username) < 3:
			errMsg = "Username must be at least 3 characters."
		case len(password) < 8:
			errMsg = "Password must be at least 8 characters."
		case password != confirm:
			errMsg = "Passwords do not match."
		}

		if errMsg != "" {
			serveSetupPage(w, errMsg)
			return
		}

		if err := saveCreds(username, password); err != nil {
			log.Printf("Failed to save creds: %v", err)
			http.Error(w, "Failed to save credentials", http.StatusInternalServerError)
			return
		}
		adminCreds.Lock()
		adminCreds.Username = username
		adminCreds.Password = password
		adminCreds.Unlock()
		log.Printf("First-run setup complete — admin user: %s", username)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	serveSetupPage(w, "")
}

// handlePassword now also persists the new password to creds.json.
func handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.NewPassword) == "" {
		http.Error(w, "bad request: new_password required", http.StatusBadRequest)
		return
	}
	adminCreds.Lock()
	username := adminCreds.Username
	adminCreds.Password = req.NewPassword
	adminCreds.Unlock()
	if err := saveCreds(username, req.NewPassword); err != nil {
		log.Printf("Warning: password changed in memory but failed to persist: %v", err)
	}
	log.Printf("Admin password updated and persisted")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func serveSetupPage(w http.ResponseWriter, errMsg string) {
	errHTML := ""
	if errMsg != "" {
		errHTML = `<p class="text-red-400 text-sm mb-4 bg-red-900/30 p-3 rounded border border-red-700">` + errMsg + `</p>`
	}
	page := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>APRS Gateway — First Run Setup</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#111827;color:#f9fafb;font-family:system-ui,sans-serif;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:1rem}
.card{background:#1f2937;border:1px solid #374151;border-radius:12px;padding:2.5rem;width:100%;max-width:420px;box-shadow:0 25px 50px rgba(0,0,0,.5)}
.logo{text-align:center;margin-bottom:2rem}
.logo .icon{font-size:3rem;margin-bottom:.5rem}
.logo h1{font-size:1.5rem;font-weight:700;color:#60a5fa}
.logo p{color:#9ca3af;font-size:.875rem;margin-top:.25rem}
label{display:block;font-size:.7rem;font-weight:700;text-transform:uppercase;letter-spacing:.05em;color:#9ca3af;margin-bottom:.4rem;margin-top:1.25rem}
input{width:100%;background:#374151;border:1px solid #4b5563;border-radius:6px;padding:.75rem;color:#fff;font-size:.875rem;outline:none;transition:border-color .2s}
input:focus{border-color:#3b82f6}
.err{background:rgba(127,29,29,.4);border:1px solid #991b1b;color:#fca5a5;padding:.75rem 1rem;border-radius:6px;font-size:.875rem;margin-top:1rem}
button{width:100%;margin-top:1.5rem;background:#2563eb;color:#fff;border:none;padding:.875rem;border-radius:6px;font-weight:700;font-size:.875rem;text-transform:uppercase;letter-spacing:.1em;cursor:pointer;transition:background .2s}
button:hover{background:#1d4ed8}
.note{text-align:center;font-size:.7rem;color:#6b7280;margin-top:1.5rem}
.note code{color:#9ca3af}
</style>
</head>
<body>
<div class="card">
  <div class="logo">
    <div class="icon">📡</div>
    <h1>APRS Gateway</h1>
    <p>First-run setup — create your admin account</p>
  </div>
  ` + errHTML + `
  <form method="POST" action="/setup">
    <label>Admin Username</label>
    <input type="text" name="username" required minlength="3" autocomplete="username" placeholder="e.g. 2e0lxy">
    <label>Password</label>
    <input type="password" name="password" required minlength="8" autocomplete="new-password" placeholder="Minimum 8 characters">
    <label>Confirm Password</label>
    <input type="password" name="confirm" required autocomplete="new-password" placeholder="Repeat password">
    <button type="submit">Create Account &amp; Launch Gateway</button>
  </form>
  <p class="note">Credentials stored in <code>creds.json</code> on the server. Shown once only.</p>
</div>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(page))
}

// ─── Prometheus Metrics ───────────────────────────────────────────────────────

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	clientsMu.Lock()
	wsClients := len(clients)
	clientsMu.Unlock()

	historyMu.RLock()
	histLen := len(history)
	historyMu.RUnlock()

	rawRingMu.RLock()
	ringLen := len(rawRing)
	rawRingMu.RUnlock()

	objectStoreMu.RLock()
	objCount := len(objectStore)
	objectStoreMu.RUnlock()

	upSecs := time.Since(metricsStart).Seconds()
	pkts := atomic.LoadInt64(&totalPackets)
	dupes := atomic.LoadInt64(&dupePackets)
	bytes := atomic.LoadInt64(&upstreamBytes)
	upConn := atomic.LoadInt32(&upstreamConnected)

	config.RLock()
	call := config.Callsign
	filter := config.ServerFilter
	config.RUnlock()

	fmt.Fprintf(w, "# HELP aprs_packets_total Total APRS packets received from upstream\n")
	fmt.Fprintf(w, "# TYPE aprs_packets_total counter\n")
	fmt.Fprintf(w, "aprs_packets_total{callsign=%q} %d\n\n", call, pkts)

	fmt.Fprintf(w, "# HELP aprs_dupe_packets_total Duplicate packets dropped\n")
	fmt.Fprintf(w, "# TYPE aprs_dupe_packets_total counter\n")
	fmt.Fprintf(w, "aprs_dupe_packets_total{callsign=%q} %d\n\n", call, dupes)

	fmt.Fprintf(w, "# HELP aprs_upstream_bytes_total Bytes received from APRS-IS upstream\n")
	fmt.Fprintf(w, "# TYPE aprs_upstream_bytes_total counter\n")
	fmt.Fprintf(w, "aprs_upstream_bytes_total{callsign=%q} %d\n\n", call, bytes)

	fmt.Fprintf(w, "# HELP aprs_upstream_connected 1 if connected to APRS-IS upstream, 0 otherwise\n")
	fmt.Fprintf(w, "# TYPE aprs_upstream_connected gauge\n")
	fmt.Fprintf(w, "aprs_upstream_connected{callsign=%q,filter=%q} %d\n\n", call, filter, upConn)

	fmt.Fprintf(w, "# HELP aprs_websocket_clients Current WebSocket client connections\n")
	fmt.Fprintf(w, "# TYPE aprs_websocket_clients gauge\n")
	fmt.Fprintf(w, "aprs_websocket_clients %d\n\n", wsClients)

	fmt.Fprintf(w, "# HELP aprs_history_packets Packets in position history store\n")
	fmt.Fprintf(w, "# TYPE aprs_history_packets gauge\n")
	fmt.Fprintf(w, "aprs_history_packets %d\n\n", histLen)

	fmt.Fprintf(w, "# HELP aprs_raw_ring_packets Packets in 1-hour raw ring buffer\n")
	fmt.Fprintf(w, "# TYPE aprs_raw_ring_packets gauge\n")
	fmt.Fprintf(w, "aprs_raw_ring_packets %d\n\n", ringLen)

	fmt.Fprintf(w, "# HELP aprs_objects_active Active APRS objects/items on map\n")
	fmt.Fprintf(w, "# TYPE aprs_objects_active gauge\n")
	fmt.Fprintf(w, "aprs_objects_active %d\n\n", objCount)

	fmt.Fprintf(w, "# HELP aprs_uptime_seconds Seconds since server start\n")
	fmt.Fprintf(w, "# TYPE aprs_uptime_seconds counter\n")
	fmt.Fprintf(w, "aprs_uptime_seconds %.0f\n\n", upSecs)

	pktRate := 0.0
	if upSecs > 0 { pktRate = float64(pkts) / upSecs }
	fmt.Fprintf(w, "# HELP aprs_packets_per_second Average packets per second since start\n")
	fmt.Fprintf(w, "# TYPE aprs_packets_per_second gauge\n")
	fmt.Fprintf(w, "aprs_packets_per_second %.3f\n\n", pktRate)
}

// ─── GeoJSON Export ───────────────────────────────────────────────────────────

func handleGeoJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Content-Disposition", `attachment; filename="aprs-stations.geojson"`)

	historyMu.RLock()
	snap := make([]HistoryPacket, len(history))
	copy(snap, history)
	historyMu.RUnlock()

	// Deduplicate - latest position per callsign
	latest := make(map[string]HistoryPacket)
	for _, p := range snap {
		if _, ok := latest[p.Callsign]; !ok {
			latest[p.Callsign] = p
		}
		if p.Timestamp > latest[p.Callsign].Timestamp {
			latest[p.Callsign] = p
		}
	}

	type GeoFeature struct {
		Type       string                 `json:"type"`
		Geometry   map[string]interface{} `json:"geometry"`
		Properties map[string]interface{} `json:"properties"`
	}
	type GeoCollection struct {
		Type     string       `json:"type"`
		Features []GeoFeature `json:"features"`
	}

	fc := GeoCollection{Type: "FeatureCollection"}
	for _, p := range latest {
		fc.Features = append(fc.Features, GeoFeature{
			Type: "Feature",
			Geometry: map[string]interface{}{
				"type":        "Point",
				"coordinates": []float64{p.Lon, p.Lat},
			},
			Properties: map[string]interface{}{
				"callsign":  p.Callsign,
				"symbol":    p.Symbol,
				"path":      p.Path,
				"timestamp": p.Timestamp,
				"raw":       p.Raw,
			},
		})
	}

	// Add objects
	objectStoreMu.RLock()
	for _, o := range objectStore {
		if !o.Killed {
			fc.Features = append(fc.Features, GeoFeature{
				Type: "Feature",
				Geometry: map[string]interface{}{
					"type":        "Point",
					"coordinates": []float64{o.Lon, o.Lat},
				},
				Properties: map[string]interface{}{
					"callsign":  o.Name,
					"name":      o.Name,
					"owner":     o.Owner,
					"symbol":    o.Symbol,
					"comment":   o.Comment,
					"timestamp": o.Timestamp,
					"type":      "object",
				},
			})
		}
	}
	objectStoreMu.RUnlock()

	json.NewEncoder(w).Encode(fc)
}

// ─── KML Export ───────────────────────────────────────────────────────────────

func handleKML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.google-earth.kml+xml")
	w.Header().Set("Content-Disposition", `attachment; filename="aprs-stations.kml"`)

	historyMu.RLock()
	snap := make([]HistoryPacket, len(history))
	copy(snap, history)
	historyMu.RUnlock()

	latest := make(map[string]HistoryPacket)
	for _, p := range snap {
		if ex, ok := latest[p.Callsign]; !ok || p.Timestamp > ex.Timestamp {
			latest[p.Callsign] = p
		}
	}

	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<kml xmlns="http://www.opengis.net/kml/2.2">
<Document>
<name>APRS Stations</name>
<description>Exported from Advanced APRS Go Server</description>
`)
	for _, p := range latest {
		fmt.Fprintf(w, `<Placemark>
  <name>%s</name>
  <description>%s&#10;Path: %s</description>
  <Point><coordinates>%f,%f,0</coordinates></Point>
</Placemark>
`, p.Callsign, p.Raw, p.Path, p.Lon, p.Lat)
	}

	objectStoreMu.RLock()
	for _, o := range objectStore {
		if !o.Killed {
			fmt.Fprintf(w, `<Placemark>
  <name>%s (Object)</name>
  <description>Owner: %s&#10;%s</description>
  <Point><coordinates>%f,%f,0</coordinates></Point>
</Placemark>
`, o.Name, o.Owner, o.Comment, o.Lon, o.Lat)
		}
	}
	objectStoreMu.RUnlock()

	fmt.Fprintf(w, "</Document>\n</kml>\n")
}

// ─── Webhooks & API Keys ──────────────────────────────────────────────────────

const webhooksFile = "webhooks.json"
const apiKeysFile  = "apikeys.json"

func loadWebhooksAndKeys() {
	// Load webhooks
	if data, err := os.ReadFile(webhooksFile); err == nil {
		webhooksMu.Lock()
		json.Unmarshal(data, &webhooks)
		webhooksMu.Unlock()
		log.Printf("Loaded %d webhooks", len(webhooks))
	}
	// Load API keys
	if data, err := os.ReadFile(apiKeysFile); err == nil {
		apiKeysMu.Lock()
		json.Unmarshal(data, &apiKeys)
		apiKeysMu.Unlock()
		log.Printf("Loaded %d API keys", len(apiKeys))
	}
}

func saveWebhooks() {
	webhooksMu.RLock()
	data, _ := json.MarshalIndent(webhooks, "", "  ")
	webhooksMu.RUnlock()
	os.WriteFile(webhooksFile, data, 0600)
}

func saveAPIKeys() {
	apiKeysMu.RLock()
	data, _ := json.MarshalIndent(apiKeys, "", "  ")
	apiKeysMu.RUnlock()
	os.WriteFile(apiKeysFile, data, 0600)
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func generateAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "aprs_" + fmt.Sprintf("%x", b)
}

// handleWebhooks - GET returns all webhooks, POST creates one
func handleWebhooks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		webhooksMu.RLock()
		json.NewEncoder(w).Encode(webhooks)
		webhooksMu.RUnlock()

	case http.MethodPost:
		var wh WebhookConfig
		if err := json.NewDecoder(r.Body).Decode(&wh); err != nil {
			http.Error(w, `{"error":"invalid json"}`, 400); return
		}
		if wh.URL == "" {
			http.Error(w, `{"error":"url required"}`, 400); return
		}
		wh.ID      = generateID()
		wh.Enabled = true
		webhooksMu.Lock()
		webhooks = append(webhooks, wh)
		webhooksMu.Unlock()
		saveWebhooks()
		json.NewEncoder(w).Encode(wh)

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleWebhookDelete - DELETE /api/webhooks/{id}
func handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405); return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/webhooks/")
	webhooksMu.Lock()
	newWH := webhooks[:0]
	for _, wh := range webhooks {
		if wh.ID != id { newWH = append(newWH, wh) }
	}
	webhooks = newWH
	webhooksMu.Unlock()
	saveWebhooks()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// handleAPIKeys - GET returns all keys (with key masked), POST creates one
func handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		apiKeysMu.RLock()
		result := make([]map[string]interface{}, 0)
		for _, k := range apiKeys {
			// Mask key - only show prefix
			masked := k.Key[:12] + "..."
			result = append(result, map[string]interface{}{
				"id": k.ID, "name": k.Name, "key": masked,
				"created": k.Created, "last_used": k.LastUsed, "read_only": k.ReadOnly,
			})
		}
		apiKeysMu.RUnlock()
		json.NewEncoder(w).Encode(result)

	case http.MethodPost:
		var req struct {
			Name     string `json:"name"`
			ReadOnly bool   `json:"read_only"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, 400); return
		}
		if req.Name == "" { req.Name = "API Key" }
		k := APIKey{
			ID: generateID(), Name: req.Name,
			Key: generateAPIKey(), Created: time.Now().Unix(),
			ReadOnly: req.ReadOnly,
		}
		apiKeysMu.Lock()
		apiKeys[k.Key] = k
		apiKeysMu.Unlock()
		saveAPIKeys()
		// Return full key ONCE on creation
		json.NewEncoder(w).Encode(k)

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleAPIKeyDelete - DELETE /api/keys/{id}
func handleAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405); return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	apiKeysMu.Lock()
	for key, k := range apiKeys {
		if k.ID == id { delete(apiKeys, key); break }
	}
	apiKeysMu.Unlock()
	saveAPIKeys()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// apiKeyAuth middleware - accepts either Basic Auth OR X-API-Key header
func apiKeyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Try X-API-Key header
		key := r.Header.Get("X-API-Key")
		if key == "" { key = r.URL.Query().Get("api_key") }
		if key != "" {
			apiKeysMu.Lock()
			k, ok := apiKeys[key]
			if ok {
				k.LastUsed = time.Now().Unix()
				apiKeys[key] = k
				apiKeysMu.Unlock()
				next(w, r)
				return
			}
			apiKeysMu.Unlock()
		}
		// Fall through to basic auth
		user, pass, ok := r.BasicAuth()
		adminCreds.RLock()
		wantUser, wantPass := adminCreds.Username, adminCreds.Password
		adminCreds.RUnlock()
		if !ok || user != wantUser || pass != wantPass {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next(w, r)
	}
}

// fireWebhooks sends an event to all matching enabled webhooks
func fireWebhooks(event string, callsign string, data interface{}) {
	webhooksMu.RLock()
	whs := make([]WebhookConfig, len(webhooks))
	copy(whs, webhooks)
	webhooksMu.RUnlock()

	payload := WebhookEvent{
		Event:     event,
		Timestamp: time.Now().Unix(),
		Callsign:  callsign,
		Data:      data,
	}
	body, _ := json.Marshal(payload)

	for i, wh := range whs {
		if !wh.Enabled { continue }
		// Check callsign filter (empty = all)
		if wh.Callsign != "" && !strings.EqualFold(wh.Callsign, callsign) &&
			!strings.HasSuffix(callsign, wh.Callsign) { continue }
		// Check event filter
		if len(wh.Events) > 0 {
			matched := false
			for _, e := range wh.Events {
				if e == event || e == "*" { matched = true; break }
			}
			if !matched { continue }
		}

		go func(wh WebhookConfig, idx int) {
			client := &http.Client{Timeout: 8 * time.Second}
			req, err := http.NewRequest("POST", wh.URL, strings.NewReader(string(body)))
			if err != nil { return }
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-APRS-Event", event)
			req.Header.Set("X-APRS-Callsign", callsign)
			if wh.Secret != "" {
				req.Header.Set("X-APRS-Secret", wh.Secret)
			}
			resp, err := client.Do(req)
			status := 0
			if err == nil { status = resp.StatusCode; resp.Body.Close() }
			webhooksMu.Lock()
			for j := range webhooks {
				if webhooks[j].ID == wh.ID {
					webhooks[j].LastFired  = time.Now().Unix()
					webhooks[j].LastStatus = status
					break
				}
			}
			webhooksMu.Unlock()
			saveWebhooks()
		}(wh, i)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// MEMBER SYSTEM
// ═══════════════════════════════════════════════════════════════════════════════

const membersFile  = "members.json"
const sessionTTL   = 30 * 24 * time.Hour   // 30 day sessions

// ── Types ─────────────────────────────────────────────────────────────────────

type Member struct {
	ID           string    `json:"id"`
	Callsign     string    `json:"callsign"`             // primary callsign (login)
	Password     string    `json:"password"`             // bcrypt-style: sha256+salt stored as hex
	Salt         string    `json:"salt"`
	Name         string    `json:"name,omitempty"`
	Email        string    `json:"email,omitempty"`
	Callsigns    []string  `json:"callsigns"`            // all owned callsigns
	Passcode     int       `json:"passcode"`             // APRS-IS passcode
	Watchlist    []string  `json:"watchlist"`
	Created      int64     `json:"created"`
	LastLogin    int64     `json:"last_login,omitempty"`
	Verified     bool      `json:"verified"`
}

type StoredMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Text      string `json:"text"`
	Ts        int64  `json:"ts"`
	Read      bool   `json:"read"`
	Raw       string `json:"raw,omitempty"`
}

type MemberSession struct {
	Token    string `json:"token"`
	MemberID string `json:"member_id"`
	Expires  int64  `json:"expires"`
}

type MemberStore struct {
	Members  map[string]*Member   `json:"members"`   // id -> member
	Sessions map[string]*MemberSession `json:"sessions"` // token -> session
	Messages map[string][]StoredMessage `json:"messages"` // callsign -> []msgs
}

var (
	memberStore   *MemberStore
	memberStoreMu sync.RWMutex
)

// ── Persistence ───────────────────────────────────────────────────────────────

func loadMemberStore() {
	memberStoreMu.Lock()
	defer memberStoreMu.Unlock()
	memberStore = &MemberStore{
		Members:  make(map[string]*Member),
		Sessions: make(map[string]*MemberSession),
		Messages: make(map[string][]StoredMessage),
	}
	data, err := os.ReadFile(membersFile)
	if err != nil { return }
	if err := json.Unmarshal(data, memberStore); err != nil {
		log.Printf("Warning: could not parse members.json: %v", err)
		memberStore = &MemberStore{
			Members:  make(map[string]*Member),
			Sessions: make(map[string]*MemberSession),
			Messages: make(map[string][]StoredMessage),
		}
	}
	log.Printf("Loaded %d members", len(memberStore.Members))
}

func saveMemberStore() {
	data, err := json.MarshalIndent(memberStore, "", "  ")
	if err != nil { return }
	os.WriteFile(membersFile, data, 0600)
}

// ── Password hashing (no bcrypt dependency - use SHA256+salt) ─────────────────

// hashPassword uses SHA-256 with salt + 10000 iterations (PBKDF2-lite)
func hashPassword(password, salt string) string {
	data := []byte(password + ":" + salt)
	for i := 0; i < 10000; i++ {
		h := sha256.Sum256(data)
		data = h[:]
	}
	return hex.EncodeToString(data)
}

func calcAPRSPasscode(callsign string) int {
	call := strings.ToUpper(strings.SplitN(callsign, "-", 2)[0])
	hash := 0x73e2
	for i := 0; i < len(call); i += 2 {
		hash ^= int(call[i]) << 8
		if i+1 < len(call) {
			hash ^= int(call[i+1])
		}
	}
	return hash & 0x7fff
}

// ── Session helpers ───────────────────────────────────────────────────────────

func generateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func getMemberFromRequest(r *http.Request) *Member {
	token := r.Header.Get("X-Member-Token")
	if token == "" {
		if c, err := r.Cookie("member_token"); err == nil {
			token = c.Value
		}
	}
	if token == "" { return nil }
	memberStoreMu.RLock()
	defer memberStoreMu.RUnlock()
	sess, ok := memberStore.Sessions[token]
	if !ok || sess.Expires < time.Now().Unix() { return nil }
	m, ok := memberStore.Members[sess.MemberID]
	if !ok { return nil }
	return m
}

func cleanExpiredSessions() {
	for {
		time.Sleep(1 * time.Hour)
		memberStoreMu.Lock()
		now := time.Now().Unix()
		for tok, sess := range memberStore.Sessions {
			if sess.Expires < now { delete(memberStore.Sessions, tok) }
		}
		saveMemberStore()
		memberStoreMu.Unlock()
	}
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// POST /api/member/register
func handleMemberRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	var req struct {
		Callsign string `json:"callsign"`
		Password string `json:"password"`
		Name     string `json:"name"`
		Email    string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400); return
	}
	req.Callsign = strings.ToUpper(strings.TrimSpace(req.Callsign))
	if len(req.Callsign) < 3 || len(req.Password) < 6 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "callsign must be 3+ chars, password 6+ chars"})
		return
	}
	memberStoreMu.Lock()
	defer memberStoreMu.Unlock()
	// Check callsign not already registered
	for _, m := range memberStore.Members {
		if strings.ToUpper(m.Callsign) == req.Callsign {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "callsign already registered"})
			return
		}
	}
	salt := generateToken()[:16]
	member := &Member{
		ID:        generateID(),
		Callsign:  req.Callsign,
		Password:  hashPassword(req.Password, salt),
		Salt:      salt,
		Name:      req.Name,
		Email:     req.Email,
		Callsigns: []string{req.Callsign},
		Passcode:  calcAPRSPasscode(req.Callsign),
		Watchlist: []string{},
		Created:   time.Now().Unix(),
		Verified:  true,
	}
	memberStore.Members[member.ID] = member
	saveMemberStore()
	go auditLog("system", "member.register", member.Callsign, "", getClientIP(r))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true, "callsign": member.Callsign, "passcode": member.Passcode,
	})
}

// POST /api/member/login
func handleMemberLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	var req struct {
		Callsign string `json:"callsign"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400); return
	}
	req.Callsign = strings.ToUpper(strings.TrimSpace(req.Callsign))
	memberStoreMu.Lock()
	defer memberStoreMu.Unlock()
	var found *Member
	for _, m := range memberStore.Members {
		if strings.ToUpper(m.Callsign) == req.Callsign {
			found = m; break
		}
	}
	if found == nil || hashPassword(req.Password, found.Salt) != found.Password {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid callsign or password"})
		return
	}
	token := generateToken()
	memberStore.Sessions[token] = &MemberSession{
		Token: token, MemberID: found.ID,
		Expires: time.Now().Add(sessionTTL).Unix(),
	}
	found.LastLogin = time.Now().Unix()
	saveMemberStore()
	http.SetCookie(w, &http.Cookie{
		Name: "member_token", Value: token,
		Expires: time.Now().Add(sessionTTL),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true, "token": token,
		"callsign": found.Callsign,
		"name":     found.Name,
		"passcode": found.Passcode,
		"callsigns": found.Callsigns,
		"watchlist": found.Watchlist,
	})
}

// POST /api/member/logout
func handleMemberLogout(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Member-Token")
	if token == "" {
		if c, err := r.Cookie("member_token"); err == nil { token = c.Value }
	}
	if token != "" {
		memberStoreMu.Lock()
		delete(memberStore.Sessions, token)
		saveMemberStore()
		memberStoreMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "member_token", Value: "", MaxAge: -1, Path: "/"})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// GET /api/member/profile  PUT /api/member/profile
func handleMemberProfile(w http.ResponseWriter, r *http.Request) {
	m := getMemberFromRequest(r)
	if m == nil { w.WriteHeader(401); json.NewEncoder(w).Encode(map[string]string{"error":"not logged in"}); return }
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		// Return profile including offline messages count
		memberStoreMu.RLock()
		msgs := memberStore.Messages[strings.ToUpper(m.Callsign)]
		unread := 0
		for _, msg := range msgs { if !msg.Read { unread++ } }
		memberStoreMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": m.ID, "callsign": m.Callsign, "name": m.Name, "email": m.Email,
			"callsigns": m.Callsigns, "passcode": m.Passcode,
			"watchlist": m.Watchlist, "created": m.Created, "last_login": m.LastLogin,
			"unread_messages": unread,
		})
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Name      string   `json:"name"`
			Email     string   `json:"email"`
			Callsigns []string `json:"callsigns"`
			Watchlist []string `json:"watchlist"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400); json.NewEncoder(w).Encode(map[string]string{"error":"invalid json"}); return
		}
		memberStoreMu.Lock()
		if req.Name != ""      { memberStore.Members[m.ID].Name  = req.Name }
		if req.Email != ""     { memberStore.Members[m.ID].Email = req.Email }
		if req.Callsigns != nil { memberStore.Members[m.ID].Callsigns = req.Callsigns }
		if req.Watchlist != nil { memberStore.Members[m.ID].Watchlist = req.Watchlist }
		saveMemberStore()
		memberStoreMu.Unlock()
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	http.Error(w, "method not allowed", 405)
}

// GET /api/member/messages  PATCH /api/member/messages (mark read)
func handleMemberMessages(w http.ResponseWriter, r *http.Request) {
	m := getMemberFromRequest(r)
	if m == nil { w.WriteHeader(401); json.NewEncoder(w).Encode(map[string]string{"error":"not logged in"}); return }
	w.Header().Set("Content-Type", "application/json")
	call := strings.ToUpper(m.Callsign)
	if r.Method == http.MethodGet {
		memberStoreMu.RLock()
		msgs := memberStore.Messages[call]
		if msgs == nil { msgs = []StoredMessage{} }
		memberStoreMu.RUnlock()
		json.NewEncoder(w).Encode(msgs)
		return
	}
	if r.Method == http.MethodPatch {
		// Mark all as read
		memberStoreMu.Lock()
		for i := range memberStore.Messages[call] {
			memberStore.Messages[call][i].Read = true
		}
		saveMemberStore()
		memberStoreMu.Unlock()
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	http.Error(w, "method not allowed", 405)
}

// POST /api/member/password  (change password)
func handleMemberPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	m := getMemberFromRequest(r)
	if m == nil { w.WriteHeader(401); json.NewEncoder(w).Encode(map[string]string{"error":"not logged in"}); return }
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400); json.NewEncoder(w).Encode(map[string]string{"error":"invalid json"}); return
	}
	memberStoreMu.Lock()
	defer memberStoreMu.Unlock()
	mem := memberStore.Members[m.ID]
	if hashPassword(req.Current, mem.Salt) != mem.Password {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error":"current password incorrect"})
		return
	}
	if len(req.New) < 6 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error":"new password must be 6+ chars"})
		return
	}
	mem.Password = hashPassword(req.New, mem.Salt)
	saveMemberStore()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// storeMessageForMember stores an incoming APRS message for a registered member
func storeMessageForMember(to, from, text, raw string, ts int64) {
	toUpper := strings.ToUpper(strings.TrimSpace(to))
	memberStoreMu.Lock()
	defer memberStoreMu.Unlock()
	// Find member with this callsign (check all owned callsigns)
	var targetCall string
	for _, m := range memberStore.Members {
		for _, c := range m.Callsigns {
			if strings.ToUpper(c) == toUpper || strings.ToUpper(strings.SplitN(c,"-",2)[0]) == strings.ToUpper(strings.SplitN(toUpper,"-",2)[0]) {
				targetCall = strings.ToUpper(m.Callsign)
				break
			}
		}
		if targetCall != "" { break }
	}
	if targetCall == "" { return }
	msg := StoredMessage{
		ID: generateID(), From: from, To: to,
		Text: text, Ts: ts, Read: false, Raw: raw,
	}
	memberStore.Messages[targetCall] = append(memberStore.Messages[targetCall], msg)
	// Keep last 500 messages per member
	if len(memberStore.Messages[targetCall]) > 500 {
		memberStore.Messages[targetCall] = memberStore.Messages[targetCall][len(memberStore.Messages[targetCall])-500:]
	}
	saveMemberStore()
}

// ═══════════════════════════════════════════════════════════════════════════════
// ADMIN FEATURES: Member Mgmt, Ban List, Backup/Restore, MOTD, Audit Log
// ═══════════════════════════════════════════════════════════════════════════════

const banFile     = "bans.json"
const motdFile    = "motd.json"
const auditFile   = "audit.log"

// ── Ban list ────────────────────────────────────────────────────────────────

type BanEntry struct {
	Callsign  string `json:"callsign"`   // exact or with wildcard *
	Reason    string `json:"reason"`
	Added     int64  `json:"added"`
	AddedBy   string `json:"added_by"`
}

var (
	banList   []BanEntry
	banListMu sync.RWMutex
)

func loadBanList() {
	banListMu.Lock()
	defer banListMu.Unlock()
	banList = []BanEntry{}
	data, err := os.ReadFile(banFile)
	if err != nil { return }
	json.Unmarshal(data, &banList)
	log.Printf("Loaded %d ban entries", len(banList))
}

func saveBanList() {
	data, _ := json.MarshalIndent(banList, "", "  ")
	os.WriteFile(banFile, data, 0644)
}

// isCallsignBanned: returns reason if banned, empty string if not
func isCallsignBanned(call string) string {
	call = strings.ToUpper(strings.TrimSpace(call))
	banListMu.RLock()
	defer banListMu.RUnlock()
	for _, b := range banList {
		pat := strings.ToUpper(b.Callsign)
		if pat == call { return b.Reason }
		if strings.HasSuffix(pat, "*") {
			prefix := strings.TrimSuffix(pat, "*")
			if strings.HasPrefix(call, prefix) { return b.Reason }
		}
	}
	return ""
}

// ── MOTD ───────────────────────────────────────────────────────────────────

type MOTD struct {
	Enabled  bool   `json:"enabled"`
	Message  string `json:"message"`
	Level    string `json:"level"`     // info, warning, success, error
	Dismissable bool `json:"dismissable"`
	Updated  int64  `json:"updated"`
}

var (
	motd   MOTD
	motdMu sync.RWMutex
)

func loadMOTD() {
	motdMu.Lock()
	defer motdMu.Unlock()
	data, err := os.ReadFile(motdFile)
	if err != nil { motd = MOTD{}; return }
	json.Unmarshal(data, &motd)
}

func saveMOTD() {
	data, _ := json.MarshalIndent(motd, "", "  ")
	os.WriteFile(motdFile, data, 0644)
}

// ── Audit log ──────────────────────────────────────────────────────────────

type AuditEntry struct {
	Ts      int64  `json:"ts"`
	Actor   string `json:"actor"`   // who did it (admin or "system")
	Action  string `json:"action"`  // e.g. "member.delete", "ban.add"
	Target  string `json:"target"`  // affected entity
	Details string `json:"details,omitempty"`
	IP      string `json:"ip,omitempty"`
}

var auditMu sync.Mutex


// Router for /api/admin/members/{id} and /api/admin/members/{id}/messages
func handleAdminMemberRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/members/")
	if path == "" { http.Error(w, "no id", 400); return }
	if strings.HasSuffix(path, "/messages") {
		handleAdminMemberMessages(w, r)
		return
	}
	handleAdminMember(w, r)
}

func auditLog(actor, action, target, details, ip string) {
	auditMu.Lock()
	defer auditMu.Unlock()
	entry := AuditEntry{
		Ts: time.Now().Unix(), Actor: actor, Action: action,
		Target: target, Details: details, IP: ip,
	}
	data, _ := json.Marshal(entry)
	f, err := os.OpenFile(auditFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil { return }
	defer f.Close()
	f.Write(append(data, '\n'))
}

func readAuditLog(lines int) []AuditEntry {
	data, err := os.ReadFile(auditFile)
	if err != nil { return []AuditEntry{} }
	parts := strings.Split(strings.TrimSpace(string(data)), "\n")
	if lines > 0 && len(parts) > lines { parts = parts[len(parts)-lines:] }
	entries := []AuditEntry{}
	for _, line := range parts {
		if line == "" { continue }
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			entries = append(entries, e)
		}
	}
	// Reverse so newest first
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

// ── HTTP Handlers ──────────────────────────────────────────────────────────

func getClientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		return strings.SplitN(xf, ",", 2)[0]
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// GET /api/admin/members
func handleAdminMembers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	memberStoreMu.RLock()
	defer memberStoreMu.RUnlock()
	type publicMember struct {
		ID         string   `json:"id"`
		Callsign   string   `json:"callsign"`
		Name       string   `json:"name"`
		Email      string   `json:"email"`
		Callsigns  []string `json:"callsigns"`
		Passcode   int      `json:"passcode"`
		WatchCount int      `json:"watch_count"`
		MsgCount   int      `json:"msg_count"`
		Unread     int      `json:"unread"`
		Created    int64    `json:"created"`
		LastLogin  int64    `json:"last_login"`
		Sessions   int      `json:"sessions"`
	}
	list := []publicMember{}
	for _, m := range memberStore.Members {
		msgs := memberStore.Messages[strings.ToUpper(m.Callsign)]
		unread := 0
		for _, mg := range msgs { if !mg.Read { unread++ } }
		sessions := 0
		for _, s := range memberStore.Sessions { if s.MemberID == m.ID { sessions++ } }
		list = append(list, publicMember{
			ID: m.ID, Callsign: m.Callsign, Name: m.Name, Email: m.Email,
			Callsigns: m.Callsigns, Passcode: m.Passcode,
			WatchCount: len(m.Watchlist), MsgCount: len(msgs), Unread: unread,
			Created: m.Created, LastLogin: m.LastLogin, Sessions: sessions,
		})
	}
	json.NewEncoder(w).Encode(list)
}

// PUT /api/admin/members/{id} : update member
// DELETE /api/admin/members/{id} : delete member
func handleAdminMember(w http.ResponseWriter, r *http.Request) {
	ip := getClientIP(r)
	// Extract ID from path
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/members/")
	id = strings.SplitN(id, "/", 2)[0]
	if id == "" { http.Error(w, "no id", 400); return }
	w.Header().Set("Content-Type", "application/json")

	memberStoreMu.Lock()
	defer memberStoreMu.Unlock()
	mem, ok := memberStore.Members[id]
	if !ok { w.WriteHeader(404); json.NewEncoder(w).Encode(map[string]string{"error":"not found"}); return }

	if r.Method == http.MethodDelete {
		delete(memberStore.Members, id)
		delete(memberStore.Messages, strings.ToUpper(mem.Callsign))
		// Kill all sessions for this member
		for tok, s := range memberStore.Sessions {
			if s.MemberID == id { delete(memberStore.Sessions, tok) }
		}
		saveMemberStore()
		auditLog("admin", "member.delete", mem.Callsign, mem.ID, ip)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}

	if r.Method == http.MethodPut {
		var req struct {
			Name      string   `json:"name"`
			Email     string   `json:"email"`
			Callsigns []string `json:"callsigns"`
			Watchlist []string `json:"watchlist"`
			ResetPassword string `json:"reset_password"` // new password if set
			ForceLogout   bool   `json:"force_logout"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		changes := []string{}
		if req.Name != "" && req.Name != mem.Name {
			mem.Name = req.Name; changes = append(changes, "name")
		}
		if req.Email != mem.Email {
			mem.Email = req.Email; changes = append(changes, "email")
		}
		if req.Callsigns != nil {
			mem.Callsigns = req.Callsigns; changes = append(changes, "callsigns")
		}
		if req.Watchlist != nil {
			mem.Watchlist = req.Watchlist; changes = append(changes, "watchlist")
		}
		if req.ResetPassword != "" && len(req.ResetPassword) >= 6 {
			mem.Password = hashPassword(req.ResetPassword, mem.Salt)
			changes = append(changes, "password")
			// Force logout when password changes
			req.ForceLogout = true
		}
		if req.ForceLogout {
			for tok, s := range memberStore.Sessions {
				if s.MemberID == id { delete(memberStore.Sessions, tok) }
			}
			changes = append(changes, "sessions cleared")
		}
		saveMemberStore()
		auditLog("admin", "member.update", mem.Callsign, strings.Join(changes,","), ip)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "changes": changes})
		return
	}

	http.Error(w, "method not allowed", 405)
}

// GET /api/admin/members/{id}/messages : view member's stored messages
func handleAdminMemberMessages(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/members/")
	id = strings.TrimSuffix(id, "/messages")
	w.Header().Set("Content-Type", "application/json")
	memberStoreMu.RLock()
	defer memberStoreMu.RUnlock()
	mem, ok := memberStore.Members[id]
	if !ok { w.WriteHeader(404); return }
	msgs := memberStore.Messages[strings.ToUpper(mem.Callsign)]
	if msgs == nil { msgs = []StoredMessage{} }
	json.NewEncoder(w).Encode(msgs)
}

// GET/POST /api/admin/bans
func handleAdminBans(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ip := getClientIP(r)
	if r.Method == http.MethodGet {
		banListMu.RLock()
		defer banListMu.RUnlock()
		json.NewEncoder(w).Encode(banList)
		return
	}
	if r.Method == http.MethodPost {
		var req BanEntry
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", 400); return
		}
		req.Callsign = strings.ToUpper(strings.TrimSpace(req.Callsign))
		req.Added = time.Now().Unix()
		req.AddedBy = "admin"
		banListMu.Lock()
		// Update if exists, else append
		updated := false
		for i, b := range banList {
			if strings.ToUpper(b.Callsign) == req.Callsign {
				banList[i] = req; updated = true; break
			}
		}
		if !updated { banList = append(banList, req) }
		saveBanList()
		banListMu.Unlock()
		auditLog("admin", "ban.add", req.Callsign, req.Reason, ip)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	if r.Method == http.MethodDelete {
		call := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("callsign")))
		if call == "" { http.Error(w, "no callsign", 400); return }
		banListMu.Lock()
		newList := []BanEntry{}
		for _, b := range banList {
			if strings.ToUpper(b.Callsign) != call { newList = append(newList, b) }
		}
		banList = newList
		saveBanList()
		banListMu.Unlock()
		auditLog("admin", "ban.remove", call, "", ip)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	http.Error(w, "method not allowed", 405)
}

// GET/POST /api/admin/motd
// GET /api/motd (public)
func handleAdminMOTD(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		motdMu.RLock()
		defer motdMu.RUnlock()
		json.NewEncoder(w).Encode(motd)
		return
	}
	if r.Method == http.MethodPost {
		var req MOTD
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", 400); return
		}
		motdMu.Lock()
		motd = req; motd.Updated = time.Now().Unix()
		saveMOTD()
		motdMu.Unlock()
		auditLog("admin", "motd.update", "", req.Message[:minLen(req.Message,50)], getClientIP(r))
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	http.Error(w, "method not allowed", 405)
}

func handlePublicMOTD(w http.ResponseWriter, r *http.Request) {
	motdMu.RLock()
	defer motdMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if !motd.Enabled || motd.Message == "" {
		json.NewEncoder(w).Encode(map[string]bool{"enabled": false})
		return
	}
	json.NewEncoder(w).Encode(motd)
}

// GET /api/admin/audit?limit=100
func handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 { limit = v }
	}
	entries := readAuditLog(limit)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// GET /api/admin/backup : returns zip of all state
func handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="aprs-backup-%s.json"`, time.Now().Format("2006-01-02-150405")))
	backup := map[string]interface{}{
		"version": AppVersion,
		"timestamp": time.Now().Unix(),
		"server_config": loadConfigFromFile(),
		"members": memberStore,
		"bans": banList,
		"motd": motd,
		"webhooks": webhooks,
		"api_keys": apiKeys,
	}
	json.NewEncoder(w).Encode(backup)
	auditLog("admin", "backup.download", "", "", getClientIP(r))
}

// POST /api/admin/restore : upload backup JSON
func handleAdminRestore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 { http.Error(w, "no body", 400); return }
	var backup map[string]json.RawMessage
	if err := json.Unmarshal(body, &backup); err != nil {
		http.Error(w, "invalid json", 400); return
	}
	restored := []string{}
	if mb, ok := backup["members"]; ok {
		var ms MemberStore
		if json.Unmarshal(mb, &ms) == nil {
			memberStoreMu.Lock()
			memberStore = &ms
			saveMemberStore()
			memberStoreMu.Unlock()
			restored = append(restored, "members")
		}
	}
	if bb, ok := backup["bans"]; ok {
		var bl []BanEntry
		if json.Unmarshal(bb, &bl) == nil {
			banListMu.Lock()
			banList = bl
			saveBanList()
			banListMu.Unlock()
			restored = append(restored, "bans")
		}
	}
	if mm, ok := backup["motd"]; ok {
		var m MOTD
		if json.Unmarshal(mm, &m) == nil {
			motdMu.Lock()
			motd = m
			saveMOTD()
			motdMu.Unlock()
			restored = append(restored, "motd")
		}
	}
	auditLog("admin", "backup.restore", "", strings.Join(restored,","), getClientIP(r))
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "restored": restored})
}

// Helpers
func minLen(s string, n int) int { if len(s) < n { return len(s) }; return n }
func loadConfigFromFile() interface{} {
	data, err := os.ReadFile("server_config.json")
	if err != nil { return nil }
	var c interface{}
	json.Unmarshal(data, &c)
	return c
}
//go:embed tocalls.json
var embeddedTocallsJSON []byte


