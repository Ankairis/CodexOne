package cryptox

import (
	"path/filepath"
	"testing"

	"github.com/Ankairis/CodexOne/internal/config"
)

func TestCipherPersistsSQLiteKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "master.key")
	cfg := config.Config{StorageDriver: "sqlite", MasterKeyFile: keyPath}
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := first.Encrypt("secret-token")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := second.Decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "secret-token" {
		t.Fatalf("plain = %q", plain)
	}
}
