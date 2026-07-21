package main

// igate_mqtt.go — Minimal MQTT 3.1.1 broker for per-member iGate management.
//
// Architecture:
//   Device (ESP32) → TCP :1883 → this broker → iGateStore → WS push to member
//   Member clicks "Restart" → REST API → broker publishes cmd to device
//
// MQTT credentials: username = member callsign, password = member password.
// Topic convention:
//   Device publishes: aprsnet/{ownerCall}/{deviceCall}/telemetry   (JSON status)
//   Device subscribes: aprsnet/{ownerCall}/{deviceCall}/cmd         (JSON commands)
//   Device publishes: aprsnet/{ownerCall}/{rxPacket}               (existing APRS relay)
//
// All data is strictly scoped to the authenticated member — no cross-member access.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── iGate Device State ─────────────────────────────────────────────────────

type IGateDevice struct {
	DeviceCall       string              `json:"call"`
	OwnerCall        string              `json:"owner"`
	MemberID         string              `json:"-"`
	Online           bool                `json:"online"`
	LastSeen         int64               `json:"last_seen"`
	Uptime           int64               `json:"uptime_sec"`
	HeapFree         int                 `json:"heap_free"`
	WifiRSSI         int                 `json:"wifi_rssi"`
	PacketsRx        int                 `json:"packets_rx"`
	PacketsTx        int                 `json:"packets_tx"`
	Battery          float64             `json:"battery_v"`
	FW               string              `json:"fw"`
	APRSIP           string              `json:"aprs_server"`
	Board            string              `json:"board,omitempty"`
	LocalIP          string              `json:"local_ip,omitempty"`
	MQTTState        int                 `json:"mqtt_state"`
	Latitude         float64             `json:"latitude,omitempty"`
	Longitude        float64             `json:"longitude,omitempty"`
	PositionSource   string              `json:"position_source,omitempty"`
	UpdateState      string              `json:"update_state,omitempty"`
	UpdateMessage    string              `json:"update_message,omitempty"`
	RegionProfile    string              `json:"region_profile,omitempty"`
	ProfileConfirmed bool                `json:"profile_confirmed"`
	CountryCode      string              `json:"country_code,omitempty"`
	HardwareBand     string              `json:"hardware_band,omitempty"`
	Timezone         string              `json:"timezone,omitempty"`
	DistanceUnit     string              `json:"distance_unit,omitempty"`
	AltitudeUnit     string              `json:"altitude_unit,omitempty"`
	SpeedUnit        string              `json:"speed_unit,omitempty"`
	TemperatureUnit  string              `json:"temperature_unit,omitempty"`
	RXFrequency      int64               `json:"rx_frequency,omitempty"`
	TXFrequency      int64               `json:"tx_frequency,omitempty"`
	SpreadingFactor  int                 `json:"spreading_factor,omitempty"`
	Bandwidth        int64               `json:"bandwidth,omitempty"`
	CodingRate       int                 `json:"coding_rate,omitempty"`
	TXPower          int                 `json:"tx_power,omitempty"`
	Heard            []IGateHeardSummary `json:"heard_24h"`
	conn             *mqttConn
}

var (
	igatesMu sync.RWMutex
	igates   = make(map[string]*IGateDevice) // key: memberID+"|"+deviceCall
)

func igateKey(memberID, deviceCall string) string {
	return memberID + "|" + strings.ToUpper(deviceCall)
}

