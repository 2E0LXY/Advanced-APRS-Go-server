package main

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

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
	t.Setenv("APRSNET_TCP_LEGACY_FULL_FEED", "")
	p, _ := parseFilter("")
	if p.matches(pktFar, "2E0LXY") || p.matches(pktMsgOt, "2E0LXY") {
		t.Error("default: unrelated traffic must not be delivered to an unfiltered client")
	}
	if !p.matches(pktMsgMe, "2E0LXY") {
		t.Error("default: own messages must be delivered")
	}
	t.Setenv("APRSNET_TCP_LEGACY_FULL_FEED", "1")
	p, _ = parseFilter("")
	if !p.matches(pktFar, "2E0LXY") {
		t.Error("legacy opt-in: no filter keeps the full feed")
	}
	var nilPlan *filterPlan
	if nilPlan.matches(pktPos, "2E0LXY") {
		t.Error("nil plan must not deliver")
	}
}

// readLine reads one line or returns "" on timeout.
func readLine(c net.Conn, r *bufio.Reader, d time.Duration) string {
	c.SetReadDeadline(time.Now().Add(d))
	l, err := r.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(l)
}

// login connects a fake client through handleTCPClient and returns it after
// the logresp line (and any filter complaints) have been read.
func login(t *testing.T, line string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, s := net.Pipe()
	go handleTCPClient(s)
	r := bufio.NewReader(c)
	readLine(c, r, time.Second) // banner
	c.Write([]byte(line + "\r\n"))
	if l := readLine(c, r, time.Second); !strings.HasPrefix(l, "# logresp") {
		t.Fatalf("expected logresp, got %q", l)
	}
	time.Sleep(50 * time.Millisecond)
	return c, r
}

func TestWireUnverifiedDefaultStrictAndRuntimeFilter(t *testing.T) {
	t.Setenv("APRSNET_TCP_LEGACY_FULL_FEED", "")
	c, r := login(t, "user 2E0LXY pass -1 vers test 1")
	defer c.Close()

	broadcastToTCPClients(pktFar)
	broadcastToTCPClients(pktMsgOt)
	broadcastToTCPClients(pktMsgMe)
	if got := readLine(c, r, time.Second); got != pktMsgMe {
		t.Fatalf("unfiltered unverified client: got %q, want only its own message", got)
	}
	if got := readLine(c, r, 200*time.Millisecond); got != "" {
		t.Fatalf("unfiltered unverified client received extra packet %q", got)
	}

	c.Write([]byte("#filter p/K1\r\n"))
	if got := readLine(c, r, time.Second); got != "# filter ok" {
		t.Fatalf("runtime filter ack: got %q", got)
	}
	broadcastToTCPClients(pktPos)
	broadcastToTCPClients(pktFar)
	if got := readLine(c, r, time.Second); got != pktFar {
		t.Fatalf("after #filter p/K1: got %q", got)
	}
	if got := readLine(c, r, 200*time.Millisecond); got != "" {
		t.Fatalf("after #filter: unrelated packet delivered: %q", got)
	}

	c.Write([]byte("#filter x/1\r\n"))
	if got := readLine(c, r, time.Second); !strings.HasPrefix(got, "# filter x/1") {
		t.Fatalf("bad runtime filter should be reported, got %q", got)
	}
	broadcastToTCPClients(pktFar)
	if got := readLine(c, r, 200*time.Millisecond); got != "" {
		t.Fatalf("bad runtime filter widened the feed: %q", got)
	}
}

func TestWireLoginFilterAndLegacy(t *testing.T) {
	t.Setenv("APRSNET_TCP_LEGACY_FULL_FEED", "")
	c, r := login(t, "user 2E0LXY pass -1 vers test 1 filter b/G1ABC")
	broadcastToTCPClients(pktFar)
	broadcastToTCPClients(pktPos)
	if got := readLine(c, r, time.Second); got != pktPos {
		t.Fatalf("login filter b/G1ABC: got %q", got)
	}
	c.Close()

	t.Setenv("APRSNET_TCP_LEGACY_FULL_FEED", "1")
	c2, r2 := login(t, "user 2E0LXY pass -1 vers test 1")
	defer c2.Close()
	broadcastToTCPClients(pktFar)
	if got := readLine(c2, r2, time.Second); got != pktFar {
		t.Fatalf("legacy opt-in: unfiltered client should get full feed, got %q", got)
	}
}

func TestPositionWeatherType(t *testing.T) {
	cases := []struct {
		packet string
		types  string
	}{
		{"G1ABC>APRS:@092345z5330.00N/00130.00W_220/004g005t077", "pw"}, // timestamped, as formatWxPacket emits
		{"G1ABC>APRS:!5330.00N/00130.00W_220/004g005t077", "pw"},
		{"G1ABC>APRS:=5330.00N/00130.00W>car", "p"},
		{"G1ABC>APRS:@092345z5330.00N/00130.00W>car", "p"},
		{"G1ABC>APRS:!/5L!!<*e7_7P[", "pw"}, // compressed with weather symbol
		{"G1ABC>APRS:!/5L!!<*e7>7P[", "p"},
		{"G1ABC>APRS:!5330", "p"}, // truncated: no panic
		{pktWx, "w"},
	}
	for _, c := range cases {
		if got := packetTypes(c.packet); got != c.types {
			t.Errorf("%q: got %q want %q", c.packet, got, c.types)
		}
	}
	w := plan(t, "t/w")
	if !w.matches("G1ABC>APRS:@092345z5330.00N/00130.00W_220/004g005t077", "2E0LXY") {
		t.Error("t/w must match position-bearing weather")
	}
	x := plan(t, "p/G1 -t/w")
	if x.matches("G1ABC>APRS:@092345z5330.00N/00130.00W_220/004g005t077", "2E0LXY") {
		t.Error("-t/w must exclude position-bearing weather")
	}
	if !x.matches("G1ABC>APRS:@092345z5330.00N/00130.00W>car", "2E0LXY") {
		t.Error("-t/w must not exclude plain position")
	}
}

// Run with -race: broadcasts concurrent with logins must not race on callsign.
func TestConcurrentLoginAndBroadcast(t *testing.T) {
	t.Setenv("APRSNET_TCP_LEGACY_FULL_FEED", "")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				broadcastToTCPClients(pktMsgMe)
			}
		}
	}()
	for i := 0; i < 20; i++ {
		c, s := net.Pipe()
		go handleTCPClient(s)
		r := bufio.NewReader(c)
		readLine(c, r, time.Second)
		c.Write([]byte("user 2E0LXY pass -1 vers test 1\r\n"))
		readLine(c, r, time.Second)
		go func() { // drain so broadcast writes never block
			for readLine(c, r, 100*time.Millisecond) != "" {
			}
		}()
		time.Sleep(5 * time.Millisecond)
		c.Close()
	}
	close(stop)
	<-done
}
