package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const idemTTL = 24 * time.Hour

type IdempStore interface {
	Get(ctx context.Context, appID, key string) ([]byte, bool, error)
	Set(ctx context.Context, appID, key string, val []byte) error
}

type RedisIdem struct {
	rdb *redis.Client
}

func NewRedisIdem(rdb *redis.Client) *RedisIdem {
	return &RedisIdem{rdb: rdb}
}

func (r *RedisIdem) ns(appID, key string) string {
	return fmt.Sprintf("idem:%s:%s", appID, key)
}

func (r *RedisIdem) Get(ctx context.Context, appID, key string) ([]byte, bool, error) {
	if r == nil || r.rdb == nil {
		return nil, false, nil
	}
	val, err := r.rdb.Get(ctx, r.ns(appID, key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("idem get: %w", err)
	}
	return val, true, nil
}

func (r *RedisIdem) Set(ctx context.Context, appID, key string, val []byte) error {
	if r == nil || r.rdb == nil {
		return nil
	}
	if err := r.rdb.Set(ctx, r.ns(appID, key), val, idemTTL).Err(); err != nil {
		return fmt.Errorf("idem set: %w", err)
	}
	return nil
}
