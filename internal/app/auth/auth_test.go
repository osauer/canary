package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
)

func TestCompletePairingStoresGrantAndSession(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mgr := NewManager(store, store, time.Minute)
	mgr.now = func() time.Time { return now }
	key := newTestKey(t)
	pairing, err := mgr.StartPairing("https://relay.example")
	if err != nil {
		t.Fatalf("StartPairing: %v", err)
	}

	res, err := mgr.CompletePairing(CompletePairingRequest{
		PairingID:    pairing.ID,
		Nonce:        pairing.Nonce,
		DeviceName:   "iPhone",
		PublicKeyJWK: testJWK(t, key),
		Signature:    testRawSignature(t, key, pairing.Nonce),
	})
	if err != nil {
		t.Fatalf("CompletePairing: %v", err)
	}
	if res.DeviceID == "" || res.Token == "" {
		t.Fatalf("empty pairing result: %#v", res)
	}
	if _, ok := store.Device(res.DeviceID); !ok {
		t.Fatalf("device grant was not stored")
	}
	if sess, ok := mgr.Authenticate(res.Token); !ok || sess.DeviceID != res.DeviceID {
		t.Fatalf("session did not authenticate: %#v ok=%v", sess, ok)
	}
	if _, err := mgr.CompletePairing(CompletePairingRequest{PairingID: pairing.ID}); err == nil {
		t.Fatalf("reused pairing session unexpectedly succeeded")
	}
}

func TestPairingExpiryRejectsLateProof(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mgr := NewManager(store, store, time.Minute)
	mgr.now = func() time.Time { return now }
	key := newTestKey(t)
	pairing, err := mgr.StartPairing("https://relay.example")
	if err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	mgr.now = func() time.Time { return now.Add(2 * time.Minute) }
	_, err = mgr.CompletePairing(CompletePairingRequest{
		PairingID:    pairing.ID,
		Nonce:        pairing.Nonce,
		PublicKeyJWK: testJWK(t, key),
		Signature:    testRawSignature(t, key, pairing.Nonce),
	})
	if err == nil {
		t.Fatalf("expired pairing proof unexpectedly succeeded")
	}
}

