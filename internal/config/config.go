package config

import (
	"os"
	"time"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	TokenTTL    time.Duration
}

func Load() Config {
	return Config{
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		DatabaseURL: getenv("DATABASE_URL", "postgres://social:social@localhost:5432/social?sslmode=disable"),
		TokenTTL:    24 * time.Hour,
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
