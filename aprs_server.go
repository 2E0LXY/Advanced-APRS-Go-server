package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

var (
	config = AppConfig{
		ServerName:     "T2CUSTOM",
		SoftwareVers:   "AdvancedGoAPRS 12.0",
		Callsign:       "2E0LXY",
		Passcode:       "-1",
		UpstreamAddr:   "rotate.aprs2.net:14580",
		ServerFilter:   "m/50",
		DropPiStar:     true,
		DropDStar:      true,
		DropAPDesk:     true,
		EnableGeofence: false,
		CenterLat:      53.7,
		CenterLon:      -1.5,
		RadiusKm:       100.0,
	}

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
	reconnectChan = make(chan bool, 1)
	upgrader      = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	dupes         = make(map[string]time.Time)
	dupesMu       sync.Mutex
	history       []HistoryPacket
	historyMu     sync.RWMutex
	maxHistory    = 10000 
)

var (
	posRegex = regexp.MustCompile(`([!\/=])(\d{2})(\d{2}\.\d{2})([NS])(.)(\d{3})(\d{2}\.\d{2})([EW])(.)`)
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
	go cleanDuplicateCache()
	go maintainUpstream()
	go handleBroadcasts()
	go listenUDP()
	go keepaliveLoop()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "index.html") })
	http.HandleFunc("/ws", handleWS)
	http.HandleFunc("/api/config", basicAuth(handleConfig))
	http.HandleFunc("/api/history", handleHistory)
	http.HandleFunc("/api/status", handleStatus)

	log.Printf("Advanced APRS Gateway active on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "2e0lxy" || pass != "33wf31ug33" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func verifyPasscode(callsign, passcode string) bool {
	if passcode == "-1" { return false }
	baseCall := strings.Split(strings.ToUpper(callsign), "-")[0]
	hash := 0x73e2
	for i := 0; i < len(baseCall); i += 2 {
		hash ^= int(baseCall[i]) << 8
		if i+1 < len(baseCall) { hash ^= int(baseCall[i+1]) }
	}
	return fmt.Sprintf("%d", hash&0x7fff) == passcode
}

func injectQConstruct(packet, qType string) string {
	config.RLock()
	srv := config.ServerName
	config.RUnlock()
	colIdx := strings.Index(packet, ":")
	if colIdx == -1 || strings.Contains(packet[:colIdx], ",qA") { return packet }
	return fmt.Sprintf("%s,%s,%s%s", packet[:colIdx], qType, srv, packet[colIdx:])
}

func isDuplicate(packet string) bool {
	hashData := packet
	if colIdx := strings.Index(packet, ":"); colIdx != -1 {
		header := packet[:colIdx]
		if qIdx := strings.Index(header, ",qA"); qIdx != -1 {
			hashData = header[:qIdx] + packet[colIdx:]
		}
	}
	dupesMu.Lock()
	defer dupesMu.Unlock()
	if _, exists := dupes[hashData]; exists { return true }
	dupes[hashData] = time.Now()
	return false
}

func cleanDuplicateCache() {
	for {
		time.Sleep(1 * time.Minute)
		now := time.Now()
		dupesMu.Lock()
		for k, v := range dupes {
			if now.Sub(v) > 5*time.Minute { delete(dupes, k) }
		}
		dupesMu.Unlock()
	}
}

func maintainUpstream() {
	for {
		connectUpstream()
		time.Sleep(5 * time.Second)
	}
}

