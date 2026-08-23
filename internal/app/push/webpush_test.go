package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func decodeRFCValue(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(strings.ReplaceAll(value, " ", ""))
	if err != nil {
		t.Fatalf("decode RFC value: %v", err)
	}
	return decoded
}

func TestDeriveWebPushKeysMatchesRFC8291(t *testing.T) {
	receiverPublic := decodeRFCValue(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	senderPublic := decodeRFCValue(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	shared := decodeRFCValue(t, "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs")
	auth := decodeRFCValue(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := decodeRFCValue(t, "DGv6ra1nlYgDCS1FRnbzlw")

	cek, nonce, err := deriveWebPushKeys(shared, auth, receiverPublic, senderPublic, salt)
	if err != nil {
		t.Fatalf("deriveWebPushKeys: %v", err)
	}
	if want := decodeRFCValue(t, "oIhVW04MRdy2XN9CiKLxTg"); !bytes.Equal(cek, want) {
		t.Fatalf("CEK = %x, want %x", cek, want)
	}
	if want := decodeRFCValue(t, "4h_95klXJ5E_qnoN"); !bytes.Equal(nonce, want) {
		t.Fatalf("nonce = %x, want %x", nonce, want)
	}
}

func TestEncryptWebPushProducesOnePaddedRFCRecord(t *testing.T) {
	receiverPrivate, err := ecdh.P256().NewPrivateKey(decodeRFCValue(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	senderPrivate, err := ecdh.P256().NewPrivateKey(decodeRFCValue(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	auth := decodeRFCValue(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := decodeRFCValue(t, "DGv6ra1nlYgDCS1FRnbzlw")
	message := []byte("When I grow up, I want to be a watermelon")

	body, err := encryptWebPush(message, auth, receiverPrivate.PublicKey().Bytes(), senderPrivate, salt, webPushRecordSize)
	if err != nil {
		t.Fatalf("encryptWebPush: %v", err)
	}
	if len(body) != int(webPushRecordSize) {
		t.Fatalf("body length = %d, want %d", len(body), webPushRecordSize)
	}
	if !bytes.Equal(body[:16], salt) || binary.BigEndian.Uint32(body[16:20]) != webPushRecordSize || body[20] != webPushPublicSize {
		t.Fatalf("RFC 8188 header = %x", body[:21])
	}
	senderPublic := body[21 : 21+webPushPublicSize]
	shared, err := receiverPrivate.ECDH(senderPrivate.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	cek, nonce, err := deriveWebPushKeys(shared, auth, receiverPrivate.PublicKey().Bytes(), senderPublic, salt)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := gcm.Open(nil, nonce, body[21+webPushPublicSize:], nil)
	if err != nil {
		t.Fatalf("decrypt record: %v", err)
	}
	if !bytes.Equal(plaintext[:len(message)], message) || plaintext[len(message)] != 0x02 {
		t.Fatalf("decrypted record does not contain message plus delimiter")
	}
	for i, b := range plaintext[len(message)+1:] {
		if b != 0 {
			t.Fatalf("padding byte %d = %x, want 00", i, b)
		}
	}
}

type capturePushClient struct {
	req  *http.Request
	body []byte
}

func (c *capturePushClient) Do(req *http.Request) (*http.Response, error) {
	c.req = req
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	c.body = body
	return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestSendWebPushRequestContractAndVAPIDSignature(t *testing.T) {
	receiver, err := ecdh.P256().GenerateKey(strings.NewReader(strings.Repeat("r", 256)))
	if err != nil {
		t.Fatal(err)
	}
	privateKey, publicKey, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	client := &capturePushClient{}
	resp, err := sendWebPush(context.Background(), client, webPushRequest{
		Endpoint:        "https://push.example.test/subscription/opaque",
		Auth:            base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef")),
		P256DH:          base64.RawURLEncoding.EncodeToString(receiver.PublicKey().Bytes()),
		Payload:         []byte(`{"title":"safe"}`),
		Subscriber:      Subscriber,
		TTL:             60,
		Urgency:         "high",
		VAPIDPublicKey:  publicKey,
		VAPIDPrivateKey: privateKey,
	})
	if err != nil {
		t.Fatalf("sendWebPush: %v", err)
	}
	resp.Body.Close()
	if client.req.Method != http.MethodPost || client.req.URL.String() != "https://push.example.test/subscription/opaque" {
		t.Fatalf("request = %s %s", client.req.Method, client.req.URL)
	}
	for name, want := range map[string]string{
		"Content-Encoding": "aes128gcm",
		"Content-Type":     "application/octet-stream",
		"TTL":              "60",
		"Urgency":          "high",
	} {
		if got := client.req.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if len(client.body) != int(webPushRecordSize) {
		t.Fatalf("encrypted body length = %d", len(client.body))
	}

	authorization := strings.TrimPrefix(client.req.Header.Get("Authorization"), "vapid t=")
	token, encodedPublic, ok := strings.Cut(authorization, ", k=")
	if !ok {
		t.Fatalf("Authorization = %q", client.req.Header.Get("Authorization"))
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Audience string `json:"aud"`
		Expires  int64  `json:"exp"`
		Subject  string `json:"sub"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Audience != "https://push.example.test" || claims.Subject != Subscriber {
		t.Fatalf("claims = %+v", claims)
	}
	if expiry := time.Unix(claims.Expires, 0); time.Until(expiry) < 11*time.Hour || time.Until(expiry) > 13*time.Hour {
		t.Fatalf("expiry = %s, want about 12h", expiry)
	}

	publicBytes, err := base64.RawURLEncoding.DecodeString(encodedPublic)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), publicBytes)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("signature length = %d, err = %v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(public, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("VAPID ES256 signature did not verify")
	}
}

func TestSendWebPushRejectsMalformedSubscriptionKeys(t *testing.T) {
	privateKey, publicKey, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	validReceiver, err := ecdh.P256().GenerateKey(strings.NewReader(strings.Repeat("k", 256)))
	if err != nil {
		t.Fatal(err)
	}
	validAuth := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	validPublic := base64.RawURLEncoding.EncodeToString(validReceiver.PublicKey().Bytes())
	for _, tc := range []struct {
		name     string
		endpoint string
		auth     string
		p256dh   string
	}{
		{name: "non-https endpoint", endpoint: "http://push.example.test/x", auth: validAuth, p256dh: validPublic},
		{name: "bad auth encoding", endpoint: "https://push.example.test/x", auth: "%", p256dh: validPublic},
		{name: "short auth", endpoint: "https://push.example.test/x", auth: base64.RawURLEncoding.EncodeToString([]byte("short")), p256dh: validPublic},
		{name: "bad public encoding", endpoint: "https://push.example.test/x", auth: validAuth, p256dh: "%"},
		{name: "invalid curve point", endpoint: "https://push.example.test/x", auth: validAuth, p256dh: base64.RawURLEncoding.EncodeToString(make([]byte, 65))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sendWebPush(context.Background(), &capturePushClient{}, webPushRequest{
				Endpoint:        tc.endpoint,
				Auth:            tc.auth,
				P256DH:          tc.p256dh,
				Payload:         []byte("safe"),
				Subscriber:      Subscriber,
				TTL:             60,
				VAPIDPublicKey:  publicKey,
				VAPIDPrivateKey: privateKey,
			})
			if err == nil {
				t.Fatal("sendWebPush succeeded with malformed input")
			}
		})
	}
}

func TestEncryptWebPushRejectsOversizePayload(t *testing.T) {
	receiver, err := ecdh.P256().GenerateKey(strings.NewReader(strings.Repeat("u", 256)))
	if err != nil {
		t.Fatal(err)
	}
	sender, err := ecdh.P256().GenerateKey(strings.NewReader(strings.Repeat("s", 256)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = encryptWebPush(make([]byte, 3994), []byte("0123456789abcdef"), receiver.PublicKey().Bytes(), sender, []byte("0123456789abcdef"), webPushRecordSize)
	if !errors.Is(err, errWebPushPayloadTooLarge) {
		t.Fatalf("error = %v, want errWebPushPayloadTooLarge", err)
	}
}
