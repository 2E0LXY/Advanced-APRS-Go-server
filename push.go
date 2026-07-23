package main

// push.go — Web Push (VAPID) notifications for member alerts.
//
// Implements RFC 8292 (VAPID) + RFC 8188 (aes128gcm content encoding)
// using only Go standard library — no external crypto dependencies.
//
// Flow:
//   1. Server generates VAPID key pair on first start, stores in vapid.json
//   2. Web client calls /api/push/vapid-key to get the public key
//   3. Web client registers a PushSubscription and POSTs to /api/member/push-subscribe
//   4. Server stores subscription against member (persisted in members.json)
//   5. pushToMember() is called anywhere an alert fires (geo-fence, message, SOTA/POTA, HAB)

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ── VAPID key pair ─────────────────────────────────────────────────────────

const vapidFile = "vapid.json"

type vapidKeys struct {
	PrivateKey []byte `json:"private_key"` // raw 32-byte D scalar
	PublicKey  []byte `json:"public_key"`  // uncompressed 65-byte point
}

var (
	vapidMu      sync.RWMutex
	vapidPrivate *ecdsa.PrivateKey
	vapidPublic  []byte // uncompressed P-256 point, base64url for clients
)

func initVAPID() {
	// Try to load existing keys
	if data, err := os.ReadFile(vapidFile); err == nil {
		var kp vapidKeys
		if json.Unmarshal(data, &kp) == nil && len(kp.PrivateKey) == 32 && len(kp.PublicKey) == 65 {
			priv := new(ecdsa.PrivateKey)
			priv.Curve = elliptic.P256()
			priv.D = new(big.Int).SetBytes(kp.PrivateKey)
			priv.PublicKey.X, priv.PublicKey.Y = elliptic.Unmarshal(elliptic.P256(), kp.PublicKey)
			if priv.PublicKey.X != nil {
				vapidMu.Lock()
				vapidPrivate = priv
				vapidPublic = kp.PublicKey
				vapidMu.Unlock()
				log.Println("VAPID: loaded existing key pair")
				return
			}
		}
	}

	// Generate new key pair
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Printf("VAPID: key generation failed: %v", err)
		return
	}
	pub := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	kp := vapidKeys{
		PrivateKey: priv.D.Bytes(),
		PublicKey:  pub,
	}
	data, _ := json.Marshal(kp)
	os.WriteFile(vapidFile, data, 0600)

	vapidMu.Lock()
	vapidPrivate = priv
	vapidPublic = pub
	vapidMu.Unlock()
	log.Println("VAPID: generated new key pair")
}

