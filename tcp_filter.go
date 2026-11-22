package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// APRS-IS style receive filters for the TCP client listener (port 14580).
//
// Supported terms (prefix a term with '-' to exclude):
//
//	r/lat/lon/dist   position packets within dist km of lat,lon
//	b/call1/call2    packets from these callsigns (trailing * wildcard, '-SSID' optional)
//	p/pre1/pre2      packets from callsigns starting with a prefix
//	t/types          packet types: p position, o object, i item, m message,
//	                 s status, t telemetry, w weather
//
// Unsupported or malformed terms are reported to the client and ignored.
// A filter that ends up with no usable include term delivers nothing; it never
// widens the feed. Messages addressed to the client's own callsign are always
// delivered, as on APRS-IS.

const (
	maxFilterLen   = 512
	maxFilterTerms = 16
)

type filterTerm struct {
	kind    byte
	exclude bool
	lat     float64
	lon     float64
	distKm  float64
	args    []string
	types   string
}

type filterPlan struct {
	includes []filterTerm
	excludes []filterTerm
	// requireFilter is true for a client that sent no filter while the server
	// runs in strict mode: only its own messages are delivered.
	strictNone bool
	// denyAll is set for a non-empty filter that has no usable include term.
	denyAll bool
}

// strictDefault reports whether clients with no filter should receive only
// messages addressed to them (the APRS-IS behaviour) rather than the full
// feed. Off by default so existing deployments keep their current behaviour.
func strictDefault() bool {
	return os.Getenv("APRSNET_TCP_REQUIRE_FILTER") == "1"
}

// parseFilter parses a filter expression. It returns the plan and a list of
// human-readable problems (empty when every term was accepted).
func parseFilter(expr string) (*filterPlan, []string) {
	expr = strings.TrimSpace(expr)
	plan := &filterPlan{}
	if expr == "" {
		plan.strictNone = strictDefault()
		return plan, nil
	}
	var problems []string
	if len(expr) > maxFilterLen {
		return &filterPlan{strictNone: true}, []string{"filter too long"}
	}
	fields := strings.Fields(expr)
	if len(fields) > maxFilterTerms {
		problems = append(problems, "too many terms, extra terms ignored")
		fields = fields[:maxFilterTerms]
	}
	for _, f := range fields {
		t, err := parseTerm(f)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if t.exclude {
			plan.excludes = append(plan.excludes, t)
		} else {
			plan.includes = append(plan.includes, t)
		}
	}
	if len(plan.includes) == 0 {
		// Non-empty filter with no usable include term (all bad, or exclude
		// only): deliver nothing rather than falling back to the full feed.
		plan.denyAll = true
	}
	return plan, problems
}

func parseTerm(f string) (filterTerm, error) {
	var t filterTerm
	if strings.HasPrefix(f, "-") {
		t.exclude = true
		f = f[1:]
	}
	parts := strings.Split(f, "/")
	if len(parts[0]) != 1 {
		return t, fmt.Errorf("unsupported term")
	}
	t.kind = parts[0][0]
	switch t.kind {
	case 'r':
		if len(parts) != 4 {
			return t, fmt.Errorf("expected r/lat/lon/dist")
		}
		var err error
		if t.lat, err = strconv.ParseFloat(parts[1], 64); err != nil || t.lat < -90 || t.lat > 90 {
			return t, fmt.Errorf("bad latitude")
		}
		if t.lon, err = strconv.ParseFloat(parts[2], 64); err != nil || t.lon < -180 || t.lon > 180 {
			return t, fmt.Errorf("bad longitude")
		}
		if t.distKm, err = strconv.ParseFloat(parts[3], 64); err != nil || t.distKm < 0 || t.distKm > 20040 {
			return t, fmt.Errorf("bad distance")
		}
	case 'b', 'p':
		if len(parts) < 2 {
			return t, fmt.Errorf("needs at least one argument")
		}
		for _, a := range parts[1:] {
			a = strings.ToUpper(strings.TrimSpace(a))
			if a == "" || strings.Trim(a, "*") == "" {
				return t, fmt.Errorf("empty argument")
			}
			t.args = append(t.args, a)
		}
	case 't':
		if len(parts) != 2 || parts[1] == "" {
			return t, fmt.Errorf("expected t/types")
		}
		for _, c := range parts[1] {
			if !strings.ContainsRune("poimstw", c) {
				return t, fmt.Errorf("unsupported type %q", c)
			}
		}
		t.types = parts[1]
	default:
		return t, fmt.Errorf("unsupported term")
	}
	return t, nil
}

