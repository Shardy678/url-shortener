package main

import (
	"fmt"
	"os"
)

func main() {
	cfg := loadConfig()
	fmt.Println(cfg)
}

type Config struct {
	Port string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	return Config{
		Port: env("PORT", "8080"),
	}
}