func validMQTTCall(call string) bool {
	if len(call) < 1 || len(call) > 15 {
		return false
	}
	for _, r := range call {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func validAPRSStationCall(call string) bool {
	if len(call) < 1 || len(call) > 20 {
		return false
	}
	for _, r := range call {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '/' {
			return false
		}
	}
	return !strings.HasPrefix(call, "/") && !strings.HasSuffix(call, "/") && !strings.Contains(call, "//")
}

func boundedTelemetryString(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

func markIGateOffline(key string, closing *mqttConn) bool {
	igatesMu.Lock()
	defer igatesMu.Unlock()
	d, ok := igates[key]
	if !ok || d.conn != closing {
		return false
	}
	d.Online = false
	d.conn = nil
	return true
}

// ── MQTT Connection State ──────────────────────────────────────────────────

type mqttConn struct {
	conn       net.Conn
	memberID   string
	ownerCall  string
	deviceCall string
	subs       map[string]byte // topic → granted QoS
	mu         sync.Mutex
	alive      bool
}

func (c *mqttConn) send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.alive {
		return io.EOF
	}
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(data)
	return err
}

func (c *mqttConn) close() {
	c.mu.Lock()
	c.alive = false
	c.mu.Unlock()
	c.conn.Close()
}

// Active connections indexed for command delivery
var (
	mqttConnsMu sync.RWMutex
	mqttConns   = make(map[string]*mqttConn)

	mqttActiveConnections   int64
	mqttConnectionsAccepted int64
	mqttConnectionsRejected int64
	mqttAuthFailures        int64
	mqttPublishes           int64

	mqttIPMu     sync.Mutex
	mqttIPConns  = make(map[string]int)
	mqttAuthByIP = make(map[string]*mqttAuthWindow)
)

const maxMQTTConnsPerIP = 10

type mqttAuthWindow struct {
	start    time.Time
	attempts int
}

func mqttRemoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

func acquireMQTTIP(ip string) bool {
	mqttIPMu.Lock()
	defer mqttIPMu.Unlock()
	if mqttIPConns[ip] >= maxMQTTConnsPerIP {
		return false
	}
	mqttIPConns[ip]++
	return true
}

func releaseMQTTIP(ip string) {
	mqttIPMu.Lock()
	defer mqttIPMu.Unlock()
	if mqttIPConns[ip] <= 1 {
		delete(mqttIPConns, ip)
		return
	}
	mqttIPConns[ip]--
}

// mqttAuthAllowed caps expensive password checks to 60 attempts per source IP
// per minute. Normal iGates reconnect at most a few times per minute.
func mqttAuthAllowed(ip string) bool {
	now := time.Now()
	mqttIPMu.Lock()
	defer mqttIPMu.Unlock()
	window := mqttAuthByIP[ip]
	if window == nil || now.Sub(window.start) >= time.Minute {
		mqttAuthByIP[ip] = &mqttAuthWindow{start: now, attempts: 1}
		return true
	}
	if window.attempts >= 60 {
		return false
	}
	window.attempts++
	return true
}

// ── MQTT Packet Encoding Helpers ───────────────────────────────────────────

func mqttEncLen(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7F)
		n >>= 7
		if n > 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			break
		}
	}
	return out
}

func mqttReadLen(r io.Reader) (int, error) {
	mult, val := 1, 0
	for {
		b := make([]byte, 1)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		val += int(b[0]&0x7F) * mult
		mult *= 128
		if b[0]&0x80 == 0 {
			break
		}
		if mult > 128*128*128 {
			return 0, io.ErrUnexpectedEOF
		}
	}
	return val, nil
}

func mqttStr(data []byte, pos int) (string, int) {
	if pos+2 > len(data) {
		return "", pos
	}
	l := int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	if pos+l > len(data) {
		return "", pos
	}
	return string(data[pos : pos+l]), pos + l
}

func mqttEncStr(s string) []byte {
	b := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(b, uint16(len(s)))
	copy(b[2:], s)
	return b
}

func mqttConnack(code byte) []byte { return []byte{0x20, 2, 0, code} }
func mqttPuback(id uint16) []byte  { return []byte{0x40, 2, byte(id >> 8), byte(id)} }
func mqttSuback(id uint16, codes []byte) []byte {
	pl := append([]byte{byte(id >> 8), byte(id)}, codes...)
	hdr := append([]byte{0x90}, mqttEncLen(len(pl))...)
	return append(hdr, pl...)
}

var mqttPingresp = []byte{0xD0, 0}

func mqttBuildPublish(topic, payload string) []byte {
	body := append(mqttEncStr(topic), []byte(payload)...)
	hdr := append([]byte{0x30}, mqttEncLen(len(body))...)
	return append(hdr, body...)
}

// mqttTopicMatch supports + (single-level) and # (multi-level) wildcards
func mqttTopicMatch(filter, topic string) bool {
	fp := strings.Split(filter, "/")
	tp := strings.Split(topic, "/")
	for i, f := range fp {
		if f == "#" {
			return true
		}
		if i >= len(tp) {
			return false
		}
		if f != "+" && f != tp[i] {
			return false
		}
	}
	return len(fp) == len(tp)
}

// ── Member Auth ────────────────────────────────────────────────────────────

func mqttAuthMember(username, password string) (memberID, ownerCall string) {
	username = strings.ToUpper(strings.TrimSpace(username))
	if username == "" || password == "" {
		return "", ""
	}
	memberStoreMu.RLock()
	defer memberStoreMu.RUnlock()
	for _, m := range memberStore.Members {
		if strings.ToUpper(m.Callsign) == username {
			if hashPassword(password, m.Salt) == m.Password {
				return m.ID, m.Callsign
			}
		}
	}
	return "", ""
}

