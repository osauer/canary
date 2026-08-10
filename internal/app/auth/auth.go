// Package auth manages Canary device pairing and app-session credentials.
// Durable device grants and credential hashes belong to the app state store;
// pairing sessions, challenges, and bearer sessions are process-local,
// time-bounded values. Callers must treat all raw nonces, secrets, signatures,
// cookie values, and session tokens as sensitive untrusted input.
package auth

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/app/state"
	"maps"
	"math/big"
	"strings"
	"sync"
	"time"
)

// SessionTTL is the lifetime of an in-memory bearer session minted after
// successful device authentication.
const SessionTTL = 12 * time.Hour

// Manager coordinates process-local pairing sessions, challenges, and bearer
type Manager struct {
	store        *state.Store
	deviceWriter DeviceWriter
	pairingTTL   time.Duration
	now          func() time.Time

	mu         sync.Mutex
	pairing    map[string]PairingSession
	challenges map[string]Challenge
	sessions   map[string]Session
}

// DeviceWriter persists paired-device creation and revocation through the
// app's serialized alert-delivery controller.
type DeviceWriter interface {
	AddDevice(state.DeviceGrant) error
}

// PairingSession is a short-lived, one-use invitation to enroll a device. ID,
// Nonce, and URL are sensitive because URL embeds both credentials.
type PairingSession struct {
	ID        string    `json:"id"`
	Nonce     string    `json:"nonce"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// Challenge is a two-minute, one-use proof challenge for a previously paired
type Challenge struct {
	DeviceID  string    `json:"device_id"`
	Challenge string    `json:"challenge"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Session is a process-local authenticated device session. Token is a bearer
// secret and must not be logged or persisted; ExpiresAt is fixed at issuance.
type Session struct {
	Token     string    `json:"token"`
	DeviceID  string    `json:"device_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// CompletePairingRequest contains untrusted device enrollment proof. Nonce and
// Signature are sensitive. PublicKeyJWK and Signature prove possession of the
// device key; a client with no WebCrypto omits both and enrolls a cookie-only
// grant, whose continuity rests entirely on the HttpOnly device cookie.
type CompletePairingRequest struct {
	PairingID    string          `json:"pairing_id"`
	Nonce        string          `json:"nonce"`
	DeviceName   string          `json:"device_name"`
	PublicKeyJWK json.RawMessage `json:"public_key_jwk"`
	Signature    string          `json:"signature"`
}

// CompletePairingResult identifies the durable device grant and its initial
// process-local session. Token is a bearer secret.
type CompletePairingResult struct {
	DeviceID  string    `json:"device_id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewManager constructs a Manager backed by store. Device writes are routed
// through deviceWriter so revocation cannot race confirmed alert transport. A
// pairingTTL of zero or less uses five minutes. Both authorities must be
// non-nil before authentication methods are used.
func NewManager(store *state.Store, deviceWriter DeviceWriter, pairingTTL time.Duration) *Manager {
	if pairingTTL <= 0 {
		pairingTTL = 5 * time.Minute
	}
	return &Manager{
		store:        store,
		deviceWriter: deviceWriter,
		pairingTTL:   pairingTTL,
		now:          time.Now,
		pairing:      map[string]PairingSession{},
		challenges:   map[string]Challenge{},
		sessions:     map[string]Session{},
	}
}

// StartPairing creates a one-use pairing ID and nonce using cryptographic
func (m *Manager) StartPairing(publicURL string) (PairingSession, error) {
	id, err := randomToken(18)
	if err != nil {
		return PairingSession{}, err
	}
	nonce, err := randomToken(32)
	if err != nil {
		return PairingSession{}, err
	}
	now := m.now().UTC()
	s := PairingSession{
		ID:        id,
		Nonce:     nonce,
		CreatedAt: now,
		ExpiresAt: now.Add(m.pairingTTL),
		URL:       strings.TrimRight(publicURL, "/") + "/pair.html?pair=" + id + "&nonce=" + nonce,
	}
	m.mu.Lock()
	m.pairing[id] = s
	m.mu.Unlock()
	return s, nil
}

// CompletePairing consumes the referenced pairing session, validates its
// ID even when later validation fails, so the invitation cannot be retried.
func (m *Manager) CompletePairing(req CompletePairingRequest) (CompletePairingResult, error) {
	now := m.now().UTC()
	m.mu.Lock()
	s, ok := m.pairing[req.PairingID]
	if ok {
		delete(m.pairing, req.PairingID)
	}
	m.mu.Unlock()
	if !ok {
		return CompletePairingResult{}, errors.New("unknown pairing session")
	}
	if now.After(s.ExpiresAt) {
		return CompletePairingResult{}, errors.New("pairing session expired")
	}
	if subtle.ConstantTimeCompare([]byte(req.Nonce), []byte(s.Nonce)) != 1 {
		return CompletePairingResult{}, errors.New("pairing nonce mismatch")
	}
	// An enrolling client either registers a key and proves it, or registers
	// nothing at all. Consumed nonce possession is what gates both; the
	// signature only binds the grant to the key it just registered, so its
	// absence widens nothing an attacker holding the pairing URL could not
	// already do by generating a key of their own.
	if len(bytes.TrimSpace(req.PublicKeyJWK)) > 0 {
		if err := VerifyJWKSignature(req.PublicKeyJWK, []byte(req.Nonce), req.Signature); err != nil {
			return CompletePairingResult{}, fmt.Errorf("verify device proof: %w", err)
		}
	}
	deviceID, err := randomToken(16)
	if err != nil {
		return CompletePairingResult{}, err
	}
	grant := state.DeviceGrant{
		ID:           deviceID,
		Name:         strings.TrimSpace(req.DeviceName),
		PublicKeyJWK: string(req.PublicKeyJWK),
		CreatedAt:    now,
		LastSeenAt:   now,
	}
	if grant.Name == "" {
		grant.Name = "iPhone"
	}
	if m.deviceWriter == nil {
		return CompletePairingResult{}, errors.New("device writer unavailable")
	}
	if err := m.deviceWriter.AddDevice(grant); err != nil {
		return CompletePairingResult{}, err
	}
	sess, err := m.newSession(deviceID, now)
	if err != nil {
		return CompletePairingResult{}, err
	}
	return CompletePairingResult{DeviceID: deviceID, Token: sess.Token, ExpiresAt: sess.ExpiresAt}, nil
}

// StartChallenge creates a two-minute, one-use challenge for a device that is
// randomness and is held only in memory.
func (m *Manager) StartChallenge(deviceID string) (Challenge, error) {
	if _, ok := m.store.Device(deviceID); !ok {
		return Challenge{}, errors.New("unknown device")
	}
	token, err := randomToken(32)
	if err != nil {
		return Challenge{}, err
	}
	ch := Challenge{DeviceID: deviceID, Challenge: token, ExpiresAt: m.now().UTC().Add(2 * time.Minute)}
	m.mu.Lock()
	m.challenges[token] = ch
	m.mu.Unlock()
	return ch, nil
}

// CompleteChallenge consumes challenge and verifies the paired device against
// proof validation fails. A cookie-only grant holds no key and is rejected
func (m *Manager) CompleteChallenge(deviceID, challenge, signature string) (Session, error) {
	now := m.now().UTC()
	m.mu.Lock()
	ch, ok := m.challenges[challenge]
	if ok {
		delete(m.challenges, challenge)
	}
	m.mu.Unlock()
	if !ok || ch.DeviceID != deviceID {
		return Session{}, errors.New("unknown challenge")
	}
	if now.After(ch.ExpiresAt) {
		return Session{}, errors.New("challenge expired")
	}
	grant, ok := m.store.Device(deviceID)
	if !ok {
		return Session{}, errors.New("unknown device")
	}
	if strings.TrimSpace(grant.PublicKeyJWK) == "" {
		return Session{}, errors.New("device has no key credential")
	}
	if err := VerifyJWKSignature(json.RawMessage(grant.PublicKeyJWK), []byte(challenge), signature); err != nil {
		return Session{}, err
	}
	return m.newSession(deviceID, now)
}

// IssueDeviceCookie creates a durable continuity credential for a paired
// device, stores only its SHA-256 hash on the device grant, and returns the raw
// deviceID.secret value once. The returned value is a bearer secret and must be
// protected by the HTTP cookie layer. This method does not mint a session.
func (m *Manager) IssueDeviceCookie(deviceID string) (string, error) {
	if _, ok := m.store.Device(deviceID); !ok {
		return "", errors.New("unknown device")
	}
	secret, err := randomToken(32)
	if err != nil {
		return "", err
	}
	hash, err := hashCookieSecret(secret)
	if err != nil {
		return "", err
	}
	if err := m.store.AddDeviceCookieHash(deviceID, hash); err != nil {
		return "", err
	}
	return deviceID + "." + secret, nil
}

// AuthenticateDeviceCookie verifies a deviceID.secret continuity credential
// input and returned token are bearer secrets and must not be logged.
func (m *Manager) AuthenticateDeviceCookie(value string) (Session, error) {
	deviceID, secret, ok := strings.Cut(strings.TrimSpace(value), ".")
	if !ok || deviceID == "" || secret == "" {
		return Session{}, errors.New("malformed device cookie")
	}
	grant, found := m.store.Device(deviceID)
	if !found {
		return Session{}, errors.New("unknown device")
	}
	if len(grant.DeviceCookieHashes) == 0 {
		return Session{}, errors.New("device has no cookie credential")
	}
	matched := false
	for _, hash := range grant.DeviceCookieHashes {
		if verifyCookieSecret(secret, hash) == nil {
			matched = true
			break
		}
	}
	if !matched {
		return Session{}, errors.New("device cookie mismatch")
	}
	now := m.now().UTC()
	_ = m.store.SetDeviceSeen(deviceID, now)
	return m.newSession(deviceID, now)
}

// Authenticate validates a process-local bearer token, removes it if expired,
// and confirms that its durable device grant still exists. On success it
// returns a copy of the Session and best-effort updates device last-seen time.
func (m *Manager) Authenticate(token string) (Session, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Session{}, false
	}
	now := m.now().UTC()
	m.mu.Lock()
	s, ok := m.sessions[token]
	if ok && now.After(s.ExpiresAt) {
		delete(m.sessions, token)
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return Session{}, false
	}
	if _, ok := m.store.Device(s.DeviceID); !ok {
		return Session{}, false
	}
	_ = m.store.SetDeviceSeen(s.DeviceID, now)
	return s, true
}

// StartReaper blocks while periodically removing expired pairing sessions,
// challenges, and bearer sessions. An every value of zero or less uses one
// minute. The loop returns when ctx is cancelled.
func (m *Manager) StartReaper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reap(m.now().UTC())
		}
	}
}

