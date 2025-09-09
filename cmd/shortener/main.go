package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"url-shortener/internal/analytics"
	"url-shortener/internal/cache"
	"url-shortener/internal/config"
	"url-shortener/internal/httpapi"
	"url-shortener/internal/metrics"
	"url-shortener/internal/ratelimit"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	cfg := config.Load()
	ctx := context.Background()

	// Metrics registry
	metrics.Init()

	// Postgres
	pool, err := pgxpool.New(ctx, cfg.DBURL)
	if err != nil {
		log.Fatalf("postgres connect: %v", err)
	}
	defer pool.Close()

	// Redis
	var rc *cache.Redis
	if cfg.RedisAddr != "" {
		rc = cache.New(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
		if err := rc.Client.Ping(ctx).Err(); err != nil {
			log.Printf("warning: redis ping failed: %v (continuing without cache)", err)
			rc = nil
		} else {
			log.Printf("redis connected")
		}
	}

	// Rate limiter
	var limiter ratelimit.Limiter
	if rc != nil {
		limiter, err = ratelimit.NewRedisLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst, rc.Client, cfg.RateLimitLuaPath)
		if err != nil {
			log.Fatalf("rate limiter: %v", err)
		}
	}

	// Click worker
	worker := analytics.NewWorker(pool, cfg.ClickBufSize, 200, cfg.ClickFlushEvery)
	worker.Start()
	defer worker.Stop()

	// Router
	r := gin.Default()
	r.Use(metrics.Middleware())
	r.GET("/metrics", metrics.Handler())

	app := httpapi.New(cfg, pool, rc, limiter, worker, r)

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: app.Router}

	idle := make(chan struct{})
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Printf("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(idle)
	}()

	log.Printf("listening on :%s", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server error: %v", err)
	}
	<-idle
}