func connectUpstream() {
	config.RLock()
	addr, call, pass, filter, vers := config.UpstreamAddr, config.Callsign, config.Passcode, config.ServerFilter, config.SoftwareVers
	config.RUnlock()

	conn, err := net.Dial("tcp", addr)
	if err != nil { return }
	defer conn.Close()

	fmt.Fprintf(conn, "user %s pass %s vers %s filter %s\r\n", call, pass, vers, filter)
	done := make(chan bool)
	go func() {
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasPrefix(text, "#") { continue }
			metrics.Lock(); metrics.PktsRx++; metrics.BytesRx += uint64(len(text)); metrics.Unlock()
			if isAllowed(text) && !isDuplicate(text) { broadcast <- text }
		}
		done <- true
	}()

	for {
		select {
		case <-done: return
		case <-reconnectChan: return
		case pkt := <-upstreamOut:
			outBytes := []byte(pkt + "\r\n")
			conn.Write(outBytes)
			metrics.Lock(); metrics.PktsTx++; metrics.BytesTx += uint64(len(outBytes)); metrics.Unlock()
		}
	}
}

func isAllowed(packet string) bool {
	gtIdx := strings.Index(packet, ">")
	colIdx := strings.Index(packet, ":")
	if gtIdx == -1 || colIdx == -1 || gtIdx > colIdx { return false }
	upper := strings.ToUpper(packet)
	config.RLock()
	dPi, dD, dDesk, geo, cLat, cLon, rad := config.DropPiStar, config.DropDStar, config.DropAPDesk, config.EnableGeofence, config.CenterLat, config.CenterLon, config.RadiusKm
	config.RUnlock()

	if dPi && strings.Contains(upper, "PISTAR") { return false }
	if dD && (strings.Contains(upper, "D-STAR") || strings.Contains(upper, "APDSTR")) { return false }
	if dDesk && strings.Contains(upper, "APDESK") { return false }
	
	if geo {
		parsed, ok := parsePacket(packet)
		if ok {
			dLat, dLon := (parsed.Lat - cLat)*111.0, (parsed.Lon - cLon)*111.0*math.Cos(cLat*math.Pi/180)
			if (dLat*dLat + dLon*dLon) > (rad * rad) { return false }
		}
	}
	return true
}

func parsePacket(packet string) (HistoryPacket, bool) {
	var hp HistoryPacket
	gtIdx, colIdx := strings.Index(packet, ">"), strings.Index(packet, ":")
	if gtIdx == -1 || colIdx == -1 { return hp, false }
	hp.Callsign, hp.Path, hp.Raw, hp.Timestamp = packet[:gtIdx], packet[gtIdx+1:colIdx], packet, time.Now().Unix()
	payload := packet[colIdx+1:]
	match := posRegex.FindStringSubmatch(payload)
	if match != nil {
		lDeg, _ := strconv.ParseFloat(match[2], 64); lMin, _ := strconv.ParseFloat(match[3], 64)
		hp.Lat = lDeg + (lMin/60)
		if match[4] == "S" { hp.Lat = -hp.Lat }
		lnDeg, _ := strconv.ParseFloat(match[6], 64); lnMin, _ := strconv.ParseFloat(match[7], 64)
		hp.Lon = lnDeg + (lnMin/60)
		if match[8] == "W" { hp.Lon = -hp.Lon }
		hp.Symbol = match[5] + match[9]
		if pMatch := phgRegex.FindStringSubmatch(payload); pMatch != nil { hp.PHG = pMatch[0] }
		return hp, true
	}
	return hp, false
}

