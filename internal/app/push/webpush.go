package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Web Push uses one RFC 8188 record. The 4096-byte record size is the
// interoperable limit described by RFC 8291 section 4.
const (
	webPushRecordSize = uint32(4096)
	webPushSaltSize   = 16
	webPushPublicSize = 65
)

var errWebPushPayloadTooLarge = errors.New("push payload exceeds the 3993-byte plaintext limit")

type webPushRequest struct {
	Endpoint        string
	Auth            string
	P256DH          string
	Payload         []byte
	Subscriber      string
	TTL             int
	Urgency         string
	VAPIDPublicKey  string
	VAPIDPrivateKey string
}

func generateVAPIDKeys() (privateKey, publicKey string, err error) {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate VAPID key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(key.Bytes()),
		base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), nil
}

func sendWebPush(ctx context.Context, client HTTPClient, in webPushRequest) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint, err := parsePushEndpoint(in.Endpoint)
	if err != nil {
		return nil, err
	}
	auth, err := decodeWebPushKey("auth", in.Auth)
	if err != nil {
		return nil, err
	}
	if len(auth) != webPushSaltSize {
		return nil, fmt.Errorf("auth key is %d bytes, want %d", len(auth), webPushSaltSize)
	}
	receiverPublic, err := decodeWebPushKey("p256dh", in.P256DH)
	if err != nil {
		return nil, err
	}
	if len(receiverPublic) != webPushPublicSize {
		return nil, fmt.Errorf("p256dh key is %d bytes, want %d", len(receiverPublic), webPushPublicSize)
	}

	senderPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral Web Push key: %w", err)
	}
	salt := make([]byte, webPushSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generate Web Push salt: %w", err)
	}
	body, err := encryptWebPush(in.Payload, auth, receiverPublic, senderPrivate, salt, webPushRecordSize)
	if err != nil {
		return nil, err
	}
	authorization, err := vapidAuthorization(endpoint, in.Subscriber, in.VAPIDPublicKey, in.VAPIDPrivateKey, time.Now(), rand.Reader)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Web Push request: %w", err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(in.TTL))
	if in.Urgency != "" {
		req.Header.Set("Urgency", in.Urgency)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

func parsePushEndpoint(raw string) (*url.URL, error) {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return nil, fmt.Errorf("parse Web Push endpoint: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("push endpoint must be an absolute https URL")
	}
	return u, nil
}

func encryptWebPush(message, authSecret, receiverPublic []byte, senderPrivate *ecdh.PrivateKey, salt []byte, recordSize uint32) ([]byte, error) {
	if len(authSecret) != webPushSaltSize {
		return nil, fmt.Errorf("auth key is %d bytes, want %d", len(authSecret), webPushSaltSize)
	}
	if len(salt) != webPushSaltSize {
		return nil, fmt.Errorf("salt is %d bytes, want %d", len(salt), webPushSaltSize)
	}
	if senderPrivate == nil {
		return nil, errors.New("ephemeral Web Push private key is nil")
	}
	receiverKey, err := ecdh.P256().NewPublicKey(receiverPublic)
	if err != nil {
		return nil, fmt.Errorf("parse p256dh key: %w", err)
	}
	shared, err := senderPrivate.ECDH(receiverKey)
	if err != nil {
		return nil, fmt.Errorf("derive Web Push shared secret: %w", err)
	}
	senderPublic := senderPrivate.PublicKey().Bytes()
	cek, nonce, err := deriveWebPushKeys(shared, authSecret, receiverPublic, senderPublic, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("create Web Push cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create Web Push GCM: %w", err)
	}

	headerLen := webPushSaltSize + 4 + 1 + len(senderPublic)
	maxPlaintext := int(recordSize) - headerLen - gcm.Overhead() - 1
	if recordSize == 0 || maxPlaintext < 0 || len(message) > maxPlaintext {
		return nil, errWebPushPayloadTooLarge
	}
	plaintext := make([]byte, int(recordSize)-headerLen-gcm.Overhead())
	copy(plaintext, message)
	plaintext[len(message)] = 0x02

	body := make([]byte, 0, recordSize)
	body = append(body, salt...)
	record := make([]byte, 4)
	binary.BigEndian.PutUint32(record, recordSize)
	body = append(body, record...)
	body = append(body, byte(len(senderPublic)))
	body = append(body, senderPublic...)
	body = gcm.Seal(body, nonce, plaintext, nil)
	return body, nil
}

func deriveWebPushKeys(shared, authSecret, receiverPublic, senderPublic, salt []byte) (cek, nonce []byte, err error) {
	info := make([]byte, 0, len("WebPush: info\x00")+len(receiverPublic)+len(senderPublic))
	info = append(info, "WebPush: info\x00"...)
	info = append(info, receiverPublic...)
	info = append(info, senderPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(info), 32)
	if err != nil {
		return nil, nil, fmt.Errorf("derive Web Push input key: %w", err)
	}
	cek, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, nil, fmt.Errorf("derive Web Push content key: %w", err)
	}
	nonce, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, nil, fmt.Errorf("derive Web Push nonce: %w", err)
	}
	return cek, nonce, nil
}

func vapidAuthorization(endpoint *url.URL, subscriber, publicEncoded, privateEncoded string, now time.Time, random io.Reader) (string, error) {
	if endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		return "", errors.New("VAPID audience requires an absolute https endpoint")
	}
	contact, err := url.ParseRequestURI(subscriber)
	if err != nil || (contact.Scheme != "https" && contact.Scheme != "mailto") {
		return "", errors.New("VAPID subscriber must be an https or mailto URI")
	}
	privateBytes, err := decodeWebPushKey("VAPID private", privateEncoded)
	if err != nil {
		return "", err
	}
	if len(privateBytes) != 32 {
		return "", fmt.Errorf("VAPID private key is %d bytes, want 32", len(privateBytes))
	}
	privateKey, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), privateBytes)
	if err != nil {
		return "", fmt.Errorf("parse VAPID private key: %w", err)
	}
	publicBytes, err := decodeWebPushKey("VAPID public", publicEncoded)
	if err != nil {
		return "", err
	}
	publicKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), publicBytes)
	if err != nil {
		return "", fmt.Errorf("parse VAPID public key: %w", err)
	}
	derived, err := privateKey.PublicKey.Bytes()
	if err != nil {
		return "", fmt.Errorf("encode derived VAPID public key: %w", err)
	}
	parsedPublic, err := publicKey.Bytes()
	if err != nil {
		return "", fmt.Errorf("encode VAPID public key: %w", err)
	}
	if !bytes.Equal(derived, parsedPublic) || !bytes.Equal(derived, publicBytes) {
		return "", errors.New("VAPID public and private keys do not match")
	}

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(struct {
		Audience string `json:"aud"`
		Expires  int64  `json:"exp"`
		Subject  string `json:"sub"`
	}{
		Audience: endpoint.Scheme + "://" + endpoint.Host,
		Expires:  now.Add(12 * time.Hour).Unix(),
		Subject:  subscriber,
	})
	if err != nil {
		return "", fmt.Errorf("marshal VAPID claims: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	signingInput := header + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(random, privateKey, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign VAPID token: %w", err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	token := signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
	return "vapid t=" + token + ", k=" + base64.RawURLEncoding.EncodeToString(publicBytes), nil
}

func decodeWebPushKey(name, encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("%s key is empty", name)
	}
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(encoded); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("decode %s key: invalid base64", name)
}
