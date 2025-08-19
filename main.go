package main

import (
	"context"
	"crypto/rand"
	"database/sql"
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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	uaParser "github.com/mssola/user_agent"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var (
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	cacheTTL = time.Hour * 24
)

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests.",
		},
		[]string{"method", "route", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Latency of HTTP requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "route", "status"},
	)
	redirectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redirects_total",
			Help: "Number of redirects by code.",
		},
		[]string{"code"},
	)
	rateLimitAllowedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ratelimit_allowed_total",
			Help: "Requests allowed by rate limiter.",
		},
	)
	rateLimitDeniedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ratelimit_denied_total",
			Help: "Requests denied by rate limiter.",
		},
	)
	rateLimitRetryAfter = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "ratelimit_retry_after_seconds",
			Help:    "Retry-After seconds for denied requests.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10},
		},
	)

	dbOpsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "db_ops_total",
			Help: "Database operations.",
		},
		[]string{"op"},
	)
	dbErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "db_errors_total",
			Help: "Database errors.",
		},
		[]string{"op"},
	)

	cacheHitsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "cache_hits_total", Help: "Redis cache hits."},
	)
	cacheMissesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "cache_misses_total", Help: "Redis cache misses."},
	)
	cacheErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "cache_errors_total", Help: "Redis cache errors."},
	)

	clickQueueDepth = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "click_queue_depth",
			Help: "Buffered events in click channel.",
		},
		func() float64 { return float64(len(clickCh)) },
	)
	clickFlushBatchSize = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "click_flush_batch_size",
			Help:    "Batch size used when flushing clicks.",
			Buckets: []float64{1, 10, 50, 100, 150, 200, 400},
		},
	)
)

func init() {
	prometheus.MustRegister(
		httpRequestsTotal, httpRequestDuration,
		redirectsTotal,
		rateLimitAllowedTotal, rateLimitDeniedTotal, rateLimitRetryAfter,
		dbOpsTotal, dbErrorsTotal,
		cacheHitsTotal, cacheMissesTotal, cacheErrorsTotal,
		clickQueueDepth, clickFlushBatchSize,
	)
}

func promHTTPMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		dur := time.Since(start).Seconds()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		status := strconv.Itoa(c.Writer.Status())

		httpRequestsTotal.WithLabelValues(c.Request.Method, route, status).Inc()
		httpRequestDuration.WithLabelValues(c.Request.Method, route, status).Observe(dur)
	}
}

/* -------------------- analytics -------------------- */

