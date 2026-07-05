package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const softwareName = "APRSNET-CLI/1.0"

var version = "dev"

type appConfig struct {
	server   string
	call     string
	passcode string
	software string
	timeout  time.Duration
	yes      bool
}

type wsFrame struct {
	Type     string          `json:"type"`
	Callsign string          `json:"callsign,omitempty"`
	Passcode string          `json:"passcode,omitempty"`
	Software string          `json:"software,omitempty"`
	Status   string          `json:"status,omitempty"`
	Packet   string          `json:"packet,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type statusResponse struct {
	Uptime            string          `json:"uptime"`
	PktsRx            int64           `json:"pkts_rx"`
	PktsTx            int64           `json:"pkts_tx"`
	Dropped           int64           `json:"dropped"`
	BytesRx           int64           `json:"bytes_rx"`
	BytesTx           int64           `json:"bytes_tx"`
	UpstreamConnected bool            `json:"upstream_connected"`
	UpstreamAddr      string          `json:"upstream_addr"`
	TCPClients        int             `json:"tcp_clients"`
	RingSize          int             `json:"ring_size"`
	Clients           json.RawMessage `json:"clients"`
}

type geoJSON struct {
	Features []struct {
		Geometry struct {
			Coordinates []float64 `json:"coordinates"`
		} `json:"geometry"`
		Properties struct {
			Callsign  string `json:"callsign"`
			Name      string `json:"name"`
			Path      string `json:"path"`
			Timestamp int64  `json:"timestamp"`
			Type      string `json:"type"`
		} `json:"properties"`
	} `json:"features"`
}

func main() {
	cfg, args := parseGlobalFlags(os.Args[1:])
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "status":
		err = printStatus(ctx, cfg)
	case "stations":
		err = printStations(ctx, cfg)
	case "monitor":
		err = monitor(ctx, cfg, args[1:])
	case "send-message":
		err = sendMessage(ctx, cfg, args[1:])
	case "beacon":
		err = sendBeacon(ctx, cfg, args[1:])
	case "object":
		err = sendObject(ctx, cfg, args[1:])
	case "raw":
		err = sendRaw(ctx, cfg, args[1:])
	case "passcode":
		err = printPasscode(args[1:])
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseGlobalFlags(args []string) (appConfig, []string) {
	cfg := appConfig{server: "https://www.aprsnet.uk", software: softwareName, timeout: 15 * time.Second}
	fs := flag.NewFlagSet("aprsnet-client", flag.ExitOnError)
	fs.StringVar(&cfg.server, "server", envOr("APRSNET_SERVER", cfg.server), "APRSNET base URL")
	fs.StringVar(&cfg.call, "call", envOr("APRSNET_CALLSIGN", ""), "APRS callsign for authenticated TX")
	fs.StringVar(&cfg.passcode, "pass", envOr("APRSNET_PASSCODE", ""), "APRS-IS passcode")
	fs.StringVar(&cfg.software, "software", softwareName, "software label sent during WebSocket auth")
	fs.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "HTTP/WebSocket timeout")
	fs.BoolVar(&cfg.yes, "yes", false, "transmit without interactive confirmation")
	_ = fs.Parse(args)
	cfg.call = strings.ToUpper(strings.TrimSpace(cfg.call))
	cfg.passcode = strings.TrimSpace(cfg.passcode)
	cfg.server = strings.TrimRight(strings.TrimSpace(cfg.server), "/")
	return cfg, fs.Args()
}

func usage() {
	fmt.Println(`APRSNET standalone client

Usage:
  aprsnet-client [global flags] status
  aprsnet-client [global flags] stations
  aprsnet-client [global flags] monitor [-watch CALL,CALL]
  aprsnet-client -call CALL -pass PASS send-message -to CALL -text TEXT [-id ID]
  aprsnet-client -call CALL -pass PASS beacon -lat LAT -lon LON [-ssid 9] [-comment TEXT]
  aprsnet-client -call CALL -pass PASS object -name NAME -lat LAT -lon LON [-comment TEXT]
  aprsnet-client -call CALL -pass PASS raw -packet PACKET
  aprsnet-client passcode -call CALL
  aprsnet-client version

Global flags:
  -server URL      APRSNET base URL, default https://www.aprsnet.uk
  -call CALL       callsign, also APRSNET_CALLSIGN
  -pass PASS       passcode, also APRSNET_PASSCODE
  -software TEXT   WebSocket software label
  -yes             skip interactive transmit confirmation
`)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func printStatus(ctx context.Context, cfg appConfig) error {
	var s statusResponse
	if err := getJSON(ctx, cfg, "/api/status", &s); err != nil {
		return err
	}
	fmt.Printf("Server: %s\n", cfg.server)
	fmt.Printf("Uptime: %s\n", s.Uptime)
	fmt.Printf("Upstream: %v (%s)\n", s.UpstreamConnected, s.UpstreamAddr)
	fmt.Printf("Packets RX/TX: %d/%d dropped=%d\n", s.PktsRx, s.PktsTx, s.Dropped)
	fmt.Printf("Bytes RX/TX: %d/%d\n", s.BytesRx, s.BytesTx)
	fmt.Printf("TCP clients: %d  WebSocket clients: %d  Ring: %d\n", s.TCPClients, clientCount(s.Clients), s.RingSize)
	return nil
}

func printStations(ctx context.Context, cfg appConfig) error {
	var geo geoJSON
	if err := getJSON(ctx, cfg, "/api/export/geojson", &geo); err != nil {
		return err
	}
	sort.Slice(geo.Features, func(i, j int) bool {
		return geo.Features[i].Properties.Timestamp > geo.Features[j].Properties.Timestamp
	})
	for _, f := range geo.Features {
		if len(f.Geometry.Coordinates) < 2 {
			continue
		}
		call := f.Properties.Callsign
		if call == "" {
			call = f.Properties.Name
		}
		fmt.Printf("%-10s %7.3f,%8.3f age=%s path=%s\n",
			call, f.Geometry.Coordinates[1], f.Geometry.Coordinates[0],
			age(f.Properties.Timestamp), f.Properties.Path)
	}
	return nil
}

func monitor(ctx context.Context, cfg appConfig, args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	watchRaw := fs.String("watch", "", "comma-separated callsigns to highlight")
	_ = fs.Parse(args)
	watch := splitWatch(*watchRaw)

	conn, err := dialWS(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	fmt.Println("Connected. Press Ctrl+C to stop.")
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var frame wsFrame
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "rx", "obj", "sys", "alert":
			prefix := time.Now().Format("15:04:05") + " " + frame.Type
			if matchesWatch(frame.Packet, watch) {
				prefix += " WATCH"
			}
			fmt.Printf("%-20s %s\n", prefix, frame.Packet)
		case "msg_history":
			fmt.Printf("%-20s %s\n", time.Now().Format("15:04:05")+" msg", strings.TrimSpace(string(frame.Data)))
		}
	}
}

func sendMessage(ctx context.Context, cfg appConfig, args []string) error {
	fs := flag.NewFlagSet("send-message", flag.ExitOnError)
	to := fs.String("to", "", "destination callsign")
	text := fs.String("text", "", "message text")
	id := fs.String("id", "01", "message id")
	_ = fs.Parse(args)
	if !validCall(*to, true) {
		return errors.New("valid -to callsign is required")
	}
	if strings.TrimSpace(*text) == "" {
		return errors.New("-text is required")
	}
	packet := fmt.Sprintf("%s>APRS,TCPIP*::%s:%s%s", sourceCall(cfg.call), padDest(*to), trimLen(*text, 67), msgIDSuffix(*id))
	return tx(ctx, cfg, packet)
}

func sendBeacon(ctx context.Context, cfg appConfig, args []string) error {
	fs := flag.NewFlagSet("beacon", flag.ExitOnError)
	lat := fs.Float64("lat", 999, "latitude")
	lon := fs.Float64("lon", 999, "longitude")
	ssid := fs.Int("ssid", -1, "source SSID 0-15")
	comment := fs.String("comment", "APRSNET CLI", "beacon comment")
	_ = fs.Parse(args)
	if *ssid < -1 || *ssid > 15 {
		return errors.New("ssid must be 0-15")
	}
	pos, err := aprsCoord(*lat, *lon)
	if err != nil {
		return err
	}
	packet := fmt.Sprintf("%s>APRS,TCPIP*:=%s>%s", beaconSource(cfg.call, *ssid), pos, trimLen(*comment, 43))
	return tx(ctx, cfg, packet)
}

func sendObject(ctx context.Context, cfg appConfig, args []string) error {
	fs := flag.NewFlagSet("object", flag.ExitOnError)
	name := fs.String("name", "", "object name, max 9 chars")
	lat := fs.Float64("lat", 999, "latitude")
	lon := fs.Float64("lon", 999, "longitude")
	comment := fs.String("comment", "", "object comment")
	_ = fs.Parse(args)
	if strings.TrimSpace(*name) == "" {
		return errors.New("-name is required")
	}
	pos, err := aprsCoord(*lat, *lon)
	if err != nil {
		return err
	}
	packet := fmt.Sprintf("%s>APRS,TCPIP*:;%s*%s%sO%s",
		sourceCall(cfg.call), padObject(*name), aprsTime(), pos, trimLen(*comment, 36))
	return tx(ctx, cfg, packet)
}

func sendRaw(ctx context.Context, cfg appConfig, args []string) error {
	fs := flag.NewFlagSet("raw", flag.ExitOnError)
	packet := fs.String("packet", "", "raw APRS packet")
	_ = fs.Parse(args)
	if strings.TrimSpace(*packet) == "" {
		return errors.New("-packet is required")
	}
	src := strings.SplitN(*packet, ">", 2)[0]
	if baseCall(src) != baseCall(cfg.call) {
		return fmt.Errorf("raw packet source %q must match authenticated callsign %q", src, baseCall(cfg.call))
	}
	return tx(ctx, cfg, *packet)
}

func printPasscode(args []string) error {
	fs := flag.NewFlagSet("passcode", flag.ExitOnError)
	call := fs.String("call", "", "callsign")
	_ = fs.Parse(args)
	if !validCall(*call, true) {
		return errors.New("valid -call is required")
	}
	fmt.Println(passcode(*call))
	return nil
}

func tx(ctx context.Context, cfg appConfig, packet string) error {
	if !validCall(cfg.call, true) {
		return errors.New("-call must be a valid callsign")
	}
	if cfg.passcode == "" {
		return errors.New("-pass is required for transmit")
	}
	fmt.Println("Preview:", packet)
	if !cfg.yes && !confirm(os.Stdin, "Transmit this packet to APRSNET/APRS-IS? [y/N] ") {
		return errors.New("cancelled")
	}

	conn, err := dialWS(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeWS(conn)

	if err := authWS(conn, cfg); err != nil {
		return err
	}
	if err := conn.WriteJSON(wsFrame{Type: "tx", Packet: packet}); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	return nil
}

func dialWS(ctx context.Context, cfg appConfig) (*websocket.Conn, error) {
	u, err := url.Parse(cfg.server)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("unsupported server scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	dialer := websocket.Dialer{HandshakeTimeout: cfg.timeout}
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	return conn, err
}

func authWS(conn *websocket.Conn, cfg appConfig) error {
	if err := conn.WriteJSON(wsFrame{
		Type: "auth", Callsign: baseCall(cfg.call), Passcode: cfg.passcode, Software: cfg.software,
	}); err != nil {
		return err
	}
	deadline := time.Now().Add(cfg.timeout)
	_ = conn.SetReadDeadline(deadline)
	defer conn.SetReadDeadline(time.Time{})
	for {
		var frame wsFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return err
		}
		if frame.Type == "auth_ack" {
			if frame.Status == "success" {
				return nil
			}
			return errors.New("websocket authentication failed")
		}
	}
}

func getJSON(ctx context.Context, cfg appConfig, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.server+path, nil)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: cfg.timeout}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func closeWS(conn *websocket.Conn) {
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	_ = conn.Close()
}

func confirm(r io.Reader, prompt string) bool {
	fmt.Print(prompt)
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}

func clientCount(raw json.RawMessage) int {
	var clients []interface{}
	if json.Unmarshal(raw, &clients) != nil {
		return 0
	}
	return len(clients)
}

func splitWatch(raw string) map[string]bool {
	watch := make(map[string]bool)
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if part = strings.ToUpper(strings.TrimSpace(part)); part != "" {
			watch[part] = true
		}
	}
	return watch
}

func matchesWatch(packet string, watch map[string]bool) bool {
	if len(watch) == 0 {
		return false
	}
	upper := strings.ToUpper(packet)
	for call := range watch {
		if strings.Contains(upper, call) {
			return true
		}
	}
	return false
}

func validCall(call string, allowSSID bool) bool {
	call = strings.ToUpper(strings.TrimSpace(call))
	parts := strings.Split(call, "-")
	base := parts[0]
	if len(base) < 3 || len(base) > 7 {
		return false
	}
	for _, r := range base {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	if len(parts) == 1 {
		return true
	}
	if !allowSSID || len(parts) != 2 {
		return false
	}
	ssid, err := strconv.Atoi(parts[1])
	return err == nil && ssid >= 0 && ssid <= 15
}

func baseCall(call string) string {
	return strings.Split(strings.ToUpper(strings.TrimSpace(call)), "-")[0]
}

func sourceWithSSID(call string, ssid int) string {
	if ssid == 0 {
		return baseCall(call)
	}
	return fmt.Sprintf("%s-%d", baseCall(call), ssid)
}

func sourceCall(call string) string {
	call = strings.ToUpper(strings.TrimSpace(call))
	if validCall(call, true) {
		return call
	}
	return baseCall(call)
}

func beaconSource(call string, ssid int) string {
	if ssid >= 0 {
		return sourceWithSSID(call, ssid)
	}
	if strings.Contains(call, "-") {
		return sourceCall(call)
	}
	return sourceWithSSID(call, 9)
}

func passcode(call string) int {
	base := baseCall(call)
	hash := 0x73e2
	for i := 0; i < len(base); i += 2 {
		hash ^= int(base[i]) << 8
		if i+1 < len(base) {
			hash ^= int(base[i+1])
		}
	}
	return hash & 0x7fff
}

func aprsCoord(lat, lon float64) (string, error) {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return "", errors.New("lat must be -90..90 and lon must be -180..180")
	}
	latAbs, lonAbs := abs(lat), abs(lon)
	latDeg, lonDeg := int(latAbs), int(lonAbs)
	latMin := floor2((latAbs - float64(latDeg)) * 60)
	lonMin := floor2((lonAbs - float64(lonDeg)) * 60)
	if latMin >= 60 {
		latMin = 59.99
	}
	if lonMin >= 60 {
		lonMin = 59.99
	}
	ns, ew := "N", "E"
	if lat < 0 {
		ns = "S"
	}
	if lon < 0 {
		ew = "W"
	}
	return fmt.Sprintf("%02d%05.2f%s/%03d%05.2f%s", latDeg, latMin, ns, lonDeg, lonMin, ew), nil
}

func floor2(v float64) float64 {
	return float64(int(v*100)) / 100
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func padDest(call string) string {
	call = trimLen(strings.ToUpper(strings.TrimSpace(call)), 9)
	return call + strings.Repeat(" ", 9-len(call))
}

func padObject(name string) string {
	name = trimLen(strings.ToUpper(strings.TrimSpace(name)), 9)
	return name + strings.Repeat(" ", 9-len(name))
}

func msgIDSuffix(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return "{" + trimLen(id, 5)
}

func trimLen(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

func aprsTime() string {
	return time.Now().UTC().Format("021504z")
}

func age(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	d := time.Since(time.Unix(ts, 0)).Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	if d < time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Hour).String()
}
