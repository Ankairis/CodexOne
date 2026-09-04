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

type Store interface {
	Put(context.Context, string, string, time.Duration) error
	Get(context.Context, string) (string, error)
	Delete(context.Context, string) error
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
	return &memoryStore{items: make(map[string]memoryItem)}, nil
}

type memoryItem struct {
	value     string
	expiresAt time.Time
}

type memoryStore struct {
	mu    sync.RWMutex
	items map[string]memoryItem
}

func (s *memoryStore) Put(_ context.Context, key, value string, ttl time.Duration) error {
	now := time.Now()
	s.mu.Lock()
	for itemKey, item := range s.items {
		if !item.expiresAt.After(now) {
			delete(s.items, itemKey)
		}
	}
	s.items[key] = memoryItem{value: value, expiresAt: now.Add(ttl)}
	s.mu.Unlock()
	return nil
}

func (s *memoryStore) Get(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	item, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return "", ErrNotFound
	}
	if time.Now().After(item.expiresAt) {
		s.mu.Lock()
		delete(s.items, key)
		s.mu.Unlock()
		return "", ErrNotFound
	}
	return item.value, nil
}

func (s *memoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
	return nil
}

func (s *memoryStore) Close() error { return nil }

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

func (s *redisStore) Close() error { return s.client.Close() }
