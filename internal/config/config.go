package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
	_ "time/tzdata"
)

type Config struct {
	AppEnv             string
	HTTPAddr           string
	DatabaseURL        string
	RedisAddr          string
	RedisPassword      string
	RedisDB            int
	JWTSecret          string
	TokenEncryptionKey string
	MetaAPIVersion     string
	MetaAppID          string
	MetaAppSecret      string
	MetaOAuthRedirect  string
	MetaOAuthScopes    string
	TelegramBotToken   string
	TelegramChatID     string
	SyncInterval       time.Duration
	SyncBatchSize      int
	SyncBatchDelay     time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		AppEnv:             getenv("APP_ENV", "local"),
		HTTPAddr:           getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		RedisAddr:          getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:      os.Getenv("REDIS_PASSWORD"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		MetaAPIVersion:     getenv("META_API_VERSION", "v25.0"),
		MetaAppID:          os.Getenv("META_APP_ID"),
		MetaAppSecret:      os.Getenv("META_APP_SECRET"),
		MetaOAuthRedirect:  os.Getenv("META_OAUTH_REDIRECT_URI"),
		MetaOAuthScopes:    getenv("META_OAUTH_SCOPES", "ads_read"),
		TelegramBotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:     os.Getenv("TELEGRAM_CHAT_ID"),
		TokenEncryptionKey: os.Getenv("TOKEN_ENCRYPTION_KEY"),
	}

	// SYNC_INTERVAL controls how often ad-account snapshots are taken
	// (e.g. "5m" while debugging). Jitter and task-uniqueness windows are
	// derived from it, so the default 1h keeps the 5-10 min jitter behavior.
	cfg.SyncInterval = time.Hour
	if raw := os.Getenv("SYNC_INTERVAL"); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse SYNC_INTERVAL: %w", err)
		}
		if interval < time.Minute {
			return Config{}, fmt.Errorf("SYNC_INTERVAL must be at least 1m")
		}
		cfg.SyncInterval = interval
	}
	cfg.SyncBatchSize = 60
	if raw := os.Getenv("SYNC_BATCH_SIZE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse SYNC_BATCH_SIZE: %w", err)
		}
		if n < 1 {
			return Config{}, fmt.Errorf("SYNC_BATCH_SIZE must be at least 1")
		}
		cfg.SyncBatchSize = n
	}
	cfg.SyncBatchDelay = 10 * time.Minute
	if raw := os.Getenv("SYNC_BATCH_DELAY"); raw != "" {
		delay, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse SYNC_BATCH_DELAY: %w", err)
		}
		if delay < time.Minute {
			return Config{}, fmt.Errorf("SYNC_BATCH_DELAY must be at least 1m")
		}
		cfg.SyncBatchDelay = delay
	}

	if db := os.Getenv("REDIS_DB"); db != "" {
		n, err := strconv.Atoi(db)
		if err != nil {
			return Config{}, fmt.Errorf("parse REDIS_DB: %w", err)
		}
		cfg.RedisDB = n
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return Config{}, fmt.Errorf("JWT_SECRET is required")
	}
	if cfg.TokenEncryptionKey == "" {
		return Config{}, fmt.Errorf("TOKEN_ENCRYPTION_KEY is required")
	}
	if cfg.MetaAppID == "" {
		return Config{}, fmt.Errorf("META_APP_ID is required")
	}
	if cfg.MetaAppSecret == "" {
		return Config{}, fmt.Errorf("META_APP_SECRET is required")
	}
	if cfg.MetaOAuthRedirect == "" {
		return Config{}, fmt.Errorf("META_OAUTH_REDIRECT_URI is required")
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
