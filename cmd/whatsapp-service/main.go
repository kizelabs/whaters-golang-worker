package main

import (
	"log/slog"
	"net/http"
	"os"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"wago-worker/whatsapp-service/internal/app"
	"wago-worker/whatsapp-service/internal/config"
)

func main() {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		panic(err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	server, err := app.New(cfg, logger)
	if err != nil {
		panic(err)
	}

	logger.Info("starting whatsapp service", "addr", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, server.Handler()); err != nil {
		panic(err)
	}
}
