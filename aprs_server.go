package main

import (
	"bufio"
	"encoding/json"
	"os"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

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
)

var (
	posRegex = regexp.MustCompile(`[!\/=@\*](\d{2})(\d{2}\.\d{2})([NS])(.)(\d{3})(\d{2}\.\d{2})([EW])(.)`)
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
}

func main() {
	loadOrInitCreds()

	go cleanDuplicateCache()
	go maintainUpstream()
	go handleBroadcasts()
	go listenUDP()
	go keepaliveLoop()

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/setup", handleSetup)
	http.HandleFunc("/ws", handleWS)
	http.HandleFunc("/api/config", basicAuth(handleConfig))
	http.HandleFunc("/api/history", handleHistory)
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/password", basicAuth(handlePassword))

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

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		adminCreds.RLock()
		wantUser, wantPass := adminCreds.Username, adminCreds.Password
		adminCreds.RUnlock()
		if !ok || user != wantUser || pass != wantPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="APRS Admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
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

	if filter == "auto" || filter == "" {
		filter = fmt.Sprintf("r/%.4f/%.4f/%.0f", cLat, cLon, rad)
	}

	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		log.Printf("Upstream connect failed (%s): %v", addr, err)
		return
	}
	defer conn.Close()

	loginLine := fmt.Sprintf("user %s pass %s vers %s filter %s\r\n", call, pass, vers, filter)
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
	if dPi && strings.Contains(upper, "PISTAR") {
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
		msg := wsMessage{Type: "rx", Packet: packet}
		if hasCoords {
			msg.Data = parsed
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
		send:        make(chan []byte, 256),
		remoteAddr:  r.RemoteAddr,
		connectedAt: time.Now().Unix(),
		lastTx:      time.Now(),
	}
	clientsMu.Lock()
	clients[client] = true
	clientsMu.Unlock()
	go client.writePump()
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

func keepaliveLoop() {
	for {
		time.Sleep(20 * time.Second)
		config.RLock()
		msg := fmt.Sprintf("# %s %s", config.ServerName, config.SoftwareVers)
		config.RUnlock()
		data, _ := json.Marshal(wsMessage{Type: "sys", Packet: msg})
		clientsMu.Lock()
		for c := range clients {
			select {
			case c.send <- data:
			default:
			}
		}
		clientsMu.Unlock()
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
	active := []map[string]string{}
	clientsMu.Lock()
	for c := range clients {
		if c.authenticated {
			active = append(active, map[string]string{"call": c.callsign, "soft": c.software})
		}
	}
	clientsMu.Unlock()
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
	config.ServerFilter = n.ServerFilter
	config.DropPiStar = n.DropPiStar
	config.DropDStar = n.DropDStar
	config.DropAPDesk = n.DropAPDesk
	config.EnableGeofence = n.EnableGeofence
	config.CenterLat = n.CenterLat
	config.CenterLon = n.CenterLon
	config.RadiusKm = n.RadiusKm
	config.Unlock()
	select {
	case <-reconnectChan:
	default:
	}
	reconnectChan <- struct{}{}
	w.WriteHeader(http.StatusOK)
}


// ─── First-run credential persistence ────────────────────────────────────────

const credsFile = "creds.json"

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
	log.Printf("Admin credentials loaded for user: %s", sc.Username)
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
	page := `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>APRS Gateway — First Run Setup</title>
<script src="https://cdn.tailwindcss.com"></script></head>
<body class="bg-gray-900 text-white min-h-screen flex items-center justify-center">
<div class="w-full max-w-md p-8 bg-gray-800 rounded-xl border border-gray-700 shadow-2xl">
  <div class="text-center mb-8">
    <div class="text-4xl mb-3">📡</div>
    <h1 class="text-2xl font-bold text-blue-400">APRS Gateway</h1>
    <p class="text-gray-400 text-sm mt-1">First-run setup — create your admin account</p>
  </div>
  ` + errHTML + `
  <form method="POST" action="/setup" class="space-y-4">
    <div>
      <label class="block text-gray-400 text-xs uppercase font-bold mb-1">Admin Username</label>
      <input type="text" name="username" required minlength="3" autocomplete="username"
        class="w-full bg-gray-700 p-3 rounded border border-gray-600 text-white outline-none focus:border-blue-500 text-sm"
        placeholder="e.g. 2e0lxy">
    </div>
    <div>
      <label class="block text-gray-400 text-xs uppercase font-bold mb-1">Password</label>
      <input type="password" name="password" required minlength="8" autocomplete="new-password"
        class="w-full bg-gray-700 p-3 rounded border border-gray-600 text-white outline-none focus:border-blue-500 text-sm"
        placeholder="Minimum 8 characters">
    </div>
    <div>
      <label class="block text-gray-400 text-xs uppercase font-bold mb-1">Confirm Password</label>
      <input type="password" name="confirm" required autocomplete="new-password"
        class="w-full bg-gray-700 p-3 rounded border border-gray-600 text-white outline-none focus:border-blue-500 text-sm"
        placeholder="Repeat password">
    </div>
    <button type="submit"
      class="w-full bg-blue-600 hover:bg-blue-500 text-white p-3 rounded font-bold uppercase tracking-widest text-sm mt-2 transition-colors">
      Create Account &amp; Launch Gateway
    </button>
  </form>
  <p class="text-gray-600 text-xs text-center mt-6">Credentials are stored in <code class="text-gray-400">creds.json</code> on the server. This page is only shown once.</p>
</div>
</body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(page))
}
