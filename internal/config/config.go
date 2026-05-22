package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr             string
	DataDir          string
	WorkerWebhookURL string
	ServiceToken     string
	DeviceOSName     string
	SessionStore     string
	DatabaseURL      string
	DatabaseSchema   string
	InstanceID       string
	LeaseTTL         time.Duration
	LeaseHeartbeat   time.Duration
	LeaseStaleGrace  time.Duration
}

func Load() Config {
	return Config{
		Addr:             env("ADDR", ":8080"),
		DataDir:          env("DATA_DIR", "./data"),
		WorkerWebhookURL: env("WORKER_WEBHOOK_URL", ""),
		ServiceToken:     env("SERVICE_TOKEN", ""),
		DeviceOSName:     env("WHATSAPP_DEVICE_OS_NAME", "KizeLabs WhatERS"),
		SessionStore:     strings.ToLower(env("SESSION_STORE_DRIVER", "sqlite")),
		DatabaseURL:      env("DATABASE_URL", ""),
		DatabaseSchema:   env("DATABASE_SCHEMA", "public"),
		InstanceID:       env("INSTANCE_ID", ""),
		LeaseTTL:         time.Duration(envInt("LEASE_TTL_SECONDS", 30)) * time.Second,
		LeaseHeartbeat:   time.Duration(envInt("LEASE_HEARTBEAT_SECONDS", 10)) * time.Second,
		LeaseStaleGrace:  time.Duration(envInt("LEASE_STALE_GRACE_SECONDS", 5)) * time.Second,
	}
}

func (c Config) IsPostgresStore() bool {
	return c.SessionStore == "postgres"
}

func env(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