func TestChallengeSessionUsesStoredDeviceKey(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	key := newTestKey(t)
	if err := store.AddDevice(state.DeviceGrant{
		ID:           "device-1",
		PublicKeyJWK: string(testJWK(t, key)),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	mgr := NewManager(store, store, time.Minute)
	challenge, err := mgr.StartChallenge("device-1")
	if err != nil {
		t.Fatalf("StartChallenge: %v", err)
	}
	sess, err := mgr.CompleteChallenge("device-1", challenge.Challenge, testDERSignature(t, key, challenge.Challenge))
	if err != nil {
		t.Fatalf("CompleteChallenge: %v", err)
	}
	if sess.Token == "" {
		t.Fatalf("empty session token")
	}
	if _, err := mgr.CompleteChallenge("device-1", challenge.Challenge, testDERSignature(t, key, challenge.Challenge)); err == nil {
		t.Fatalf("reused challenge unexpectedly succeeded")
	}
}

// A crypto-less origin enrolls with no client-held credential at all. The
// grant must store nothing, refuse the challenge path outright, and still be
// reachable through the cookie that is now its only continuity credential.
func TestCookieOnlyPairingHoldsNoClientCredential(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	mgr := NewManager(store, store, time.Minute)
	pairing, err := mgr.StartPairing("http://192.168.1.42:8765")
	if err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	res, err := mgr.CompletePairing(CompletePairingRequest{
		PairingID:  pairing.ID,
		Nonce:      pairing.Nonce,
		DeviceName: "HTTP Browser",
	})
	if err != nil {
		t.Fatalf("CompletePairing: %v", err)
	}
	grant, ok := store.Device(res.DeviceID)
	if !ok {
		t.Fatalf("device grant was not stored")
	}
	if grant.PublicKeyJWK != "" {
		t.Fatalf("cookie-only grant stored a key unexpectedly: %q", grant.PublicKeyJWK)
	}
	challenge, err := mgr.StartChallenge(res.DeviceID)
	if err != nil {
		t.Fatalf("StartChallenge: %v", err)
	}
	if _, err := mgr.CompleteChallenge(res.DeviceID, challenge.Challenge, ""); err == nil {
		t.Fatalf("keyless grant minted a session from a challenge")
	}
	cookie, err := mgr.IssueDeviceCookie(res.DeviceID)
	if err != nil {
		t.Fatalf("IssueDeviceCookie: %v", err)
	}
	sess, err := mgr.AuthenticateDeviceCookie(cookie)
	if err != nil {
		t.Fatalf("AuthenticateDeviceCookie: %v", err)
	}
	if sess.Token == "" {
		t.Fatalf("empty session token")
	}
}

// Omitting the key is how a crypto-less client enrolls, but supplying one and
// failing to prove it stays a rejection: the signature still binds the grant.
func TestPairingWithKeyStillRequiresProof(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	mgr := NewManager(store, store, time.Minute)
	pairing, err := mgr.StartPairing("https://relay.example")
	if err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	if _, err := mgr.CompletePairing(CompletePairingRequest{
		PairingID:    pairing.ID,
		Nonce:        pairing.Nonce,
		PublicKeyJWK: testJWK(t, newTestKey(t)),
		Signature:    testRawSignature(t, newTestKey(t), pairing.Nonce),
	}); err == nil {
		t.Fatalf("pairing proof signed by the wrong key unexpectedly succeeded")
	}
}

func TestReapDropsExpiredEntriesAndKeepsLiveOnes(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	mgr := NewManager(store, store, time.Minute)
	mgr.now = func() time.Time { return now }
	if err := store.AddDevice(state.DeviceGrant{ID: "device-1", CreatedAt: now}); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}

	oldPairing, err := mgr.StartPairing("https://relay.example") // expires now+1m
	if err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	oldChallenge, err := mgr.StartChallenge("device-1") // expires now+2m
	if err != nil {
		t.Fatalf("StartChallenge: %v", err)
	}
	oldSession, err := mgr.newSession("device-1", now) // expires now+12h
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}

	later := now.Add(SessionTTL + time.Minute)
	mgr.now = func() time.Time { return later }
	keepPairing, err := mgr.StartPairing("https://relay.example")
	if err != nil {
		t.Fatalf("StartPairing (live): %v", err)
	}
	keepChallenge, err := mgr.StartChallenge("device-1")
	if err != nil {
		t.Fatalf("StartChallenge (live): %v", err)
	}
	keepSession, err := mgr.newSession("device-1", later)
	if err != nil {
		t.Fatalf("newSession (live): %v", err)
	}

	mgr.reap(mgr.now().UTC())

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if _, ok := mgr.pairing[oldPairing.ID]; ok {
		t.Fatalf("expired pairing session survived reap")
	}
	if _, ok := mgr.challenges[oldChallenge.Challenge]; ok {
		t.Fatalf("expired challenge survived reap")
	}
	if _, ok := mgr.sessions[oldSession.Token]; ok {
		t.Fatalf("expired session survived reap")
	}
	if _, ok := mgr.pairing[keepPairing.ID]; !ok {
		t.Fatalf("live pairing session was reaped")
	}
	if _, ok := mgr.challenges[keepChallenge.Challenge]; !ok {
		t.Fatalf("live challenge was reaped")
	}
	if _, ok := mgr.sessions[keepSession.Token]; !ok {
		t.Fatalf("live session was reaped")
	}
}

func TestStartReaperStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	mgr := NewManager(store, store, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.StartReaper(ctx, time.Millisecond)
	}()
	time.Sleep(5 * time.Millisecond) // let it tick a few times
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("StartReaper did not stop on context cancel")
	}
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func testJWK(t *testing.T, key *ecdsa.PrivateKey) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(jwkP256{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(leftPad32(key.X)),
		Y:   base64.RawURLEncoding.EncodeToString(leftPad32(key.Y)),
	})
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	return raw
}

func testRawSignature(t *testing.T, key *ecdsa.PrivateKey, message string) string {
	t.Helper()
	r, s := signParts(t, key, message)
	raw := append(leftPad32(r), leftPad32(s)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func testDERSignature(t *testing.T, key *ecdsa.PrivateKey, message string) string {
	t.Helper()
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest(message))
	if err != nil {
		t.Fatalf("sign ASN.1: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

func signParts(t *testing.T, key *ecdsa.PrivateKey, message string) (*big.Int, *big.Int) {
	t.Helper()
	r, s, err := ecdsa.Sign(rand.Reader, key, digest(message))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return r, s
}

func digest(message string) []byte {
	sum := sha256Sum([]byte(message))
	return sum[:]
}

func sha256Sum(message []byte) [32]byte {
	return sha256.Sum256(message)
}

func leftPad32(v *big.Int) []byte {
	b := v.Bytes()
	if len(b) >= 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}
