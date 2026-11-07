package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr             string
	DatabaseURL          string
	DatabaseReplicaURLs  []string
	TokenTTL             time.Duration
	DBMaxConns           int32
	LogRequests          bool
	ReadFallbackToMaster bool
	EnableTestAPI        bool
}

func Load() Config {
	return Config{
		HTTPAddr:             getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:          getenv("DATABASE_URL", "postgres://social:social@localhost:5432/social?sslmode=disable"),
		DatabaseReplicaURLs:  splitURLs(getenv("DATABASE_REPLICA_URLS", "")),
		TokenTTL:             24 * time.Hour,
		DBMaxConns:           int32(getenvInt("DB_MAX_CONNS", 16)),
		LogRequests:          getenvBool("LOG_REQUESTS", true),
		ReadFallbackToMaster: getenvBool("READ_FALLBACK_TO_MASTER", true),
		EnableTestAPI:        getenvBool("ENABLE_TEST_API", false),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func getenvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// splitURLs parses a comma-separated list of DSNs, dropping empty entries.
func splitURLs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	urls := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			urls = append(urls, p)
		}
	}
	return urls
}