// matches reports whether a packet should be delivered to a client with the
// given callsign under this plan.
func (p *filterPlan) matches(packet, callsign string) bool {
	if p == nil {
		return false // no plan yet: never widen the feed
	}
	if addressedTo(packet, callsign) {
		return true
	}
	if p.strictNone || p.denyAll {
		return false
	}
	if len(p.includes) == 0 && len(p.excludes) == 0 {
		return true // no filter, default mode: full feed (unchanged behaviour)
	}
	hit := false
	for _, t := range p.includes {
		if t.match(packet) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, t := range p.excludes {
		if t.match(packet) {
			return false
		}
	}
	return true
}

func (t filterTerm) match(packet string) bool {
	gt := strings.Index(packet, ">")
	if gt <= 0 {
		return false
	}
	src := strings.ToUpper(packet[:gt])
	switch t.kind {
	case 'b':
		for _, a := range t.args {
			if callMatch(src, a) {
				return true
			}
		}
	case 'p':
		for _, a := range t.args {
			if strings.HasPrefix(src, strings.TrimSuffix(a, "*")) {
				return true
			}
		}
	case 't':
		for _, c := range packetTypes(packet) {
			if strings.ContainsRune(t.types, c) {
				return true
			}
		}
	case 'r':
		hp, ok := parsePacket(packet)
		if !ok || (hp.Lat == 0 && hp.Lon == 0) {
			return false
		}
		return haversineKm(t.lat, t.lon, hp.Lat, hp.Lon) <= t.distKm
	}
	return false
}

func callMatch(src, pattern string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(src, strings.TrimSuffix(pattern, "*"))
	}
	if src == pattern {
		return true
	}
	// A pattern without an SSID matches any SSID of that call.
	return !strings.Contains(pattern, "-") && strings.HasPrefix(src, pattern+"-")
}

// packetTypes classifies the APRS information field. A position report whose
// symbol code is '_' is also a weather report, so it returns "pw". Returns ""
// if unknown.
func packetTypes(packet string) string {
	col := strings.Index(packet, ":")
	if col < 0 || col+1 >= len(packet) {
		return ""
	}
	info := packet[col+1:]
	switch info[0] {
	case '!', '=', '/', '@':
		start := 1
		if info[0] == '/' || info[0] == '@' {
			start += 7 // timestamp
		}
		// Symbol code: uncompressed = lat(8) table(1) lon(9) code(1);
		// compressed = table(1) lat(4) lon(4) code(1).
		off := -1
		if start < len(info) {
			if info[start] >= '0' && info[start] <= '9' {
				off = start + 18
			} else {
				off = start + 9
			}
		}
		if off >= 0 && off < len(info) && info[off] == '_' {
			return "pw"
		}
		return "p"
	case '`', '\'':
		return "p"
	case ';':
		return "o"
	case ')':
		return "i"
	case ':':
		return "m"
	case '>':
		return "s"
	case 'T':
		return "t"
	case '_':
		return "w"
	}
	return ""
}

// addressedTo reports whether packet is an APRS message to callsign.
func addressedTo(packet, callsign string) bool {
	if callsign == "" {
		return false
	}
	col := strings.Index(packet, ":")
	if col < 0 || len(packet) < col+12 || packet[col+1] != ':' || packet[col+11] != ':' {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(packet[col+2:col+11]), callsign)
}
