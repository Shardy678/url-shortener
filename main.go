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
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	uaParser "github.com/mssola/user_agent"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

/* -------------------------------------------------------------------------- */
/*                                   Конфиг                                   */
/* -------------------------------------------------------------------------- */

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

func loadConfig() Config {
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

/* -------------------------------------------------------------------------- */
/*                                Обсервабилити                               */
/* -------------------------------------------------------------------------- */

var (
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total", Help: "Total HTTP requests."},
		[]string{"method", "route", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "Latency of HTTP requests.", Buckets: prometheus.DefBuckets},
		[]string{"method", "route", "status"},
	)
	redirectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "redirects_total", Help: "Number of redirects by code."},
		[]string{"code"},
	)
	rateLimitAllowedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "ratelimit_allowed_total", Help: "Requests allowed by rate limiter."},
	)
	rateLimitDeniedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "ratelimit_denied_total", Help: "Requests denied by rate limiter."},
	)
	rateLimitRetryAfter = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "ratelimit_retry_after_seconds", Help: "Retry-After seconds for denied requests.", Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10}},
	)

	dbOpsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "db_ops_total", Help: "Database operations."},
		[]string{"op"},
	)
	dbErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "db_errors_total", Help: "Database errors."},
		[]string{"op"},
	)

	cacheHitsTotal   = prometheus.NewCounter(prometheus.CounterOpts{Name: "cache_hits_total", Help: "Redis cache hits."})
	cacheMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "cache_misses_total", Help: "Redis cache misses."})
	cacheErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "cache_errors_total", Help: "Redis cache errors."})

	clickQueueDepth     = prometheus.NewGauge(prometheus.GaugeOpts{Name: "click_queue_depth", Help: "Buffered events in click channel."})
	clickFlushBatchSize = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "click_flush_batch_size", Help: "Batch size used when flushing clicks.", Buckets: []float64{1, 10, 50, 100, 150, 200, 400}})
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

/* -------------------------------------------------------------------------- */
/*                                  Analytics                                 */
/* -------------------------------------------------------------------------- */

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

type Worker struct {
	buf        []EventWithCode
	flushEvery time.Duration
	maxBatch   int
	ch         chan EventWithCode
	stop       chan struct{}
	stopped    chan struct{}
	pool       *pgxpool.Pool
}

func newWorker(pool *pgxpool.Pool, bufSize, maxBatch int, flushEvery time.Duration) *Worker {
	w := &Worker{
		buf:        make([]EventWithCode, 0, maxBatch),
		flushEvery: flushEvery,
		maxBatch:   maxBatch,
		ch:         make(chan EventWithCode, bufSize),
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
		pool:       pool,
	}
	return w
}

func (w *Worker) Start() {
	go func() {
		defer close(w.stopped)
		ticker := time.NewTicker(w.flushEvery)
		defer ticker.Stop()
		for {
			clickQueueDepth.Set(float64(len(w.ch)))
			select {
			case ev := <-w.ch:
				w.buf = append(w.buf, ev)
				if len(w.buf) >= w.maxBatch {
					w.flush(context.Background())
				}
			case <-ticker.C:
				if len(w.buf) > 0 {
					w.flush(context.Background())
				}
			case <-w.stop:
				if len(w.buf) > 0 {
					w.flush(context.Background())
				}
				return
			}
		}
	}()
}

func (w *Worker) Stop() { close(w.stop); <-w.stopped }

func (w *Worker) Enqueue(ev EventWithCode) {
	select {
	case w.ch <- ev:
	default:
	}
	clickQueueDepth.Set(float64(len(w.ch)))
}

func (w *Worker) flush(ctx context.Context) {
	if len(w.buf) == 0 {
		return
	}
	clickFlushBatchSize.Observe(float64(len(w.buf)))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	sb := strings.Builder{}
	sb.WriteString("INSERT INTO clicks(code, ts, ip, user_agent, device, os, browser) VALUES ")
	args := make([]any, 0, len(w.buf)*7)
	for i, it := range w.buf {
		if i > 0 {
			sb.WriteByte(',')
		}
		idx := i * 7
		sb.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d)", idx+1, idx+2, idx+3, idx+4, idx+5, idx+6, idx+7))
		args = append(args, it.Code, it.E.Timestamp, it.E.IP, it.E.UserAgent, it.E.Device, it.E.OS, it.E.Browser)
	}
	dbOpsTotal.WithLabelValues("clicks_insert").Inc()
	if _, err := w.pool.Exec(ctx, sb.String(), args...); err != nil {
		dbErrorsTotal.WithLabelValues("clicks_insert").Inc()
		log.Printf("click insert error: %v", err)
	}
	w.buf = w.buf[:0]
}

/* -------------------------------------------------------------------------- */
/*                                 БД и Кэш                                   */
/* -------------------------------------------------------------------------- */

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
	return nil
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

/* -------------------------------------------------------------------------- */
/*                               Rate Limiting                                */
/* -------------------------------------------------------------------------- */

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
	rateLimitRetryAfter.Observe(waitMs / 1000)
	return false, time.Duration(waitMs) * time.Millisecond, nil
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

/* -------------------------------------------------------------------------- */
/*                                   Сервер                                   */
/* -------------------------------------------------------------------------- */

type App struct {
	cfg     Config
	pg      *pgRepo
	cache   *redisCache
	limiter Limiter
	worker  *Worker
	router  *gin.Engine
}

