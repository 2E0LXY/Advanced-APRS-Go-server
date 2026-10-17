package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetLastPos() {
	lastPosMu.Lock()
	lastPos = map[string]*stationTrack{}
	lastPosMu.Unlock()
}

func pkt(call string, ts int64, lat, lon float64) HistoryPacket {
	return HistoryPacket{Callsign: call, Timestamp: ts, Lat: lat, Lon: lon, Symbol: "/>"}
}

func TestLookupExactCaseInsensitive(t *testing.T) {
	resetLastPos()
	recordLastPos(pkt("2E0LXY-13", 100, 53.7, -1.5))
	name, pk := lookupStation("2e0lxy-13")
	if name != "2E0LXY-13" || len(pk) != 1 || pk[0].Lat != 53.7 {
		t.Fatalf("got %q %v", name, pk)
	}
}

func TestLookupBaseCallPicksNewestSSID(t *testing.T) {
	resetLastPos()
	recordLastPos(pkt("2E0LXY-9", 100, 1, 1))
	recordLastPos(pkt("2E0LXY-13", 200, 2, 2))
	recordLastPos(pkt("2E0LXYZ", 300, 3, 3)) // different callsign, must not match
	name, _ := lookupStation("2E0LXY")
	if name != "2E0LXY-13" {
		t.Fatalf("base call lookup returned %q", name)
	}
	if n, _ := lookupStation("2E0LXY-5"); n != "" {
		t.Fatalf("unknown SSID must not match, got %q", n)
	}
}

func TestTrailCapAndDuplicates(t *testing.T) {
	resetLastPos()
	for i := 0; i < 50; i++ {
		recordLastPos(pkt("M0TST", int64(i), float64(i), 0))
	}
	recordLastPos(pkt("M0TST", 49, 49, 0)) // digipeated duplicate
	_, pk := lookupStation("M0TST")
	if len(pk) != lastPosTrailMax || pk[len(pk)-1].Lat != 49 || pk[0].Lat != float64(50-lastPosTrailMax) {
		t.Fatalf("trail wrong: len=%d first=%v last=%v", len(pk), pk[0].Lat, pk[len(pk)-1].Lat)
	}
}

func TestPruneExpired(t *testing.T) {
	resetLastPos()
	old := time.Now().Add(-lastPosTTL - time.Hour).Unix()
	recordLastPos(pkt("OLD1", old, 1, 1))
	recordLastPos(pkt("NEW1", time.Now().Unix(), 2, 2))
	lastPosMu.Lock()
	pruneLastPosLocked(time.Now())
	lastPosMu.Unlock()
	if n, _ := lookupStation("OLD1"); n != "" {
		t.Fatal("expired station kept")
	}
	if n, _ := lookupStation("NEW1"); n == "" {
		t.Fatal("fresh station pruned")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	resetLastPos()
	recordLastPos(pkt("M0SAVE-1", time.Now().Unix(), 5, 6))
	path := t.TempDir() + "/lastpos.json"
	if err := saveLastPos(path); err != nil {
		t.Fatal(err)
	}
	resetLastPos()
	loadLastPos(path)
	if n, pk := lookupStation("M0SAVE-1"); n != "M0SAVE-1" || len(pk) != 1 || pk[0].Lon != 6 {
		t.Fatalf("round trip failed: %q %v", n, pk)
	}
}

func TestHandler(t *testing.T) {
	resetLastPos()
	recordLastPos(pkt("2E0LXY-13", time.Now().Unix(), 53.7, -1.5))
	for _, c := range []struct {
		q    string
		code int
		has  string
	}{
		{"2E0LXY-13", 200, `"call":"2E0LXY-13"`},
		{"2e0lxy-13", 200, `"packets"`},
		{"NOBODY", 404, "not heard"},
		{"", 400, "invalid"},
		{"a b;drop", 400, "invalid"},
	} {
		rec := httptest.NewRecorder()
		handleStationLookup(rec, httptest.NewRequest(http.MethodGet, "/api/station?call="+strings.ReplaceAll(c.q, " ", "%20"), nil))
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.has) {
			t.Errorf("%q: code=%d body=%s", c.q, rec.Code, rec.Body.String())
		}
	}
}
