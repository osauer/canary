package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestSignAndVerifyWithPinnedPrivateKey(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "private.pem")
	publicPath := filepath.Join(dir, "public.pem")
	inputPath := filepath.Join(dir, "SHA256SUMS")
	outputPath := filepath.Join(dir, "SHA256SUMS.ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	message := []byte("abc123  canary-v3.2.0-darwin-arm64.tar.gz\n")
	if err := os.WriteFile(inputPath, message, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := readPrivateKeyFile(keyPath)
	if err != nil {
		t.Fatalf("readPrivateKeyFile: %v", err)
	}
	if err := signFile(loaded, publicPath, inputPath, outputPath); err != nil {
		t.Fatalf("signFile: %v", err)
	}
	if err := verifyFile(publicPath, inputPath, outputPath); err != nil {
		t.Fatalf("verifyFile: %v", err)
	}

	encoded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawStdEncoding.DecodeString(string(encoded[:len(encoded)-1]))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(publicKey, message, signature) {
		t.Fatal("signature did not verify")
	}
	if err := os.WriteFile(inputPath, append(message, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(publicPath, inputPath, outputPath); err == nil {
		t.Fatal("verifyFile accepted tampered input")
	}
}

func TestParsePrivateDERBase64(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := parsePrivateDERBase64([]byte(base64.RawStdEncoding.EncodeToString(der) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !privateKey.Equal(loaded) {
		t.Fatal("decoded private key differs")
	}
	if _, err := parsePrivateDERBase64([]byte("not-base64")); err == nil {
		t.Fatal("accepted malformed Keychain secret")
	}
}

func TestCheckPrivateKeyRejectsDifferentPublicKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(otherPublic)
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateKey(privateKey, publicPath); err == nil {
		t.Fatal("accepted a private key for a different public key")
	}
}

func TestReadPrivateKeyFileRejectsGroupReadablePrivateKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "private.pem")
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKeyFile(keyPath); err == nil {
		t.Fatal("accepted a group-readable private key")
	}
}

func TestReadPrivateKeyFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "private.pem")
	link := filepath.Join(dir, "private-link.pem")
	if err := os.WriteFile(target, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKeyFile(link); err == nil {
		t.Fatal("accepted a symlinked private key")
	}
}