func (m *Manager) reap(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.pairing, func(_ string, s PairingSession) bool { return now.After(s.ExpiresAt) })
	maps.DeleteFunc(m.challenges, func(_ string, c Challenge) bool { return now.After(c.ExpiresAt) })
	maps.DeleteFunc(m.sessions, func(_ string, s Session) bool { return now.After(s.ExpiresAt) })
}

func (m *Manager) newSession(deviceID string, now time.Time) (Session, error) {
	token, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	s := Session{Token: token, DeviceID: deviceID, CreatedAt: now, ExpiresAt: now.Add(SessionTTL)}
	m.mu.Lock()
	m.sessions[token] = s
	m.mu.Unlock()
	return s, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashCookieSecret(secret string) (string, error) {
	secret = strings.TrimSpace(secret)
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		return "", err
	}
	if len(raw) < 32 {
		return "", errors.New("device cookie secret must be at least 256 bits")
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func verifyCookieSecret(secret, wantHash string) error {
	got, err := hashCookieSecret(secret)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.TrimSpace(wantHash))) != 1 {
		return errors.New("invalid device cookie")
	}
	return nil
}

type jwkP256 struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// VerifyJWKSignature verifies a SHA-256 ECDSA signature over message using an
// EC P-256 public JWK. sigB64 must use unpadded base64url and may contain either
func VerifyJWKSignature(raw json.RawMessage, message []byte, sigB64 string) error {
	var jwk jwkP256
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return err
	}
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		return errors.New("device key must be EC P-256")
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return err
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return err
	}
	if _, err := validateP256PublicKey(xBytes, yBytes); err != nil {
		return err
	}
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	curve := elliptic.P256()
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(message)
	pub := ecdsa.PublicKey{Curve: curve, X: x, Y: y}
	if len(sig) == 64 {
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		if ecdsa.Verify(&pub, digest[:], r, s) {
			return nil
		}
	}
	if ecdsa.VerifyASN1(&pub, digest[:], sig) {
		return nil
	}
	var parsed struct {
		R, S *big.Int
	}
	if _, err := asn1.Unmarshal(sig, &parsed); err == nil && parsed.R != nil && parsed.S != nil {
		if ecdsa.Verify(&pub, digest[:], parsed.R, parsed.S) {
			return nil
		}
	}
	return errors.New("invalid signature")
}

func validateP256PublicKey(xBytes, yBytes []byte) (*ecdh.PublicKey, error) {
	x, err := p256Coordinate(xBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid P-256 x coordinate: %w", err)
	}
	y, err := p256Coordinate(yBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid P-256 y coordinate: %w", err)
	}
	encoded := make([]byte, 65)
	encoded[0] = 4
	copy(encoded[1:33], x)
	copy(encoded[33:], y)
	key, err := ecdh.P256().NewPublicKey(encoded)
	if err != nil {
		return nil, errors.New("public key is not on P-256")
	}
	return key, nil
}

func p256Coordinate(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty coordinate")
	}
	if len(raw) > 32 {
		return nil, errors.New("coordinate exceeds 32 bytes")
	}
	if len(raw) == 32 {
		return raw, nil
	}
	out := make([]byte, 32)
	copy(out[32-len(raw):], raw)
	return out, nil
}
