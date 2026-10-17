package main

import "strings"

// micEDecode decodes a Mic-E position report (APRS 1.0.1 chapter 10).
//
// dest is the destination address (first element of the AX.25 path, SSID is
// ignored); info is the full information field including the leading data type
// indicator ('`' or '\”). Returns latitude/longitude in decimal degrees and the
// two-character APRS symbol (table + code, table may be an overlay character).
func micEDecode(dest, info string) (lat, lon float64, sym string, course, speedKt int, ok bool) {
	if i := strings.IndexByte(dest, '-'); i >= 0 {
		dest = dest[:i]
	}
	if len(dest) < 6 || len(info) < 9 || (info[0] != '`' && info[0] != '\'') {
		return
	}

	// Destination chars 0-5 carry the latitude digits plus flags.
	// 0-9 -> digit, A-J -> digit (custom msg bit), K/L -> space, P-Y -> digit (std msg bit), Z -> space.
	var digit [6]int
	var high [6]bool // P-Z: the "1" flag state (msg bit / North / +100 lon / West)
	for i := 0; i < 6; i++ {
		c := dest[i]
		switch {
		case c >= '0' && c <= '9':
			digit[i] = int(c - '0')
		case c >= 'A' && c <= 'J' && i < 3:
			digit[i] = int(c - 'A')
			high[i] = true // custom message bit
		case (c == 'K') && i < 3:
			digit[i], high[i] = 0, true
		case c == 'L':
			digit[i] = 0
		case c >= 'P' && c <= 'Y':
			digit[i], high[i] = int(c-'P'), true
		case c == 'Z':
			digit[i], high[i] = 0, true
		default:
			return
		}
	}

	latDeg := digit[0]*10 + digit[1]
	latMin := float64(digit[2]*10+digit[3]) + float64(digit[4]*10+digit[5])/100.0
	if latDeg > 90 || latMin >= 60 {
		return
	}
	lat = float64(latDeg) + latMin/60.0
	if !high[3] { // char 4: 0-9/L = South, P-Z = North
		lat = -lat
	}
	lonOffset := 0
	if high[4] { // char 5: P-Z = longitude offset +100
		lonOffset = 100
	}
	west := high[5] // char 6: P-Z = West

	// Information field bytes 1-3: longitude, 4-6: speed/course, 7: symbol code, 8: symbol table.
	for i := 1; i <= 8; i++ {
		if info[i] < 28 {
			return
		}
	}
	d := int(info[1]) - 28 + lonOffset
	if d >= 190 {
		d -= 190
	} else if d >= 180 {
		d -= 80
	}
	m := int(info[2]) - 28
	if m >= 60 {
		m -= 60
	}
	h := int(info[3]) - 28
	if d > 180 || m < 0 || m >= 60 || h < 0 || h > 99 {
		return
	}
	lon = float64(d) + (float64(m)+float64(h)/100.0)/60.0
	if west {
		lon = -lon
	}
	if lon > 180 {
		return
	}

	sp := (int(info[4])-28)*10 + (int(info[5])-28)/10
	dc := ((int(info[5])-28)%10)*100 + (int(info[6]) - 28)
	if sp >= 800 {
		sp -= 800
	}
	if dc >= 400 {
		dc -= 400
	}

	code, table := info[7], info[8]
	if code < 33 || code > 126 {
		return
	}
	return lat, lon, string([]byte{table, code}), dc, sp, true
}

// normalizeSymbolTable converts the compressed-position overlay encoding
// ('a'-'j' = overlay digits 0-9) to the overlay character used everywhere else.
func normalizeSymbolTable(t byte) byte {
	if t >= 'a' && t <= 'j' {
		return '0' + (t - 'a')
	}
	return t
}
