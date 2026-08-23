// Command release-sign-ed25519 creates or verifies Canary's compact detached
// release signature. Production signing reads the private key from the macOS
// login Keychain; only the matching public key is stored in the repository.
package main

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const (
	defaultKeychainService = "com.osauer.canary.release-ed25519"
	defaultKeychainAccount = "release"
	securityTool           = "/usr/bin/security"
)

func main() {
	keyPath := flag.String("key", "", "PKCS#8 Ed25519 private key (test or recovery use only)")
	service := flag.String("keychain-service", defaultKeychainService, "macOS Keychain service")
	account := flag.String("keychain-account", defaultKeychainAccount, "macOS Keychain account")
	publicPath := flag.String("public", "", "pinned PKIX Ed25519 public key")
	inputPath := flag.String("input", "", "signed file")
	outputPath := flag.String("output", "", "base64 detached signature destination")
	verifyPath := flag.String("verify", "", "verify this base64 detached signature instead of signing")
	checkKey := flag.Bool("check-key", false, "prove the private key is available and matches -public")
	initKeychain := flag.Bool("init-keychain", false, "create a new signing key in macOS Keychain")
	flag.Parse()

	if *initKeychain {
		if *keyPath != "" || *verifyPath != "" || *checkKey || *inputPath != "" || *outputPath != "" || *publicPath == "" {
			fatal(errors.New("-init-keychain requires only -public (and optional Keychain service/account)"))
		}
		if err := initializeKeychain(*service, *account, *publicPath); err != nil {
			fatal(err)
		}
		return
	}

	if *publicPath == "" {
		fatal(errors.New("-public is required"))
	}
	if *verifyPath != "" {
		if *keyPath != "" || *checkKey || *inputPath == "" || *outputPath != "" {
			fatal(errors.New("-verify requires -input and cannot be combined with -key, -check-key, or -output"))
		}
		if err := verifyFile(*publicPath, *inputPath, *verifyPath); err != nil {
			fatal(err)
		}
		return
	}
	var privateKey ed25519.PrivateKey
	var err error
	if *keyPath != "" {
		privateKey, err = readPrivateKeyFile(*keyPath)
	} else {
		privateKey, err = readKeychainPrivateKey(*service, *account)
	}
	if err != nil {
		fatal(err)
	}
	if *checkKey {
		if *inputPath != "" || *outputPath != "" {
			fatal(errors.New("-check-key cannot be combined with -input or -output"))
		}
		if err := checkPrivateKey(privateKey, *publicPath); err != nil {
			fatal(err)
		}
		return
	}
	if *inputPath == "" || *outputPath == "" {
		fatal(errors.New("-input and -output are required when signing"))
	}
	if err := signFile(privateKey, *publicPath, *inputPath, *outputPath); err != nil {
		fatal(err)
	}
}

func initializeKeychain(service, account, publicPath string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("macOS Keychain initialization is available only on Darwin")
	}
	if strings.TrimSpace(service) == "" || strings.TrimSpace(account) == "" {
		return errors.New("keychain service and account must not be empty")
	}
	if _, err := os.Lstat(publicPath); err == nil {
		return fmt.Errorf("refusing to replace existing public key: %s", publicPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect public key destination: %w", err)
	}
	if command := exec.Command(securityTool, "find-generic-password", "-w", "-s", service, "-a", account); command.Run() == nil {
		return errors.New("keychain signing key already exists; refusing implicit rotation")
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate Ed25519 signing key: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("marshal public key: %w", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := os.WriteFile(publicPath, publicPEM, 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}

	secret := base64.RawStdEncoding.EncodeToString(privateDER)
	command := exec.Command(securityTool, "add-generic-password", "-s", service, "-a", account, "-w", secret)
	if output, err := command.CombinedOutput(); err != nil {
		_ = os.Remove(publicPath)
		return fmt.Errorf("store private key in macOS Keychain: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func readKeychainPrivateKey(service, account string) (ed25519.PrivateKey, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("macOS Keychain signing is available only on Darwin; use -key for explicit recovery")
	}
	if strings.TrimSpace(service) == "" || strings.TrimSpace(account) == "" {
		return nil, errors.New("keychain service and account must not be empty")
	}
	command := exec.Command(securityTool, "find-generic-password", "-w", "-s", service, "-a", account)
	encoded, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("read release-signing key from macOS Keychain: %w", err)
	}
	return parsePrivateDERBase64(encoded)
}

func parsePrivateDERBase64(encoded []byte) (ed25519.PrivateKey, error) {
	der, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return nil, errors.New("keychain release-signing key is not valid base64 PKCS#8")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse Keychain release-signing key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("keychain release-signing key is not Ed25519")
	}
	return privateKey, nil
}

func readPrivateKeyFile(keyPath string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("inspect private key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private key must be a regular file inaccessible to group and other users")
	}
	privatePEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, rest := pem.Decode(privatePEM)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("private key must contain exactly one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not Ed25519")
	}
	return privateKey, nil
}

func signFile(privateKey ed25519.PrivateKey, publicPath, inputPath, outputPath string) error {
	if err := checkPrivateKey(privateKey, publicPath); err != nil {
		return err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	message, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	signature, err := privateKey.Sign(nil, message, crypto.Hash(0))
	if err != nil {
		return fmt.Errorf("sign input: %w", err)
	}
	if !ed25519.Verify(publicKey, message, signature) {
		return errors.New("generated signature did not verify")
	}
	if info, err := os.Lstat(outputPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to write signature through a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect signature destination: %w", err)
	}
	encoded := append([]byte(base64.RawStdEncoding.EncodeToString(signature)), '\n')
	if err := os.WriteFile(outputPath, encoded, 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	return nil
}

func checkPrivateKey(privateKey ed25519.PrivateKey, publicPath string) error {
	publicKey, err := readPublicKey(publicPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(privateKey.Public().(ed25519.PublicKey), publicKey) {
		return errors.New("private key does not match the public key pinned by this release")
	}
	return nil
}

func verifyFile(publicPath, inputPath, signaturePath string) error {
	publicKey, err := readPublicKey(publicPath)
	if err != nil {
		return err
	}
	message, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	encoded, err := os.ReadFile(signaturePath)
	if err != nil {
		return fmt.Errorf("read signature: %w", err)
	}
	signature, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, message, signature) {
		return errors.New("Ed25519 signature did not verify")
	}
	return nil
}

func readPublicKey(path string) (ed25519.PublicKey, error) {
	publicPEM, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	block, rest := pem.Decode(publicPEM)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("public key must contain exactly one PKIX PUBLIC KEY PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not Ed25519")
	}
	return publicKey, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "release-sign-ed25519:", err)
	os.Exit(1)
}