// ── iGate WS Status Broadcast ─────────────────────────────────────────────

func broadcastIGateStatus(memberID string) {
	memberStoreMu.RLock()
	mem := memberStore.Members[memberID]
	memberStoreMu.RUnlock()
	if mem == nil {
		return
	}
	ownerCall := strings.ToUpper(mem.Callsign)

	igatesMu.RLock()
	var list []IGateDevice
	for _, d := range igates {
		if d.MemberID == memberID {
			cp := *d
			cp.conn = nil
			list = append(list, cp)
		}
	}
	igatesMu.RUnlock()
	for i := range list {
		list[i].Heard = buildIGateHeardSummaries(list[i].MemberID, list[i].DeviceCall, list[i].Latitude, list[i].Longitude)
	}

	if list == nil {
		list = []IGateDevice{}
	}
	data, _ := json.Marshal(wsMessage{Type: "igate_status", Data: list})

	clientsMu.Lock()
	defer clientsMu.Unlock()
	for c := range clients {
		if c.authenticated && strings.ToUpper(c.callsign) == ownerCall {
			select {
			case c.send <- data:
			default:
			}
		}
	}
}

// ── Handle MQTT Client Connection ─────────────────────────────────────────

func handleMQTTClient(conn net.Conn) {
	defer conn.Close()
	mc := &mqttConn{conn: conn, subs: make(map[string]byte), alive: true}
	remoteIP := mqttRemoteIP(conn)

	// ── CONNECT ──
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	hdr := make([]byte, 1)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	if (hdr[0]>>4)&0x0F != 1 {
		return // must be CONNECT
	}
	remLen, err := mqttReadLen(conn)
	if err != nil || remLen > 65535 {
		return
	}
	pkt := make([]byte, remLen)
	if _, err := io.ReadFull(conn, pkt); err != nil {
		return
	}

	pos := 0
	_, pos = mqttStr(pkt, pos) // protocol name
	if pos >= len(pkt) {
		return
	}
	protoLevel := pkt[pos]
	pos++
	if protoLevel != 4 && protoLevel != 3 {
		mc.send(mqttConnack(1)) // unacceptable protocol level
		return
	}
	// Accept both MQTT 3.1 (level 3, PubSubClient default) and 3.1.1 (level 4)
	flags := pkt[pos]
	pos++
	pos += 2 // keep-alive

	clientID, pos := mqttStr(pkt, pos)
	if flags&0x04 != 0 { // will topic + message
		_, pos = mqttStr(pkt, pos)
		_, pos = mqttStr(pkt, pos)
	}
	username := ""
	if flags&0x80 != 0 {
		username, pos = mqttStr(pkt, pos)
	}
	password := ""
	if flags&0x40 != 0 {
		password, _ = mqttStr(pkt, pos)
	}

	if !mqttAuthAllowed(remoteIP) {
		atomic.AddInt64(&mqttConnectionsRejected, 1)
		mc.send(mqttConnack(3)) // server unavailable while rate limited
		return
	}
	memberID, ownerCall := mqttAuthMember(username, password)
	if memberID == "" {
		atomic.AddInt64(&mqttAuthFailures, 1)
		mc.send(mqttConnack(4)) // bad credentials
		log.Printf("MQTT iGate: auth failed for '%s' (clientID=%s)", username, clientID)
		return
	}
	log.Printf("MQTT iGate: auth OK user=%s device=%s proto=%d", username, clientID, protoLevel)

	deviceCall := strings.ToUpper(strings.TrimSpace(clientID))
	if deviceCall == "" {
		deviceCall = strings.ToUpper(ownerCall)
	}
	if !validMQTTCall(deviceCall) {
		atomic.AddInt64(&mqttConnectionsRejected, 1)
		mc.send(mqttConnack(2))
		return
	}
	mc.memberID = memberID
	mc.ownerCall = ownerCall
	mc.deviceCall = deviceCall

	if err := mc.send(mqttConnack(0)); err != nil {
		return
	}
	log.Printf("MQTT iGate: connected %s (owner %s)", deviceCall, ownerCall)

	// Register device
	key := igateKey(memberID, deviceCall)
	igatesMu.Lock()
	if igates[key] == nil {
		igates[key] = &IGateDevice{}
	}
	d := igates[key]
	d.DeviceCall = deviceCall
	d.OwnerCall = ownerCall
	d.MemberID = memberID
	d.Online = true
	d.LastSeen = time.Now().Unix()
	d.conn = mc
	igatesMu.Unlock()

	mqttConnsMu.Lock()
	mqttConns[key] = mc
	mqttConnsMu.Unlock()

	broadcastIGateStatus(memberID)

	defer func() {
		mc.close()
		// A replacement connection for the same device may already be active.
		// Only the socket that currently owns the device is allowed to mark it
		// offline; otherwise a late cleanup races with a successful reconnect.
		stateChanged := markIGateOffline(key, mc)
		mqttConnsMu.Lock()
		if mqttConns[key] == mc {
			delete(mqttConns, key)
		}
		mqttConnsMu.Unlock()
		log.Printf("MQTT iGate: disconnected %s", deviceCall)
		if stateChanged {
			broadcastIGateStatus(memberID)
		}
	}()

	// ── Packet loop ──
	conn.SetDeadline(time.Time{})
	for {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		hdr := make([]byte, 1)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		pktType := (hdr[0] >> 4) & 0x0F
		pktFlags := hdr[0] & 0x0F

		remLen, err := mqttReadLen(conn)
		if err != nil {
			return
		}
		var body []byte
		if remLen > 0 {
			if remLen > 1<<20 {
				return // sanity: max 1 MB
			}
			body = make([]byte, remLen)
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
		}

		switch pktType {
		case 3: // PUBLISH
			mc.mqttHandlePublish(pktFlags, body)

		case 8: // SUBSCRIBE
			if len(body) < 2 {
				continue
			}
			pktID := binary.BigEndian.Uint16(body[:2])
			pos := 2
			var codes []byte
			for pos < len(body) {
				topic, np := mqttStr(body, pos)
				pos = np
				qos := byte(0)
				if pos < len(body) {
					qos = body[pos] & 0x03
					pos++
				}
				mc.mu.Lock()
				mc.subs[topic] = qos
				mc.mu.Unlock()
				codes = append(codes, qos)
			}
			mc.send(mqttSuback(pktID, codes))

		case 10: // UNSUBSCRIBE
			if len(body) < 2 {
				continue
			}
			pktID := binary.BigEndian.Uint16(body[:2])
			pos := 2
			for pos < len(body) {
				topic, np := mqttStr(body, pos)
				pos = np
				mc.mu.Lock()
				delete(mc.subs, topic)
				mc.mu.Unlock()
			}
			mc.send([]byte{0xB0, 2, byte(pktID >> 8), byte(pktID)})

		case 12: // PINGREQ
			mc.send(mqttPingresp)

		case 14: // DISCONNECT
			return
		}
	}
}

