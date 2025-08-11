package main

import (
	"crypto/rand"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

var (
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	store    = newMemStore()
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

	r.GET("/:code", func(c *gin.Context) {
		code := c.Param("code")

		url, exists := store.get(code)
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
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
