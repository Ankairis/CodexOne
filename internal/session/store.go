package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Ankairis/CodexOne/internal/config"
	"github.com/redis/go-redis/v9"
)

var ErrNotFound = errors.New("session not found")

var ErrCapacity = errors.New("session store capacity exceeded")

const defaultMemoryStoreCapacity = 4096

type Store interface {
	Put(context.Context, string, string, time.Duration) error
	Get(context.Context, string) (string, error)
	Delete(context.Context, string) error
	Ping(context.Context) error
	Close() error
}

func New(ctx context.Context, cfg config.Config) (Store, error) {
	if cfg.StorageDriver == "postgres" {
		options, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("parse REDIS_URL: %w", err)
		}
		client := redis.NewClient(options)
		if err = client.Ping(ctx).Err(); err != nil {
			client.Close()
			return nil, fmt.Errorf("connect redis: %w", err)
		}
		return &redisStore{client: client, prefix: "codexone:"}, nil
	}
	return newMemoryStore(defaultMemoryStoreCapacity), nil
}

type memoryItem struct {
	value     string
	expiresAt time.Time
}

type memoryStore struct {
	mu         sync.RWMutex
	items      map[string]memoryItem
	capacity   int
	nextExpiry time.Time
}

func (s *memoryStore) Put(_ context.Context, key, value string, ttl time.Duration) error {
	now := time.Now()
	expiresAt := now.Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(now)
	capacity := s.capacity
	if capacity <= 0 {
		capacity = defaultMemoryStoreCapacity
	}
	if _, exists := s.items[key]; !exists && len(s.items) >= capacity {
		return ErrCapacity
	}
	s.items[key] = memoryItem{value: value, expiresAt: expiresAt}
	if s.nextExpiry.IsZero() || expiresAt.Before(s.nextExpiry) {
		s.nextExpiry = expiresAt
	}
	return nil
}

func newMemoryStore(capacity int) *memoryStore {
	if capacity <= 0 {
		capacity = defaultMemoryStoreCapacity
	}
	return &memoryStore{items: make(map[string]memoryItem), capacity: capacity}
}

func (s *memoryStore) removeExpiredLocked(now time.Time) {
	if len(s.items) == 0 {
		s.nextExpiry = time.Time{}
		return
	}
	if !s.nextExpiry.IsZero() && s.nextExpiry.After(now) {
		return
	}
	nextExpiry := time.Time{}
	for key, item := range s.items {
		if !item.expiresAt.After(now) {
			delete(s.items, key)
			continue
		}
		if nextExpiry.IsZero() || item.expiresAt.Before(nextExpiry) {
			nextExpiry = item.expiresAt
		}
	}
	s.nextExpiry = nextExpiry
}

func (s *memoryStore) Get(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	item, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return "", ErrNotFound
	}
	now := time.Now()
	if !item.expiresAt.After(now) {
		s.mu.Lock()
		current, exists := s.items[key]
		if exists && !current.expiresAt.After(now) {
			delete(s.items, key)
			if current.expiresAt.Equal(s.nextExpiry) {
				s.nextExpiry = time.Time{}
			}
		}
		s.mu.Unlock()
		return "", ErrNotFound
	}
	return item.value, nil
}

func (s *memoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	item, exists := s.items[key]
	delete(s.items, key)
	if exists && item.expiresAt.Equal(s.nextExpiry) {
		s.nextExpiry = time.Time{}
	}
	s.mu.Unlock()
	return nil
}

func (s *memoryStore) Close() error { return nil }

func (s *memoryStore) Ping(context.Context) error { return nil }

type redisStore struct {
	client *redis.Client
	prefix string
}

func (s *redisStore) Put(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.client.Set(ctx, s.prefix+key, value, ttl).Err()
}

func (s *redisStore) Get(ctx context.Context, key string) (string, error) {
	value, err := s.client.Get(ctx, s.prefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	return value, err
}

func (s *redisStore) Delete(ctx context.Context, key string) error {
	return s.client.Del(ctx, s.prefix+key).Err()
}

func (s *redisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

func (s *redisStore) Close() error { return s.client.Close() }
