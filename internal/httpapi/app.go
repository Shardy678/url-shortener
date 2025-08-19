package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"url-shortener/internal/analytics"
	"url-shortener/internal/cache"
	"url-shortener/internal/config"
	"url-shortener/internal/metrics"
	"url-shortener/internal/ratelimit"
	"url-shortener/internal/storage"
	"url-shortener/internal/util"
)

type App struct {
	Cfg    config.Config
	Repo   *storage.Repo
	Cache  *cache.Redis
	Lim    ratelimit.Limiter
	Worker *analytics.Worker
	Router *gin.Engine
}

func New(cfg config.Config, pool *pgxpool.Pool, rc *cache.Redis, lim ratelimit.Limiter, worker *analytics.Worker, r *gin.Engine) *App {
	app := &App{Cfg: cfg, Repo: storage.New(pool), Cache: rc, Lim: lim, Worker: worker, Router: r}
	app.routes()
	return app
}

func (a *App) routes() {
	h := a.shortenHandler()
	if a.Lim != nil {
		a.Router.POST("/api/shorten", ratelimit.Middleware(a.Lim, util.GetClientIP), h)
	} else {
		a.Router.POST("/api/shorten", h)
	}

	a.Router.GET("/api/analytics/:code", a.analyticsHandler())
	a.Router.GET("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	a.Router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	a.Router.GET("/:code", a.redirectHandler())
}

func (a *App) shortenHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			URL  string `json:"url" binding:"required,url"`
			Slug string `json:"slug"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.URL) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		code := strings.TrimSpace(req.Slug)
		if code == "" {
			code = util.GenID(a.Cfg.IDLen)
		}

		var createErr error
		for attempt := 0; attempt < 5; attempt++ {
			createErr = a.Repo.Create(c.Request.Context(), code, req.URL)
			if createErr == nil {
				break
			}
			if errors.Is(createErr, storage.ErrSlugConflict) {
				if req.Slug != "" {
					c.JSON(http.StatusConflict, gin.H{"error": "slug already exists"})
					return
				}
				code = util.GenID(a.Cfg.IDLen)
				continue
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if createErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate unique id"})
			return
		}

		if a.Cache != nil {
			_ = a.Cache.Set(c.Request.Context(), code, req.URL, a.Cfg.CacheTTL)
		}
		short := strings.TrimRight(a.Cfg.BaseURL, "/") + "/" + code
		c.JSON(http.StatusOK, gin.H{"code": code, "short_url": short})
	}
}

func (a *App) analyticsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Param("code")
		limit := util.AtoiDefault(strings.TrimSpace(c.Query("limit")), 100)

		var total int64
		var last *time.Time
		err := a.Repo.Pool.QueryRow(c, `SELECT COUNT(*) as total, MAX(ts) AS last FROM clicks WHERE code = $1`, code).Scan(&total, &last)
		metrics.DBOpsTotal.WithLabelValues("analytics_summary").Inc()
		if err != nil {
			metrics.DBErrorsTotal.WithLabelValues("analytics_summary").Inc()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}

		rows, err := a.Repo.Pool.Query(c, `SELECT ts, ip, user_agent, device, os, browser FROM clicks WHERE code = $1 ORDER BY ts DESC LIMIT $2`, code, limit)
		metrics.DBOpsTotal.WithLabelValues("analytics_recent").Inc()
		if err != nil {
			metrics.DBErrorsTotal.WithLabelValues("analytics_recent").Inc()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		defer rows.Close()

		out := make([]analytics.Event, 0, limit)
		for rows.Next() {
			var e analytics.Event
			var ip sql.NullString
			if err := rows.Scan(&e.Timestamp, &ip, &e.UserAgent, &e.Device, &e.OS, &e.Browser); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "scan error"})
				return
			}
			e.IP = ip.String
			out = append(out, e)
		}
		c.JSON(http.StatusOK, gin.H{"code": code, "total": total, "last_click": last, "recent": out})
	}
}

func (a *App) redirectHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Param("code")
		if a.Cache != nil {
			if url, ok, err := a.Cache.Get(c, code); err == nil && ok {
				a.recordAndRedirect(c, code, url)
				return
			}
		}
		url, exists, err := a.Repo.Get(c, code)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if a.Cache != nil {
			_ = a.Cache.Set(c, code, url, a.Cfg.CacheTTL)
		}
		a.recordAndRedirect(c, code, url)
	}
}

func (a *App) recordAndRedirect(c *gin.Context, code, url string) {
	ua := c.Request.UserAgent()
	device, osName, browser := util.ParseUA(ua)
	evt := analytics.Event{Timestamp: time.Now().UTC(), IP: util.GetClientIP(c.Request), UserAgent: ua, Device: device, OS: osName, Browser: browser}
	if a.Worker != nil {
		a.Worker.Enqueue(analytics.EventWithCode{Code: code, E: evt})
	}
	metrics.RedirectsTotal.WithLabelValues(code).Inc()
	c.Redirect(http.StatusFound, url)
}
