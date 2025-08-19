package util

import (
	"crypto/rand"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"

	uaParser "github.com/mssola/user_agent"
)

var alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func GetClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			if ip := strings.TrimSpace(part); ip != "" {
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

func ParseUA(uaStr string) (device, os, browser string) {
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

func GenID(n int) string {
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(62))
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

func AtoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}
