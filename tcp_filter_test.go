package main

import "testing"

const (
	pktPos   = "G1ABC-9>APRS,TCPIP*:!5330.00N/00130.00W>test"          // near 53.5N 1.5W
	pktFar   = "K1XYZ>APRS,TCPIP*:!4000.00N/07400.00W>far"             // New York
	pktMsgMe = "N0CALL>APRS,TCPIP*::2E0LXY   :hello{1"                 // message to 2E0LXY
	pktMsgOt = "N0CALL>APRS,TCPIP*::M0OTHER  :hello{2"                 // message to someone else
	pktWx    = "G1ABC-9>APRS,TCPIP*:_10090000c220s004g005t077r000p000" // weather
)

func plan(t *testing.T, expr string) *filterPlan {
	t.Helper()
	p, probs := parseFilter(expr)
	if len(probs) != 0 {
		t.Fatalf("%q: unexpected problems %v", expr, probs)
	}
	return p
}

func TestFilterBasics(t *testing.T) {
	me := "2E0LXY"
	cases := []struct {
		expr   string
		packet string
		want   bool
	}{
		{"r/53.5/-1.5/50", pktPos, true},
		{"r/53.5/-1.5/50", pktFar, false},
		{"b/G1ABC", pktPos, true},
		{"b/G1ABC", pktFar, false},
		{"p/K1", pktFar, true},
		{"p/K1", pktPos, false},
		{"t/w", pktWx, true},
		{"t/w", pktPos, false},
		{"p/G1 -t/w", pktWx, false},
		{"p/G1 -t/w", pktPos, true},
		{"r/53.5/-1.5/50 p/K1", pktFar, true}, // union of includes
		{"b/G1ABC", pktMsgMe, true},           // own messages always delivered
		{"b/G1ABC", pktMsgOt, false},
	}
	for _, c := range cases {
		if got := plan(t, c.expr).matches(c.packet, me); got != c.want {
			t.Errorf("filter %q packet %q: got %v want %v", c.expr, c.packet, got, c.want)
		}
	}
}

func TestBadFilterNeverWidens(t *testing.T) {
	for _, expr := range []string{"x/1/2", "r/abc/1/2", "r/91/0/10", "t/z", "b/", "m/50"} {
		p, probs := parseFilter(expr)
		if len(probs) == 0 {
			t.Errorf("%q: expected a problem", expr)
		}
		if p.matches(pktPos, "2E0LXY") || p.matches(pktFar, "2E0LXY") {
			t.Errorf("%q: unusable filter delivered traffic", expr)
		}
	}
	// A good term plus a bad one keeps only the good one.
	p, probs := parseFilter("b/G1ABC x/1")
	if len(probs) != 1 || !p.matches(pktPos, "2E0LXY") || p.matches(pktFar, "2E0LXY") {
		t.Errorf("mixed filter handled wrongly: probs=%v", probs)
	}
	long := make([]byte, maxFilterLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if p, probs := parseFilter(string(long)); len(probs) == 0 || p.matches(pktPos, "2E0LXY") {
		t.Error("oversized filter must be rejected and deliver nothing")
	}
}

func TestNoFilterModes(t *testing.T) {
	t.Setenv("APRSNET_TCP_REQUIRE_FILTER", "")
	p, _ := parseFilter("")
	if !p.matches(pktFar, "2E0LXY") {
		t.Error("default mode: no filter keeps full feed")
	}
	t.Setenv("APRSNET_TCP_REQUIRE_FILTER", "1")
	p, _ = parseFilter("")
	if p.matches(pktFar, "2E0LXY") || p.matches(pktMsgOt, "2E0LXY") {
		t.Error("strict mode: unrelated traffic must not be delivered")
	}
	if !p.matches(pktMsgMe, "2E0LXY") {
		t.Error("strict mode: own messages must be delivered")
	}
	var nilPlan *filterPlan
	if nilPlan.matches(pktPos, "2E0LXY") {
		t.Error("nil plan must not deliver")
	}
}
