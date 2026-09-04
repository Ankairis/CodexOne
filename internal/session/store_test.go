package session

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStorePutRemovesExpiredSessions(t *testing.T) {
	store := &memoryStore{items: map[string]memoryItem{
		"expired": {value: "old", expiresAt: time.Now().Add(-time.Minute)},
		"live":    {value: "current", expiresAt: time.Now().Add(time.Hour)},
	}}

	if err := store.Put(context.Background(), "new", "value", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.items["expired"]; ok {
		t.Fatal("expired session was retained")
	}
	if _, ok := store.items["live"]; !ok {
		t.Fatal("live session was removed")
	}
	if value, err := store.Get(context.Background(), "new"); err != nil || value != "value" {
		t.Fatalf("new session = %q, %v", value, err)
	}
}
