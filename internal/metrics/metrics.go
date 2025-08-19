package metrics

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "http_requests_total", Help: "Total HTTP requests."},
		[]string{"method", "route", "status"},
	)
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "Latency of HTTP requests.", Buckets: prometheus.DefBuckets},
		[]string{"method", "route", "status"},
	)
	RedirectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "redirects_total", Help: "Number of redirects by code."},
		[]string{"code"},
	)

	RateLimitAllowedTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "ratelimit_allowed_total", Help: "Requests allowed by rate limiter."})
	RateLimitDeniedTotal  = prometheus.NewCounter(prometheus.CounterOpts{Name: "ratelimit_denied_total", Help: "Requests denied by rate limiter."})
	RateLimitRetryAfter   = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "ratelimit_retry_after_seconds", Help: "Retry-After seconds for denied requests.", Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10}})

	DBOpsTotal    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "db_ops_total", Help: "Database operations."}, []string{"op"})
	DBErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "db_errors_total", Help: "Database errors."}, []string{"op"})

	CacheHitsTotal   = prometheus.NewCounter(prometheus.CounterOpts{Name: "cache_hits_total", Help: "Redis cache hits."})
	CacheMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "cache_misses_total", Help: "Redis cache misses."})
	CacheErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "cache_errors_total", Help: "Redis cache errors."})

	ClickQueueDepth     = prometheus.NewGauge(prometheus.GaugeOpts{Name: "click_queue_depth", Help: "Buffered events in click channel."})
	ClickFlushBatchSize = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "click_flush_batch_size", Help: "Batch size used when flushing clicks.", Buckets: []float64{1, 10, 50, 100, 150, 200, 400}})
)

func Init() {
	prometheus.MustRegister(
		HTTPRequestsTotal, HTTPRequestDuration,
		RedirectsTotal,
		RateLimitAllowedTotal, RateLimitDeniedTotal, RateLimitRetryAfter,
		DBOpsTotal, DBErrorsTotal,
		CacheHitsTotal, CacheMissesTotal, CacheErrorsTotal,
		ClickQueueDepth, ClickFlushBatchSize,
	)
}

func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		dur := time.Since(start).Seconds()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		status := strconv.Itoa(c.Writer.Status())
		HTTPRequestsTotal.WithLabelValues(c.Request.Method, route, status).Inc()
		HTTPRequestDuration.WithLabelValues(c.Request.Method, route, status).Observe(dur)
	}
}

func Handler() gin.HandlerFunc { return gin.WrapH(promhttp.Handler()) }
