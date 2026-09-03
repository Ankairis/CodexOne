package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ankairis/CodexOne/internal/config"
)

type Cipher struct {
	aead cipher.AEAD
}

func New(cfg config.Config) (*Cipher, error) {
	key, err := loadKey(cfg)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create token cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create token AEAD: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate token nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode encrypted token: %w", err)
	}
	if len(payload) < c.aead.NonceSize() {
		return "", fmt.Errorf("encrypted token is truncated")
	}
	nonce, ciphertext := payload[:c.aead.NonceSize()], payload[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt token: %w", err)
	}
	return string(plain), nil
}

func loadKey(cfg config.Config) ([]byte, error) {
	if cfg.MasterKey != "" {
		return decodeKey(cfg.MasterKey)
	}
	if cfg.StorageDriver != "sqlite" {
		return nil, fmt.Errorf("MASTER_KEY is required")
	}
	if raw, err := os.ReadFile(cfg.MasterKeyFile); err == nil {
		return decodeKey(strings.TrimSpace(string(raw)))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read master key: %w", err)
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.MasterKeyFile), 0o700); err != nil {
		return nil, fmt.Errorf("create master key directory: %w", err)
	}
	encoded := base64.RawStdEncoding.EncodeToString(key)
	file, err := os.OpenFile(cfg.MasterKeyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			raw, readErr := os.ReadFile(cfg.MasterKeyFile)
			if readErr != nil {
				return nil, fmt.Errorf("read concurrently-created master key: %w", readErr)
			}
			return decodeKey(strings.TrimSpace(string(raw)))
		}
		return nil, fmt.Errorf("create master key: %w", err)
	}
	defer file.Close()
	if _, err = file.WriteString(encoded + "\n"); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}
	return key, nil
}

func decodeKey(raw string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding, base64.RawURLEncoding, base64.URLEncoding} {
		if key, err := encoding.DecodeString(raw); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("MASTER_KEY must decode to exactly 32 bytes")
}