func newApp(ctx context.Context, cfg Config) (*App, error) {
	pg, err := newPGRepo(ctx, cfg.DBURL)
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}

	var rc *redisCache
	if cfg.RedisAddr != "" {
		rc = newRedis(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
		if err := rc.client.Ping(ctx).Err(); err != nil {
			log.Printf("warning: redis ping failed: %v (continuing without cache)", err)
			rc = nil
		} else {
			log.Printf("redis connected")
		}
	}

	var limiter Limiter
	if rc != nil && rc.client != nil {
		limiter, err = newRedisLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst, rc.client, cfg.RateLimitLuaPath)
		if err != nil {
			return nil, fmt.Errorf("rate limiter: %w", err)
		}
	}

	w := newWorker(pg.pool, cfg.ClickBufSize, 200, cfg.ClickFlushEvery)
	w.Start()

	r := gin.Default()
	r.Use(promHTTPMiddleware())
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	app := &App{cfg: cfg, pg: pg, cache: rc, limiter: limiter, worker: w, router: r}
	app.routes()
	return app, nil
}

func (a *App) Close() {
	if a.worker != nil {
		a.worker.Stop()
	}
	if a.pg != nil && a.pg.pool != nil {
		a.pg.pool.Close()
	}
}

func (a *App) routes() {
	h := a.shortenHandler()
	if a.limiter != nil {
		a.router.POST("/api/shorten", rateLimitMiddleware(a.limiter, getClientIP), h)
	} else {
		a.router.POST("/api/shorten", h)
	}

	a.router.GET("/api/analytics/:code", a.analyticsHandler())
	a.router.GET("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	a.router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	a.router.GET("/:code", a.redirectHandler())
}

func (a *App) shortenHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			URL  string `json:"url" binding:"required,url"`
			Slug string `json:"slug"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.URL) == "" {
			log.Printf("invalid shorten body: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}

		code := strings.TrimSpace(req.Slug)
		if code == "" {
			code = genID(a.cfg.IDLen)
		}

		var createErr error
		for attempt := 0; attempt < 5; attempt++ {
			createErr = a.pg.Create(c.Request.Context(), code, req.URL)
			if createErr == nil {
				break
			}
			if errors.Is(createErr, ErrConflict) {
				if req.Slug != "" {
					c.JSON(http.StatusConflict, gin.H{"error": "slug already exists"})
					return
				}
				code = genID(a.cfg.IDLen)
				continue
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if createErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate unique id"})
			return
		}

		if a.cache != nil {
			_ = a.cache.Set(c.Request.Context(), code, req.URL, a.cfg.CacheTTL)
		}

		short := strings.TrimRight(a.cfg.BaseURL, "/") + "/" + code
		c.JSON(http.StatusOK, gin.H{"code": code, "short_url": short})
	}
}

func (a *App) analyticsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Param("code")
		limit := atoi(strings.TrimSpace(c.Query("limit")), 100)
		ctx := c.Request.Context()

		var total int64
		var last *time.Time
		err := a.pg.pool.QueryRow(ctx, `SELECT COUNT(*) as total, MAX(ts) AS last FROM clicks WHERE code = $1`, code).Scan(&total, &last)
		dbOpsTotal.WithLabelValues("analytics_summary").Inc()
		if err != nil {
			dbErrorsTotal.WithLabelValues("analytics_summary").Inc()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}

		rows, err := a.pg.pool.Query(ctx, `SELECT ts, ip, user_agent, device, os, browser FROM clicks WHERE code = $1 ORDER BY ts DESC LIMIT $2`, code, limit)
		dbOpsTotal.WithLabelValues("analytics_recent").Inc()
		if err != nil {
			dbErrorsTotal.WithLabelValues("analytics_recent").Inc()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		defer rows.Close()

		eventsOut := make([]Event, 0, limit)
		for rows.Next() {
			var e Event
			var ip sql.NullString
			if err := rows.Scan(&e.Timestamp, &ip, &e.UserAgent, &e.Device, &e.OS, &e.Browser); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "scan error"})
				return
			}
			e.IP = ip.String
			eventsOut = append(eventsOut, e)
		}
		c.JSON(http.StatusOK, gin.H{"code": code, "total": total, "last_click": last, "recent": eventsOut})
	}
}

func (a *App) redirectHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Param("code")
		ctx := c.Request.Context()

		if a.cache != nil {
			if url, ok, err := a.cache.Get(ctx, code); err == nil && ok {
				a.recordAndRedirect(c, code, url)
				return
			}
		}

		url, exists, err := a.pg.Get(ctx, code)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}

		if a.cache != nil {
			_ = a.cache.Set(ctx, code, url, a.cfg.CacheTTL)
		}
		a.recordAndRedirect(c, code, url)
	}
}

func (a *App) recordAndRedirect(c *gin.Context, code, url string) {
	ua := c.Request.UserAgent()
	device, osName, browser := parseUA(ua)
	evt := Event{Timestamp: time.Now().UTC(), IP: getClientIP(c.Request), UserAgent: ua, Device: device, OS: osName, Browser: browser}
	if a.worker != nil {
		a.worker.Enqueue(EventWithCode{Code: code, E: evt})
	}
	redirectsTotal.WithLabelValues(code).Inc()
	c.Redirect(http.StatusFound, url)
}

/* -------------------------------------------------------------------------- */
/*                                  Утилиты                                   */
/* -------------------------------------------------------------------------- */

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

/* -------------------------------------------------------------------------- */
/*                                     Main                                   */
/* -------------------------------------------------------------------------- */

func main() {
	cfg := loadConfig()
	ctx := context.Background()

	app, err := newApp(ctx, cfg)
	if err != nil {
		log.Fatalf("boot error: %v", err)
	}
	defer app.Close()

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: app.router}

	// graceful shutdown
	idleConnsClosed := make(chan struct{})
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Printf("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(idleConnsClosed)
	}()

	log.Printf("listening on :%s", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server error: %v", err)
	}
	<-idleConnsClosed
}
