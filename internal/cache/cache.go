package cache

import (
	"context"
	"time"

	"url-shortener/internal/metrics"

	"github.com/redis/go-redis/v9"
)

type Redis struct{ Client *redis.Client }

func New(addr, password string, db int) *Redis {
	c := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})
	return &Redis{Client: c}
}

func (r *Redis) key(code string) string { return "url:" + code }

func (r *Redis) Get(ctx context.Context, code string) (string, bool, error) {
	if r == nil || r.Client == nil {
		return "", false, nil
	}
	v, err := r.Client.Get(ctx, r.key(code)).Result()
	if err == redis.Nil {
		metrics.CacheMissesTotal.Inc()
		return "", false, nil
	}
	if err != nil {
		metrics.CacheErrorsTotal.Inc()
		return "", false, err
	}
	metrics.CacheHitsTotal.Inc()
	return v, true, nil
}

func (r *Redis) Set(ctx context.Context, code, url string, ttl time.Duration) error {
	if r == nil || r.Client == nil {
		return nil
	}
	return r.Client.Set(ctx, r.key(code), url, ttl).Err()
}
