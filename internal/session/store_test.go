package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStorePutRemovesExpiredSessions(t *testing.T) {
	store := newMemoryStore(8)
	store.items = map[string]memoryItem{
		"expired": {value: "old", expiresAt: time.Now().Add(-time.Minute)},
		"live":    {value: "current", expiresAt: time.Now().Add(time.Hour)},
	}

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

func TestMemoryStoreBoundsLiveSessions(t *testing.T) {
	store := newMemoryStore(2)
	if err := store.Put(context.Background(), "one", "1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "two", "2", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "three", "3", time.Hour); !errors.Is(err, ErrCapacity) {
		t.Fatalf("third Put() error = %v, want ErrCapacity", err)
	}
	if len(store.items) != 2 {
		t.Fatalf("memory store size = %d, want 2", len(store.items))
	}
	if err := store.Put(context.Background(), "one", "updated", time.Hour); err != nil {
		t.Fatalf("updating an existing session at capacity: %v", err)
	}
}
