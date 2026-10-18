package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	GameplayServerSecret string
	ExchangeWindow       time.Duration
}

func Load() Config {
	port := getEnv("PORT", "8080")
	dbURL := getEnv("DATABASE_URL", "postgres://lootuser:lootpass@localhost:5432/lootdb?sslmode=disable")
	secret := getEnv("GAMEPLAY_SERVER_SECRET", "trusted-gameplay-server-secret-2026")
	windowSec, err := strconv.Atoi(getEnv("EXCHANGE_WINDOW_SECONDS", "300"))
	if err != nil || windowSec <= 0 {
		windowSec = 300
	}

	return Config{
		Port:                 port,
		DatabaseURL:          dbURL,
		GameplayServerSecret: secret,
		ExchangeWindow:       time.Duration(windowSec) * time.Second,
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}