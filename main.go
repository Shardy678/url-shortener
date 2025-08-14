package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	uaParser "github.com/mssola/user_agent"
	"github.com/redis/go-redis/v9"
)

var (
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	cacheTTL = time.Hour * 24
)

/* -------------------- analytics -------------------- */

type Event struct {
	Timestamp time.Time `json:"timestamp"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Device    string    `json:"device"`
	OS        string    `json:"os"`
	Browser   string    `json:"browser"`
}

type analyticsStore struct {
	mu   sync.RWMutex
	data map[string][]Event
}

func newAnalyticsStore() *analyticsStore {
	return &analyticsStore{
		data: make(map[string][]Event),
	}
}

func (a *analyticsStore) add(code string, e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data[code] = append(a.data[code], e)
}

func (a *analyticsStore) get(code string, limit int) []Event {
	a.mu.RLock()
	defer a.mu.RUnlock()
	list := a.data[code]
	if limit <= 0 || limit >= len(list) {
		out := make([]Event, len(list))
		copy(out, list)
		return out
	}
	start := len(list) - limit
	out := make([]Event, limit)
	copy(out, list[start:])
	return out
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			ip := strings.TrimSpace(part)
			if ip != "" {
				return ip
			}
		}
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func parseUA(uaStr string) (device, os, browser string) {
	ua := uaParser.New(uaStr)

	browserName, _ := ua.Browser()
	browser = browserName
	os = ua.OS()

	switch {
	case ua.Bot():
		device = "bot"
	case ua.Mobile():
		device = "mobile"
	default:
		device = "desktop"
	}

	return
}

/* -------------------- persistence & caching -------------------- */

var ErrConflict = errors.New("slug already exists")

type repo interface {
	Create(ctx context.Context, code, url string) error
	Get(ctx context.Context, code string) (string, bool, error)
}

type pgRepo struct{ pool *pgxpool.Pool }

type redisCache struct{ client *redis.Client }

func newPGRepo(ctx context.Context, dsn string) (*pgRepo, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &pgRepo{pool: pool}, nil
}

func (r *pgRepo) Create(ctx context.Context, code, url string) error {
	ct, err := r.pool.Exec(ctx, "INSERT INTO urls (code, target_url) VALUES ($1, $2) ON CONFLICT (code) DO NOTHING", code, url)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

func (r *pgRepo) Get(ctx context.Context, code string) (string, bool, error) {
	var url string
	err := r.pool.QueryRow(ctx, "SELECT target_url FROM urls WHERE code=$1", code).Scan(&url)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return url, true, nil
}

func newRedis(addr, password string, db int) *redisCache {
	client := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})
	return &redisCache{client: client}
}

func (c *redisCache) key(code string) string { return "url:" + code }

func (c *redisCache) Get(ctx context.Context, code string) (string, bool, error) {
	if c == nil || c.client == nil {
		return "", false, nil
	}
	v, err := c.client.Get(ctx, c.key(code)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (c *redisCache) Set(ctx context.Context, code, url string, ttl time.Duration) error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Set(ctx, c.key(code), url, ttl).Err()
}

/* -------------------- rate limiting -------------------- */

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

func newRedisLimiter(rps float64, burst int, rc *redis.Client, scripthPath string) (*redisLimiter, error) {
	code, err := os.ReadFile(scripthPath)
	if err != nil {
		return nil, fmt.Errorf("read lua file: %w", err)
	}
	return &redisLimiter{rps: rps, burst: burst, redis: rc, prefix: "rl:", script: redis.NewScript(string(code))}, nil
}

func loadLuaScript(path string) (*redis.Script, error) {
	code, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read lua file: %w", err)
	}
	return redis.NewScript(string(code)), nil
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
		if f, err := strconv.ParseFloat(v, 64); err == nil {
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
	return false, time.Duration(waitMs) * time.Millisecond, nil
}

func rateLimitMiddleware(l Limiter, keyFunc func(*http.Request) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ok, retryAfter, _ := l.Allow(c.Request.Context(), getClientIP(c.Request))
		if !ok {
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			}
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		}
	}
}

/* -------------------- server -------------------- */

func main() {
	ctx := context.Background()
	port := getenv("PORT", "8080")
	baseURL := getenv("BASE_URL", "http://localhost:"+port)
	idLen := 7

	rps := 5.0 // tokens per second
	if v := getenv("RATE_LIMIT_RPS", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			rps = f
		}
	}
	burst := 20 // bucket size
	if v := getenv("RATE_LIMIT_BURST", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			burst = n
		}
	}

	if ttlSec := getenv("CACHE_TTL_SECONDS", ""); ttlSec != "" {
		if n, err := strconv.Atoi(ttlSec); err == nil && n > 0 {
			cacheTTL = time.Duration(n) * time.Second
		}
	}

	pgURL := mustEnv("DATABASE_URL")
	pg, err := newPGRepo(ctx, pgURL)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pg.pool.Close()

	redisAddr := getenv("REDIS_ADDR", "")
	var rc *redisCache
	if redisAddr != "" {
		redisPass := getenv("REDIS_PASSWORD", "")
		redisDB := 0
		if v := getenv("REDIS_DB", ""); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				redisDB = n
			}
		}
		rc = newRedis(redisAddr, redisPass, redisDB)
		if err := rc.client.Ping(ctx).Err(); err != nil {
			log.Printf("warning: redis ping failed: %v (continuing without cache)", err)
			rc = nil
		} else {
			log.Printf("redis connected")
		}
	}
	var limiter Limiter
	if rc != nil && rc.client != nil {
		limiter, err = newRedisLimiter(rps, burst, rc.client, "token_bucket.lua")
		if err != nil {
			log.Fatalf("rate limiter: %v", err)
		}
	}

	events := newAnalyticsStore()

	r := gin.Default()

	r.POST("/api/shorten", rateLimitMiddleware(limiter, getClientIP), func(c *gin.Context) {
		var req struct {
			URL  string `json:"url" binding:"required,url"`
			Slug string `json:"slug"`
		}

		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.URL) == "" {
			log.Printf("Invalid URL from client: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}

		code := strings.TrimSpace(req.Slug)
		if code == "" {
			code = genID(idLen)
		}

		var createErr error
		for attempt := 0; attempt < 5; attempt++ {
			createErr = pg.Create(c.Request.Context(), code, req.URL)
			if createErr == nil {
				break
			}
			if errors.Is(createErr, ErrConflict) {
				if req.Slug != "" {
					c.JSON(http.StatusConflict, gin.H{"error": "slug already exists"})
					return
				}
				code = genID(idLen)
				continue
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if createErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate unique id"})
			return
		}

		if rc != nil {
			_ = rc.Set(c.Request.Context(), code, req.URL, cacheTTL)
		}

		short := strings.TrimRight(baseURL, "/") + "/" + code
		c.JSON(http.StatusOK, gin.H{
			"code":      code,
			"short_url": short,
		})
	})

	r.GET("/api/analytics/:code", func(c *gin.Context) {
		code := c.Param("code")
		limit := 100
		if q := strings.TrimSpace(c.Query("limit")); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 {
				limit = n
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"code":   code,
			"events": events.get(code, limit),
		})
	})

	r.GET("/:code", func(c *gin.Context) {
		code := c.Param("code")
		ctx := c.Request.Context()

		if rc != nil {
			if url, ok, err := rc.Get(ctx, code); err == nil && ok {
				recordAndRedirect(c, events, code, url)
				return
			}
		}

		url, exists, err := pg.Get(ctx, code)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}

		if rc != nil {
			_ = rc.Set(ctx, code, url, cacheTTL)
		}
		recordAndRedirect(c, events, code, url)
	})

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	_ = r.Run(":" + port)
}

func recordAndRedirect(c *gin.Context, events *analyticsStore, code, url string) {
	ua := c.Request.UserAgent()
	device, osName, browser := parseUA(ua)

	e := Event{
		Timestamp: time.Now().UTC(),
		IP:        getClientIP(c.Request),
		UserAgent: ua,
		Device:    device,
		OS:        osName,
		Browser:   browser,
	}
	events.add(code, e)
	c.Redirect(http.StatusFound, url)
}

func genID(n int) string {
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		v, _ := rand.Int(rand.Reader, big.NewInt(62))
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
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
		log.Fatalf("missing required env var %s", k)
	}
	return v
}
