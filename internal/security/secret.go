package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func RandomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func RandomPassword() (string, error) {
	token, err := RandomToken(18)
	if err != nil {
		return "", err
	}
	return token, nil
}

func NewAPIKey() (plain, prefix, hash string, err error) {
	token, err := RandomToken(32)
	if err != nil {
		return "", "", "", err
	}
	plain = "sk-codexone-" + token
	prefix = plain
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	hash = HashSecret(plain)
	return plain, prefix, hash, nil
}

func HashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func ValidatePassword(password string) error {
	if len(strings.TrimSpace(password)) < 12 {
		return fmt.Errorf("password must contain at least 12 characters")
	}
	if len(password) > 256 {
		return fmt.Errorf("password is too long")
	}
	return nil
}
