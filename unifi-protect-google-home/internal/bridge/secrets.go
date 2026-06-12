package bridge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
)

// LoadOrCreateSecret returns the user-configured secret when set; otherwise
// it loads — or generates and persists (0600) — a 32-byte random secret at
// path, so blank bridge.stream_token_secret never weakens token signing.
func LoadOrCreateSecret(configured, path string) ([]byte, error) {
	if configured != "" {
		return []byte(configured), nil
	}
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("persist %s: %w", path, err)
	}
	log.Printf("stream secret: generated and persisted to %s (bridge.stream_token_secret is blank)", path)
	return b, nil
}

// DeriveKey derives a purpose-bound signing key from the master secret via
// HMAC-SHA256 with a domain-separation label, so OAuth tokens and stream
// URLs can never validate against each other's keys.
func DeriveKey(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte(label))
	return mac.Sum(nil)
}
