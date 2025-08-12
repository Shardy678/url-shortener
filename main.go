package main

import (
	"crypto/rand"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	uaParser "github.com/mssola/user_agent"
)

var (
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	store    = newMemStore()
	events   = newAnalyticsStore()
)

type memStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func newMemStore() *memStore {
	return &memStore{
		data: map[string]string{},
	}
}

func (m *memStore) get(code string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[code]
	return v, ok
}

func (m *memStore) set(code, url string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.data[code]; exists {
		return false
	}
	m.data[code] = url
	return true
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

/* -------------------- server -------------------- */

func main() {
	port := getenv("PORT", "8080")
	baseURL := getenv("BASE_URL", "http://localhost:"+port)
	idLen := 7

	r := gin.Default()
	r.POST("/api/shorten", func(c *gin.Context) {
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

		if ok := store.set(code, req.URL); !ok {
			c.JSON(http.StatusConflict, gin.H{"error": "slug already exists"})
			return
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

		url, exists := store.get(code)
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}

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
	})

	_ = r.Run(":" + port)
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
