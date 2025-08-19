package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port             string
	BaseURL          string
	IDLen            int
	DBURL            string
	RedisAddr        string
	RedisPassword    string
	RedisDB          int
	CacheTTL         time.Duration
	RateLimitRPS     float64
	RateLimitBurst   int
	RateLimitLuaPath string
	ClickBufSize     int
	ClickFlushEvery  time.Duration
	ShutdownGrace    time.Duration
}

func Load() Config {
	cfg := Config{
		Port:             getenv("PORT", "8080"),
		IDLen:            atoi(getenv("ID_LEN", "7"), 7),
		DBURL:            mustEnv("DATABASE_URL"),
		RedisAddr:        getenv("REDIS_ADDR", ""),
		RedisPassword:    getenv("REDIS_PASSWORD", ""),
		RedisDB:          atoi(getenv("REDIS_DB", "0"), 0),
		CacheTTL:         dur(getenv("CACHE_TTL", "24h"), 24*time.Hour),
		RateLimitRPS:     atof(getenv("RATE_LIMIT_RPS", "5"), 5),
		RateLimitBurst:   atoi(getenv("RATE_LIMIT_BURST", "20"), 20),
		RateLimitLuaPath: getenv("RATE_LIMIT_LUA", "token_bucket.lua"),
		ClickBufSize:     atoi(getenv("CLICK_BUFFER", "1024"), 1024),
		ClickFlushEvery:  dur(getenv("CLICK_FLUSH_EVERY", "400ms"), 400*time.Millisecond),
		ShutdownGrace:    dur(getenv("SHUTDOWN_GRACE", "8s"), 8*time.Second),
	}
	cfg.BaseURL = getenv("BASE_URL", "http://localhost:"+cfg.Port)
	return cfg
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
func atof(s string, def float64) float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}
func dur(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		panic("missing required env var " + k)
	}
	return v
}
