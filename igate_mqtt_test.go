package main

import "testing"

func TestOldMQTTConnectionCannotMarkReplacementOffline(t *testing.T) {
	key := "member|2E0LXY-10"
	oldConn := &mqttConn{}
	newConn := &mqttConn{}
	igatesMu.Lock()
	igates[key] = &IGateDevice{DeviceCall: "2E0LXY-10", Online: true, conn: newConn}
	igatesMu.Unlock()
	defer func() {
		igatesMu.Lock()
		delete(igates, key)
		igatesMu.Unlock()
	}()

	if markIGateOffline(key, oldConn) {
		t.Fatal("stale connection changed device state")
	}
	igatesMu.RLock()
	device := igates[key]
	online, owner := device.Online, device.conn
	igatesMu.RUnlock()
	if !online || owner != newConn {
		t.Fatalf("replacement connection lost: online=%v owner=%p", online, owner)
	}
}

func TestCurrentMQTTConnectionMarksDeviceOffline(t *testing.T) {
	key := "member|2E0LXY-9"
	conn := &mqttConn{}
	igatesMu.Lock()
	igates[key] = &IGateDevice{DeviceCall: "2E0LXY-9", Online: true, conn: conn}
	igatesMu.Unlock()
	defer func() {
		igatesMu.Lock()
		delete(igates, key)
		igatesMu.Unlock()
	}()

	if !markIGateOffline(key, conn) {
		t.Fatal("current connection did not change device state")
	}
	igatesMu.RLock()
	device := igates[key]
	online, owner := device.Online, device.conn
	igatesMu.RUnlock()
	if online || owner != nil {
		t.Fatalf("device still online: online=%v owner=%p", online, owner)
	}
}
