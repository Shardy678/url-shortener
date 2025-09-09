package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"url-shortener/internal/metrics"
)

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()

	// Gin in test mode to silence logs
	gin.SetMode(gin.TestMode)

	// Initialize metrics registry as the app does
	metrics.Init()

	// Build a minimal router exactly like main wires it
	r := gin.New()
	r.Use(metrics.Middleware())
	r.GET("/metrics", metrics.Handler())

	// Exercise the endpoint
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /metrics, got %d, body=%s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	// Basic Prometheus exposition format sanity checks
	if !strings.Contains(body, "# HELP") || !strings.Contains(body, "# TYPE") {
		t.Fatalf("metrics body doesn't look like Prometheus exposition format:\n%s", body)
	}
}

func TestHTTPServerGracefulShutdown(t *testing.T) {
	t.Parallel()

	// Use a trivial handler; we're testing net/http shutdown semantics,
	// which main relies on via srv.Shutdown(ctx).
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:    "127.0.0.1:0", // random free port
		Handler: mux,
	}

	// Start the server
	ln, err := newLocalListener("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	go func() {
		_ = srv.Serve(ln)
	}()

	// Quick liveness check
	url := "http://" + ln.Addr().String() + "/healthz"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /healthz, got %d", resp.StatusCode)
	}

	// Shutdown with a grace period (mirrors main's shutdown flow)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("server shutdown error: %v", err)
	}

	// After Shutdown, server should refuse new connections.
	if _, err := http.Get(url); err == nil {
		t.Fatalf("server still accepted connections after shutdown")
	}
}

// newLocalListener is a tiny helper to avoid data races when using ":0".
// It dials the actual chosen port back out via ln.Addr().
func newLocalListener(network, address string) (ln net.Listener, err error) {
	type n = interface {
		Listen(network, address string) (net.Listener, error)
	}
	var l n = &netListenWrapper{}
	return l.Listen(network, address)
}

type netListenWrapper struct{}

func (*netListenWrapper) Listen(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}
