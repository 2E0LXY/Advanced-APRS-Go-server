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
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── iGate Device State ─────────────────────────────────────────────────────

type IGateDevice struct {
	DeviceCall string  `json:"call"`
	OwnerCall  string  `json:"owner"`
	MemberID   string  `json:"-"`
	Online     bool    `json:"online"`
	LastSeen   int64   `json:"last_seen"`
	Uptime     int64   `json:"uptime_sec"`
	HeapFree   int     `json:"heap_free"`
	WifiRSSI   int     `json:"wifi_rssi"`
	PacketsRx  int     `json:"packets_rx"`
	PacketsTx  int     `json:"packets_tx"`
	Battery    float64 `json:"battery_v"`
	FW         string  `json:"fw"`
	APRSIP     string  `json:"aprs_server"`
	conn       *mqttConn
}

var (
	igatesMu sync.RWMutex
	igates   = make(map[string]*IGateDevice) // key: memberID+"|"+deviceCall
)

func igateKey(memberID, deviceCall string) string {
	return memberID + "|" + strings.ToUpper(deviceCall)
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
)

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

	if list == nil {
		list = []IGateDevice{}
	}
	data, _ := json.Marshal(wsMessage{Type: "igate_status", Data: list})

	clientsMu.RLock()
	defer clientsMu.RUnlock()
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
	protoLevel := pkt[pos]; pos++
	if protoLevel != 4 {
		mc.send(mqttConnack(1)) // unacceptable protocol level
		return
	}
	flags := pkt[pos]; pos++
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

	memberID, ownerCall := mqttAuthMember(username, password)
	if memberID == "" {
		mc.send(mqttConnack(4)) // bad credentials
		log.Printf("MQTT iGate: auth failed for '%s'", username)
		return
	}

	deviceCall := strings.ToUpper(strings.TrimSpace(clientID))
	if deviceCall == "" {
		deviceCall = strings.ToUpper(ownerCall)
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
		igatesMu.Lock()
		if d, ok := igates[key]; ok {
			d.Online = false
			d.conn = nil
		}
		igatesMu.Unlock()
		mqttConnsMu.Lock()
		if mqttConns[key] == mc {
			delete(mqttConns, key)
		}
		mqttConnsMu.Unlock()
		log.Printf("MQTT iGate: disconnected %s", deviceCall)
		broadcastIGateStatus(memberID)
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

	// Forward to any subscribed device on the same member account (e.g. inter-device)
	mqttConnsMu.RLock()
	for k, other := range mqttConns {
		if k == igateKey(mc.memberID, mc.deviceCall) || other.memberID != mc.memberID {
			continue
		}
		other.mu.Lock()
		for sub := range other.subs {
			if mqttTopicMatch(sub, topic) {
				other.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				other.conn.Write(mqttBuildPublish(topic, payload))
				break
			}
		}
		other.mu.Unlock()
	}
	mqttConnsMu.RUnlock()

	if qos == 1 {
		mc.send(mqttPuback(pktID))
	}
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
	if v, ok := tel["uptime"].(float64);    ok { d.Uptime   = int64(v) }
	if v, ok := tel["heap"].(float64);      ok { d.HeapFree = int(v) }
	if v, ok := tel["wifi_rssi"].(float64); ok { d.WifiRSSI = int(v) }
	if v, ok := tel["rx"].(float64);        ok { d.PacketsRx = int(v) }
	if v, ok := tel["tx"].(float64);        ok { d.PacketsTx = int(v) }
	if v, ok := tel["batt"].(float64);      ok { d.Battery   = v }
	if v, ok := tel["fw"].(string);         ok { d.FW        = v }
	if v, ok := tel["aprs_server"].(string); ok { d.APRSIP  = v }
	memberID := d.MemberID
	igatesMu.Unlock()

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
	m, ok := requireMemberSession(w, r)
	if !ok {
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
	m, ok := requireMemberSession(w, r)
	if !ok {
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
		go handleMQTTClient(conn)
	}
}