type Event struct {
	Timestamp time.Time `json:"timestamp"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Device    string    `json:"device"`
	OS        string    `json:"os"`
	Browser   string    `json:"browser"`
}

type EventWithCode struct {
	Code string
	E    Event
}

var clickCh = make(chan EventWithCode, 1024)

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

func startClickWorker(pool *pgxpool.Pool) (stop chan struct{}, stopped chan struct{}) {
	stop = make(chan struct{})
	stopped = make(chan struct{})
	go func() {
		defer close(stopped)
		buf := make([]EventWithCode, 0, 200)
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case ev := <-clickCh:
				buf = append(buf, ev)
				if len(buf) >= 200 {
					flushClicks(context.Background(), pool, buf)
					buf = buf[:0]
				}
			case <-ticker.C:
				if len(buf) > 0 {
					flushClicks(context.Background(), pool, buf)
					buf = buf[:0]
				}
			case <-stop:
				if len(buf) > 0 {
					flushClicks(context.Background(), pool, buf)
				}
				return
			}
		}
	}()
	return
}

func flushClicks(ctx context.Context, pool *pgxpool.Pool, batch []EventWithCode) {
	if len(batch) == 0 {
		return
	}
	clickFlushBatchSize.Observe(float64(len(batch)))

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	sb := strings.Builder{}
	sb.WriteString("INSERT INTO clicks(code, ts, ip, user_agent, device, os, browser) VALUES ")
	args := make([]any, 0, len(batch)*7)
	for i, it := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		idx := i * 7
		sb.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d)", idx+1, idx+2, idx+3, idx+4, idx+5, idx+6, idx+7))
		args = append(args, it.Code, it.E.Timestamp, it.E.IP, it.E.UserAgent, it.E.Device, it.E.OS, it.E.Browser)
	}
	dbOpsTotal.WithLabelValues("clicks_insert").Inc()
	if _, err := pool.Exec(ctx, sb.String(), args...); err != nil {
		dbErrorsTotal.WithLabelValues("clicks_insert").Inc()
	}
}

/* -------------------- persistence & caching -------------------- */

var ErrConflict = errors.New("slug already exists")

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
	dbOpsTotal.WithLabelValues("create_url").Inc()
	ct, err := r.pool.Exec(ctx, "INSERT INTO urls (code, target_url) VALUES ($1, $2) ON CONFLICT (code) DO NOTHING", code, url)
	if err != nil {
		dbErrorsTotal.WithLabelValues("create_url").Inc()
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

func (r *pgRepo) Get(ctx context.Context, code string) (string, bool, error) {
	dbOpsTotal.WithLabelValues("get_url").Inc()
	var url string
	err := r.pool.QueryRow(ctx, "SELECT target_url FROM urls WHERE code=$1", code).Scan(&url)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		dbErrorsTotal.WithLabelValues("get_url").Inc()
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
		cacheMissesTotal.Inc()
		return "", false, nil
	}
	if err != nil {
		cacheErrorsTotal.Inc()
		return "", false, err
	}
	cacheHitsTotal.Inc()
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

func newRedisLimiter(rps float64, burst int, rc *redis.Client, scriptPath string) (*redisLimiter, error) {
	script, err := loadLuaScript(scriptPath)
	if err != nil {
		return nil, err
	}
	return &redisLimiter{rps: rps, burst: burst, redis: rc, prefix: "rl:", script: script}, nil
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

func shortenHandler(pg *pgRepo, rc *redisCache, baseURL string, idLen int) gin.HandlerFunc {
	return func(c *gin.Context) {
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
	}
}

func rateLimitMiddleware(l Limiter, keyFunc func(*http.Request) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := keyFunc(c.Request)
		ok, retryAfter, _ := l.Allow(c.Request.Context(), key)
		if !ok {
			rateLimitDeniedTotal.Inc()
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			}
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		rateLimitAllowedTotal.Inc()
		c.Next()
	}
}

/* -------------------- server -------------------- */

func main() {
	ctx := context.Background()
	port := getenv("PORT", "8080")
	baseURL := getenv("BASE_URL", "http://localhost:"+port)
	idLen := 7

	rps := 5.0
	if v := getenv("RATE_LIMIT_RPS", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			rps = f
		}
	}
	burst := 20
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

	r := gin.Default()
	r.Use(promHTTPMiddleware())
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	h := shortenHandler(pg, rc, baseURL, idLen)
	if limiter != nil {
		r.POST("/api/shorten", rateLimitMiddleware(limiter, getClientIP), h)
	} else {
		r.POST("/api/shorten", h)
	}

	stop, stopped := startClickWorker(pg.pool)
	defer func() { close(stop); <-stopped }()

	r.GET("/api/analytics/:code", func(c *gin.Context) {
		code := c.Param("code")
		limit := 100
		if q := strings.TrimSpace(c.Query("limit")); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 {
				limit = n
			}
		}

		ctx := c.Request.Context()

		var total int64
		var last *time.Time
		err := pg.pool.QueryRow(ctx, `
			SELECT COUNT(*) as total, MAX(ts) AS last
			FROM clicks WHERE code = $1`, code).Scan(&total, &last)
		dbOpsTotal.WithLabelValues("analytics_summary").Inc()
		if err != nil {
			dbErrorsTotal.WithLabelValues("analytics_summary").Inc()
			c.JSON(500, gin.H{"error": "db error"})
			return
		}

		rows, err := pg.pool.Query(ctx, `
			SELECT ts, ip, user_agent, device, os, browser
			FROM clicks WHERE code = $1
			ORDER BY ts DESC
			LIMIT $2`, code, limit)
		dbOpsTotal.WithLabelValues("analytics_recent").Inc()
		if err != nil {
			dbErrorsTotal.WithLabelValues("analytics_recent").Inc()
			c.JSON(500, gin.H{"error": "db error"})
			return
		}
		defer rows.Close()

		eventsOut := make([]Event, 0, limit)
		for rows.Next() {
			var e Event
			var ip sql.NullString
			if err := rows.Scan(&e.Timestamp, &ip, &e.UserAgent, &e.Device, &e.OS, &e.Browser); err != nil {
				c.JSON(500, gin.H{"error": "scan error"})
				return
			}
			e.IP = ip.String
			eventsOut = append(eventsOut, e)
		}
		c.JSON(http.StatusOK, gin.H{
			"code":  code,
			"total": total,
			"last_click": func() *time.Time {
				if last == nil {
					return nil
				}
				return last
			}(),
			"recent": eventsOut,
		})
	})

	r.GET("/favicon.ico", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	r.GET("/:code", func(c *gin.Context) {
		code := c.Param("code")
		ctx := c.Request.Context()

		if rc != nil {
			if url, ok, err := rc.Get(ctx, code); err == nil && ok {
				recordAndRedirect(c, code, url)
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
		recordAndRedirect(c, code, url)
	})

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	_ = r.Run(":" + port)
}

func recordAndRedirect(c *gin.Context, code, url string) {
	ua := c.Request.UserAgent()
	device, osName, browser := parseUA(ua)

	evt := Event{
		Timestamp: time.Now().UTC(),
		IP:        getClientIP(c.Request),
		UserAgent: ua,
		Device:    device,
		OS:        osName,
		Browser:   browser,
	}

	select {
	case clickCh <- EventWithCode{Code: code, E: evt}:
	default:
	}

	redirectsTotal.WithLabelValues(code).Inc()
	c.Redirect(http.StatusFound, url)
}

func genID(n int) string {
	b := make([]byte, n)
	for i := range b {
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
