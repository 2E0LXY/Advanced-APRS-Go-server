package main

import (
	"net/http/httptest"
	"testing"
)

func replaceWSClientsForTest(t *testing.T, testClients map[*wsClient]bool) {
	t.Helper()
	clientsMu.Lock()
	previous := clients
	clients = testClients
	clientsMu.Unlock()
	t.Cleanup(func() {
		clientsMu.Lock()
		clients = previous
		clientsMu.Unlock()
	})
}

func TestRegisterWSAuthenticationReplacesSameInstallation(t *testing.T) {
	old := &wsClient{
		authenticated: true,
		callsign:      "2E0LXY",
		software:      "APRSNetAndroid 2.0",
		clientID:      "phone-a",
	}
	current := &wsClient{}
	replaceWSClientsForTest(t, map[*wsClient]bool{old: true, current: true})

	stale := registerWSAuthentication(
		current,
		"2e0lxy",
		"APRSNetAndroid 2.0",
		"phone-a",
	)

	if len(stale) != 1 || stale[0] != old {
		t.Fatalf("expected the older socket from the same installation, got %#v", stale)
	}
	if !current.authenticated || current.callsign != "2E0LXY" || current.clientID != "phone-a" {
		t.Fatalf("current authentication was not recorded: %#v", current)
	}
}

func TestRegisterWSAuthenticationKeepsSeparateInstallations(t *testing.T) {
	otherPhone := &wsClient{
		authenticated: true,
		callsign:      "2E0LXY",
		software:      "APRSNetAndroid 2.0",
		clientID:      "phone-b",
	}
	current := &wsClient{}
	replaceWSClientsForTest(t, map[*wsClient]bool{otherPhone: true, current: true})

	if stale := registerWSAuthentication(current, "2E0LXY", "APRSNetAndroid 2.0", "phone-a"); len(stale) != 0 {
		t.Fatalf("separate installations must remain connected, got %#v", stale)
	}
}

func TestRegisterWSAuthenticationDoesNotGuessForLegacyClients(t *testing.T) {
	legacy := &wsClient{
		authenticated: true,
		callsign:      "2E0LXY",
		software:      "APRSNetAndroid 2.0",
	}
	current := &wsClient{}
	replaceWSClientsForTest(t, map[*wsClient]bool{legacy: true, current: true})

	if stale := registerWSAuthentication(current, "2E0LXY", "APRSNetAndroid 2.0", ""); len(stale) != 0 {
		t.Fatalf("legacy clients cannot be safely identified as the same device, got %#v", stale)
	}
}

func TestGetRealIPPrefersProxyHeaders(t *testing.T) {
	req := httptest.NewRequest("GET", "https://www.aprsnet.uk/ws", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.8, 127.0.0.1")
	req.Header.Set("X-Real-IP", "198.51.100.4")

	if got := getRealIP(req); got != "198.51.100.4" {
		t.Fatalf("getRealIP() = %q, want X-Real-IP", got)
	}
}

