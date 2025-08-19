package ratelimit

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"url-shortener/internal/metrics"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type Limiter interface {
	Allow(ctx context.Context, key string) (bool, time.Duration, error)
}

type redisLimiter struct {
	rps    float64
	burst  int
	redis  *redis.Client
	prefix string
	script *redis.Script
}

func NewRedisLimiter(rps float64, burst int, rc *redis.Client, scriptPath string) (Limiter, error) {
	code, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("read lua file: %w", err)
	}
	return &redisLimiter{rps: rps, burst: burst, redis: rc, prefix: "rl:", script: redis.NewScript(string(code))}, nil
}

func (l *redisLimiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	if l.redis == nil || l.script == nil {
		return true, 0, nil
	}
	now := time.Now().UnixMilli()
	res, err := l.script.Run(ctx, l.redis, []string{l.prefix + key}, now, l.rps, l.burst).Result()
	if err != nil {
		return true, 0, nil
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return true, 0, nil
	}
	allowedInt, _ := arr[0].(int64)
	allowed := allowedInt == 1
	var tokens float64
	switch v := arr[1].(type) {
	case float64:
		tokens = v
	case string:
		if f, e := strconv.ParseFloat(v, 64); e == nil {
			tokens = f
		}
	}
	if allowed {
		return true, 0, nil
	}
	waitMs := math.Ceil((1.0 - tokens) / l.rps * 1000.0)
	if waitMs < 100 {
		waitMs = 100
	}
	metrics.RateLimitRetryAfter.Observe(waitMs / 1000)
	return false, time.Duration(waitMs) * time.Millisecond, nil
}

func Middleware(l Limiter, keyFunc func(*http.Request) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := keyFunc(c.Request)
		ok, retryAfter, _ := l.Allow(c.Request.Context(), key)
		if !ok {
			metrics.RateLimitDeniedTotal.Inc()
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			}
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		metrics.RateLimitAllowedTotal.Inc()
		c.Next()
	}
}