// vapidJWT creates a signed ES256 JWT for a push endpoint's origin.
func vapidJWT(endpoint string) (string, error) {
	vapidMu.RLock()
	priv := vapidPrivate
	vapidMu.RUnlock()
	if priv == nil {
		return "", fmt.Errorf("VAPID keys not initialised")
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	aud := u.Scheme + "://" + u.Host

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"aud":%q,"exp":%d,"sub":"mailto:admin@aprsnet.uk"}`,
		aud, time.Now().Add(12*time.Hour).Unix(),
	)))
	sigInput := header + "." + claims

	h := sha256.Sum256([]byte(sigInput))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		return "", err
	}

	// Encode r,s as a 64-byte fixed-length signature (IEEE P1363)
	rBytes := zeroPad(r.Bytes(), 32)
	sBytes := zeroPad(s.Bytes(), 32)
	sig := base64.RawURLEncoding.EncodeToString(append(rBytes, sBytes...))

	return sigInput + "." + sig, nil
}

func zeroPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b[len(b)-n:]
	}
	padded := make([]byte, n)
	copy(padded[n-len(b):], b)
	return padded
}

// ── Web Push content encryption (RFC 8188 / draft-ietf-webpush-encryption) ─

func encryptPushPayload(sub *PushSubscription, plaintext []byte) ([]byte, error) {
	// Decode subscriber's public key and auth secret
	subPub, err := base64.RawURLEncoding.DecodeString(sub.Keys.P256DH)
	if err != nil {
		return nil, fmt.Errorf("bad p256dh: %w", err)
	}
	auth, err := base64.RawURLEncoding.DecodeString(sub.Keys.Auth)
	if err != nil {
		return nil, fmt.Errorf("bad auth: %w", err)
	}

	// Generate ephemeral EC key pair
	ephPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	ephPub := elliptic.Marshal(elliptic.P256(), ephPriv.PublicKey.X, ephPriv.PublicKey.Y)

	// Parse subscriber's public key
	x, y := elliptic.Unmarshal(elliptic.P256(), subPub)
	if x == nil {
		return nil, fmt.Errorf("invalid p256dh point")
	}

	// ECDH
	sharedX, _ := elliptic.P256().ScalarMult(x, y, ephPriv.D.Bytes())
	sharedKey := zeroPad(sharedX.Bytes(), 32)

	// HKDF-SHA-256 per RFC 8291
	salt := make([]byte, 16)
	rand.Read(salt)

	// PRK = HMAC-SHA256(auth, sharedKey)
	prk := hkdfExtract(auth, sharedKey)

	// info strings per draft-ietf-httpbis-encryption-encoding
	keyInfo := concat([]byte("WebPush: info\x00"), subPub, ephPub)
	inputKeyMaterial := hkdfExpand(prk, keyInfo, 32)

	prkKey := hkdfExtract(salt, inputKeyMaterial)
	cekInfo := []byte("Content-Encoding: aes128gcm\x00")
	cek := hkdfExpand(prkKey, cekInfo, 16)
	nonceInfo := []byte("Content-Encoding: nonce\x00")
	nonce := hkdfExpand(prkKey, nonceInfo, 12)

	// AES-128-GCM encrypt with 1-byte padding delimiter (0x02)
	padded := append(plaintext, 0x02)
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, padded, nil)

	// Build aes128gcm content-encoding record (RFC 8188 §2.1)
	// Header: salt(16) + rs(4,big-endian) + idlen(1) + keyid(ephPub,65)
	var header []byte
	header = append(header, salt...)
	rsBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(rsBytes, uint32(len(ciphertext)+16+1))
	header = append(header, rsBytes...)
	header = append(header, byte(len(ephPub)))
	header = append(header, ephPub...)
	return append(header, ciphertext...), nil
}

func hkdfExtract(salt, ikm []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	var out []byte
	var t []byte
	counter := byte(1)
	for len(out) < length {
		mac := hmac.New(sha256.New, prk)
		mac.Write(t)
		mac.Write(info)
		mac.Write([]byte{counter})
		t = mac.Sum(nil)
		out = append(out, t...)
		counter++
	}
	return out[:length]
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ── Push Subscription storage ──────────────────────────────────────────────

type PushSubscriptionKeys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

type PushSubscription struct {
	Endpoint string               `json:"endpoint"`
	Keys     PushSubscriptionKeys `json:"keys"`
}

// Push subscriptions are stored in a separate file (not members.json) to
// avoid bloating the main member store with large endpoint URLs.
const pushSubsFile = "push_subscriptions.json"

type PushSubStore struct {
	// map[memberID] → slice of subscriptions (one per device)
	Subs map[string][]PushSubscription `json:"subs"`
}

var (
	pushSubsMu sync.RWMutex
	pushSubs   = PushSubStore{Subs: make(map[string][]PushSubscription)}
)

func loadPushSubs() {
	data, err := os.ReadFile(pushSubsFile)
	if err != nil {
		return
	}
	var s PushSubStore
	if json.Unmarshal(data, &s) == nil && s.Subs != nil {
		pushSubsMu.Lock()
		pushSubs = s
		pushSubsMu.Unlock()
	}
}

func savePushSubs() {
	pushSubsMu.RLock()
	data, _ := json.Marshal(pushSubs)
	pushSubsMu.RUnlock()
	os.WriteFile(pushSubsFile, data, 0600)
}

// ── Deliver a push notification to all devices of a member ────────────────

type PushPayload struct {
	Type    string      `json:"type"`
	Title   string      `json:"title"`
	Body    string      `json:"body"`
	Tag     string      `json:"tag,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

func pushToMember(memberID string, payload PushPayload) {
	vapidMu.RLock()
	pub := vapidPublic
	vapidMu.RUnlock()
	if pub == nil {
		return
	}

	pushSubsMu.RLock()
	subs := append([]PushSubscription(nil), pushSubs.Subs[memberID]...)
	pushSubsMu.RUnlock()

	if len(subs) == 0 {
		return
	}

	body, _ := json.Marshal(payload)

	var dead []string
	for _, sub := range subs {
		if err := sendWebPush(sub, body, pub); err != nil {
			log.Printf("push: send failed for %s: %v", memberID, err)
			if strings.Contains(err.Error(), "410") || strings.Contains(err.Error(), "404") {
				dead = append(dead, sub.Endpoint)
			}
		}
	}

	// Remove expired subscriptions
	if len(dead) > 0 {
		pushSubsMu.Lock()
		var kept []PushSubscription
		for _, s := range pushSubs.Subs[memberID] {
			expired := false
			for _, d := range dead {
				if s.Endpoint == d {
					expired = true
					break
				}
			}
			if !expired {
				kept = append(kept, s)
			}
		}
		pushSubs.Subs[memberID] = kept
		pushSubsMu.Unlock()
		go savePushSubs()
	}
}

func sendWebPush(sub PushSubscription, plaintext []byte, vapidPub []byte) error {
	jwt, err := vapidJWT(sub.Endpoint)
	if err != nil {
		return err
	}

	encrypted, err := encryptPushPayload(&sub, plaintext)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", sub.Endpoint, strings.NewReader(string(encrypted)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	pubB64 := base64.RawURLEncoding.EncodeToString(vapidPub)
	req.Header.Set("Authorization", fmt.Sprintf(`vapid t=%s,k=%s`, jwt, pubB64))

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("push endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// ── HTTP endpoints ─────────────────────────────────────────────────────────

// GET /api/push/vapid-key — returns the server's VAPID public key (base64url)
func handlePushVapidKey(w http.ResponseWriter, r *http.Request) {
	vapidMu.RLock()
	pub := vapidPublic
	vapidMu.RUnlock()
	if pub == nil {
		http.Error(w, "VAPID not initialised", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"public_key": base64.RawURLEncoding.EncodeToString(pub),
	})
}

// POST /api/member/push-subscribe — register a push subscription for the member
func handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	m := getMemberFromRequest(r)
	if m == nil {
		w.WriteHeader(401)
		return
	}
	var sub PushSubscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid subscription"})
		return
	}
	pushSubsMu.Lock()
	// Deduplicate by endpoint
	subs := pushSubs.Subs[m.ID]
	for _, s := range subs {
		if s.Endpoint == sub.Endpoint {
			pushSubsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
	}
	// Cap at 10 subscriptions per member (one per device)
	if len(subs) >= 10 {
		subs = subs[len(subs)-9:]
	}
	pushSubs.Subs[m.ID] = append(subs, sub)
	pushSubsMu.Unlock()
	go savePushSubs()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// DELETE /api/member/push-subscribe — remove a push subscription
func handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	m := getMemberFromRequest(r)
	if m == nil {
		w.WriteHeader(401)
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	pushSubsMu.Lock()
	var kept []PushSubscription
	for _, s := range pushSubs.Subs[m.ID] {
		if s.Endpoint != req.Endpoint {
			kept = append(kept, s)
		}
	}
	pushSubs.Subs[m.ID] = kept
	pushSubsMu.Unlock()
	go savePushSubs()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