func handleBroadcasts() {
	for packet := range broadcast {
		parsed, hasCoords := parsePacket(packet)
		if hasCoords {
			historyMu.Lock()
			history = append(history, parsed)
			if len(history) > maxHistory { history = history[1:] }
			historyMu.Unlock()
		}
		msg := wsMessage{Type: "rx", Packet: packet}
		if hasCoords { msg.Data = parsed }
		data, _ := json.Marshal(msg)
		clientsMu.Lock()
		for c := range clients {
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil { c.conn.Close(); delete(clients, c) }
		}
		clientsMu.Unlock()
	}
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil { return }
	client := &wsClient{conn: conn, remoteAddr: r.RemoteAddr, connectedAt: time.Now().Unix(), lastTx: time.Now()}
	clientsMu.Lock(); clients[client] = true; clientsMu.Unlock()
	defer func() { clientsMu.Lock(); delete(clients, client); clientsMu.Unlock(); conn.Close() }()

	for {
		_, msgData, err := conn.ReadMessage()
		if err != nil { break }
		var in wsMessage
		if err := json.Unmarshal(msgData, &in); err != nil { continue }
		switch in.Type {
		case "auth":
			if verifyPasscode(in.Callsign, in.Passcode) {
				client.authenticated, client.callsign, client.software = true, strings.ToUpper(in.Callsign), in.Software
				conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth_ack","status":"success","callsign":"`+client.callsign+`"}`))
			} else { conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth_ack","status":"fail"}`)) }
		case "tx":
			if !client.authenticated || !strings.HasPrefix(strings.ToUpper(in.Packet), client.callsign+">") || time.Since(client.lastTx) < time.Second { continue }
			client.lastTx = time.Now()
			metrics.Lock(); metrics.PktsRx++; metrics.BytesRx += uint64(len(in.Packet)); metrics.Unlock()
			routed := injectQConstruct(in.Packet, "qAC")
			if isAllowed(routed) && !isDuplicate(routed) { broadcast <- routed; upstreamOut <- routed }
		}
	}
}

func listenUDP() {
	addr, _ := net.ResolveUDPAddr("udp", ":14580")
	conn, _ := net.ListenUDP("udp", addr)
	buf := make([]byte, 1024)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil { continue }
		p := strings.TrimSpace(string(buf[:n]))
		if p == "" || strings.HasPrefix(p, "#") { continue }
		metrics.Lock(); metrics.PktsRx++; metrics.BytesRx += uint64(n); metrics.Unlock()
		routed := injectQConstruct(p, "qAU")
		if isAllowed(routed) && !isDuplicate(routed) { broadcast <- routed; upstreamOut <- routed }
	}
}

func keepaliveLoop() {
	for {
		time.Sleep(20 * time.Second)
		config.RLock(); msg := fmt.Sprintf("# %s %s", config.ServerName, config.SoftwareVers); config.RUnlock()
		data, _ := json.Marshal(wsMessage{Type: "sys", Packet: msg})
		clientsMu.Lock(); for c := range clients { c.conn.WriteMessage(websocket.TextMessage, data) }; clientsMu.Unlock()
	}
}

func handleHistory(w http.ResponseWriter, r *http.Request) { historyMu.RLock(); defer historyMu.RUnlock(); json.NewEncoder(w).Encode(history) }
func handleStatus(w http.ResponseWriter, r *http.Request) {
	metrics.RLock(); uptime := time.Since(metrics.StartTime).Round(time.Second).String()
	res := map[string]interface{}{"uptime": uptime, "pkts_rx": metrics.PktsRx, "pkts_tx": metrics.PktsTx, "dropped": metrics.PktsDropped}
	metrics.RUnlock()
	active := []map[string]string{}
	clientsMu.Lock()
	for c := range clients { if c.authenticated { active = append(active, map[string]string{"call": c.callsign, "soft": c.software}) } }
	clientsMu.Unlock()
	res["clients"] = active
	json.NewEncoder(w).Encode(res)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" { config.RLock(); defer config.RUnlock(); json.NewEncoder(w).Encode(config); return }
	var n AppConfig
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil { return }
	config.Lock()
	config.ServerName, config.SoftwareVers, config.Callsign, config.Passcode, config.UpstreamAddr, config.ServerFilter = n.ServerName, n.SoftwareVers, n.Callsign, n.Passcode, n.UpstreamAddr, n.ServerFilter
	config.DropPiStar, config.DropDStar, config.DropAPDesk, config.EnableGeofence, config.CenterLat, config.CenterLon, config.RadiusKm = n.DropPiStar, n.DropDStar, n.DropAPDesk, n.EnableGeofence, n.CenterLat, n.CenterLon, n.RadiusKm
	config.Unlock()
	reconnectChan <- true
}