func (mc *mqttConn) mqttHandlePublish(flags byte, body []byte) {
	atomic.AddInt64(&mqttPublishes, 1)
	qos := (flags >> 1) & 0x03
	topic, pos := mqttStr(body, 0)
	var pktID uint16
	if qos > 0 && pos+2 <= len(body) {
		pktID = binary.BigEndian.Uint16(body[pos:])
		pos += 2
	}
	payload := string(body[pos:])

	// Route by topic suffix
	parts := strings.Split(topic, "/")
	if len(parts) >= 4 && parts[len(parts)-1] == "telemetry" {
		mc.mqttHandleTelemetry(payload)
	}
	// Device → server message: aprsnet/{owner}/{device}/message
	// Payload: {"to":"CALLSIGN","text":"..."}  — delivered store-and-forward
	// exactly like the website's "direct" route (no RF needed).
	if len(parts) >= 4 && parts[len(parts)-1] == "message" {
		mc.mqttHandleMessage(payload)
	}

	// Forward to any subscribed device on the same member account (e.g. inter-device)
	mqttConnsMu.RLock()
	targets := make([]*mqttConn, 0, len(mqttConns))
	for k, other := range mqttConns {
		if k == igateKey(mc.memberID, mc.deviceCall) || other.memberID != mc.memberID {
			continue
		}
		targets = append(targets, other)
	}
	mqttConnsMu.RUnlock()
	for _, other := range targets {
		other.mu.Lock()
		matched := false
		for sub := range other.subs {
			if mqttTopicMatch(sub, topic) {
				matched = true
				break
			}
		}
		other.mu.Unlock()
		if matched {
			_ = other.send(mqttBuildPublish(topic, payload))
		}
	}

	if qos == 1 {
		mc.send(mqttPuback(pktID))
	}
}

func (mc *mqttConn) mqttHandleMessage(payload string) {
	var req struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		log.Printf("MQTT message: bad payload from %s: %v", mc.deviceCall, err)
		return
	}
	if req.To == "" || req.Text == "" {
		return
	}
	// Look up the owning member to use as the "from" callsign.
	// RLock is released before storeMessageForMember (which takes its own Lock).
	memberStoreMu.RLock()
	var fromCall string
	if m, ok := memberStore.Members[mc.memberID]; ok {
		fromCall = m.Callsign
	}
	memberStoreMu.RUnlock()
	if fromCall == "" {
		fromCall = mc.deviceCall
	}
	toUpper := strings.ToUpper(req.To)
	// Synthesize a raw APRS message line for the record/history.
	raw := fromCall + ">APLT00::" + fmt.Sprintf("%-9s", toUpper) + ":" + req.Text
	// Deliver store-and-forward via the same path the website uses.
	storeMessageForMember(toUpper, fromCall, req.Text, raw, time.Now().Unix())
	log.Printf("MQTT message: %s → %s via server-direct (device %s)",
		fromCall, req.To, mc.deviceCall)
}

func (mc *mqttConn) mqttHandleTelemetry(payload string) {
	var tel map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &tel); err != nil {
		return
	}
	key := igateKey(mc.memberID, mc.deviceCall)
	igatesMu.Lock()
	d, ok := igates[key]
	if !ok {
		igatesMu.Unlock()
		return
	}
	d.LastSeen = time.Now().Unix()
	if v, ok := tel["uptime"].(float64); ok {
		d.Uptime = int64(v)
	}
	if v, ok := tel["heap"].(float64); ok {
		d.HeapFree = int(v)
	}
	if v, ok := tel["wifi_rssi"].(float64); ok {
		d.WifiRSSI = int(v)
	}
	if v, ok := tel["rx"].(float64); ok {
		d.PacketsRx = int(v)
	}
	if v, ok := tel["tx"].(float64); ok {
		d.PacketsTx = int(v)
	}
	if v, ok := tel["batt"].(float64); ok {
		d.Battery = v
	}
	if v, ok := tel["fw"].(string); ok {
		d.FW = v
	}
	if v, ok := tel["aprs_server"].(string); ok {
		d.APRSIP = boundedTelemetryString(v, 128)
	}
	if v, ok := tel["board"].(string); ok {
		d.Board = boundedTelemetryString(v, 80)
	}
	if v, ok := tel["local_ip"].(string); ok {
		if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil && ip.To4() != nil && (ip.IsPrivate() || ip.IsLoopback()) {
			d.LocalIP = ip.String()
		} else {
			d.LocalIP = ""
		}
	}
	if v, ok := tel["mqtt_state"].(float64); ok {
		d.MQTTState = int(v)
	}
	if v, ok := tel["latitude"].(float64); ok {
		d.Latitude = v
	}
	if v, ok := tel["longitude"].(float64); ok {
		d.Longitude = v
	}
	if v, ok := tel["position_source"].(string); ok {
		d.PositionSource = boundedTelemetryString(v, 20)
	}
	if v, ok := tel["update_state"].(string); ok {
		d.UpdateState = boundedTelemetryString(v, 20)
	}
	if v, ok := tel["update_message"].(string); ok {
		d.UpdateMessage = boundedTelemetryString(v, 160)
	}
	if v, ok := tel["region_profile"].(string); ok {
		d.RegionProfile = boundedTelemetryString(v, 24)
	}
	if v, ok := tel["profile_confirmed"].(bool); ok {
		d.ProfileConfirmed = v
	}
	if v, ok := tel["country_code"].(string); ok {
		d.CountryCode = boundedTelemetryString(strings.ToUpper(v), 3)
	}
	if v, ok := tel["hardware_band"].(string); ok {
		d.HardwareBand = boundedTelemetryString(v, 16)
	}
	if v, ok := tel["timezone"].(string); ok {
		d.Timezone = boundedTelemetryString(v, 48)
	}
	if v, ok := tel["distance_unit"].(string); ok && (v == "mi" || v == "km") {
		d.DistanceUnit = v
	}
	if v, ok := tel["altitude_unit"].(string); ok && (v == "m" || v == "ft") {
		d.AltitudeUnit = v
	}
	if v, ok := tel["speed_unit"].(string); ok && (v == "kmh" || v == "mph") {
		d.SpeedUnit = v
	}
	if v, ok := tel["temperature_unit"].(string); ok && (v == "c" || v == "f") {
		d.TemperatureUnit = v
	}
	if v, ok := tel["rx_frequency"].(float64); ok {
		d.RXFrequency = int64(v)
	}
	if v, ok := tel["tx_frequency"].(float64); ok {
		d.TXFrequency = int64(v)
	}
	if v, ok := tel["spreading_factor"].(float64); ok {
		d.SpreadingFactor = int(v)
	}
	if v, ok := tel["bandwidth"].(float64); ok {
		d.Bandwidth = int64(v)
	}
	if v, ok := tel["coding_rate"].(float64); ok {
		d.CodingRate = int(v)
	}
	if v, ok := tel["tx_power"].(float64); ok {
		d.TXPower = int(v)
	}
	memberID := d.MemberID
	deviceCall := d.DeviceCall
	uptime := d.Uptime
	igatesMu.Unlock()

	updateIGateHeard(memberID, deviceCall, uptime, tel["heard"])
	broadcastIGateStatus(memberID)
}

// ── Send command to a specific device ─────────────────────────────────────

func sendIGateCmd(memberID, deviceCall, payload string) bool {
	key := igateKey(memberID, deviceCall)
	mqttConnsMu.RLock()
	mc := mqttConns[key]
	mqttConnsMu.RUnlock()
	if mc == nil {
		return false
	}
	igatesMu.RLock()
	d := igates[key]
	igatesMu.RUnlock()
	if d == nil {
		return false
	}
	topic := "aprsnet/" + d.OwnerCall + "/" + deviceCall + "/cmd"
	return mc.send(mqttBuildPublish(topic, payload)) == nil
}

// ── HTTP: GET /api/member/igates ───────────────────────────────────────────

func handleMemberIGates(w http.ResponseWriter, r *http.Request) {
	m := getMemberFromRequest(r)
	if m == nil {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "not authenticated"})
		return
	}
	igatesMu.RLock()
	var list []IGateDevice
	for _, d := range igates {
		if d.MemberID == m.ID {
			cp := *d
			cp.conn = nil
			list = append(list, cp)
		}
	}
	igatesMu.RUnlock()
	for i := range list {
		list[i].Heard = buildIGateHeardSummaries(list[i].MemberID, list[i].DeviceCall, list[i].Latitude, list[i].Longitude)
	}
	if list == nil {
		list = []IGateDevice{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "igates": list})
}

// ── HTTP: POST /api/member/igate/{call}/cmd ────────────────────────────────

func handleIGateCmd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	m := getMemberFromRequest(r)
	if m == nil {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "not authenticated"})
		return
	}
	// URL: /api/member/igate/{call}/cmd
	path := strings.TrimPrefix(r.URL.Path, "/api/member/igate/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing device callsign"})
		return
	}
	deviceCall := strings.ToUpper(parts[0])

	// Verify ownership
	key := igateKey(m.ID, deviceCall)
	igatesMu.RLock()
	d := igates[key]
	igatesMu.RUnlock()
	if d == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "igate not registered to this account"})
		return
	}

	var req struct {
		Cmd     string `json:"cmd"`
		Payload string `json:"payload,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmd == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "cmd required"})
		return
	}
	allowed := map[string]bool{"restart": true, "beacon": true, "telemetry": true, "aprs": true, "update": true}
	if !allowed[req.Cmd] {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "unsupported device command"})
		return
	}

	cmdJSON, _ := json.Marshal(map[string]string{"cmd": req.Cmd, "payload": req.Payload})
	sent := sendIGateCmd(m.ID, deviceCall, string(cmdJSON))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": sent,
		"error": func() string {
			if !sent {
				return "device offline"
			}
			return ""
		}()})
}

// ── Start broker + register routes ─────────────────────────────────────────

func startIGateMQTTBroker() {
	ln, err := net.Listen("tcp", ":1883")
	if err != nil {
		log.Printf("MQTT iGate broker: failed to bind :1883 — %v (iGate MQTT disabled)", err)
		return
	}
	log.Println("MQTT iGate broker: listening on :1883")
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		ip := mqttRemoteIP(conn)
		if !acquireMQTTIP(ip) {
			atomic.AddInt64(&mqttConnectionsRejected, 1)
			conn.Close()
			continue
		}
		atomic.AddInt64(&mqttConnectionsAccepted, 1)
		atomic.AddInt64(&mqttActiveConnections, 1)
		go func(c net.Conn, remoteIP string) {
			defer releaseMQTTIP(remoteIP)
			defer atomic.AddInt64(&mqttActiveConnections, -1)
			handleMQTTClient(c)
		}(conn, ip)
	}
}
