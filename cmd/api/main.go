package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"meta-tracking/internal/config"
	"meta-tracking/internal/crypto"
	apihttp "meta-tracking/internal/http"
	"meta-tracking/internal/meta"
	"meta-tracking/internal/repository"
	"meta-tracking/internal/worker"

	"github.com/hibiken/asynq"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	db, err := repository.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close()

	tokenCipher, err := crypto.NewTokenCipher(cfg.TokenEncryptionKey)
	if err != nil {
		log.Fatalf("token cipher: %v", err)
	}

	store := repository.NewStore(db)
	metaClient := meta.NewClient(cfg.MetaAPIVersion).
		WithOAuth(cfg.MetaAppID, cfg.MetaAppSecret, cfg.MetaOAuthRedirect, cfg.MetaOAuthScopes)
	asynqClient := asynq.NewClient(worker.RedisClientOpt(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB))
	defer asynqClient.Close()
	server := apihttp.NewServer(store, metaClient, tokenCipher, cfg.JWTSecret, asynqClient, cfg.SyncBatchDelay, cfg.SyncBatchSize)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("api listening on %s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("api server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("api shutdown: %v", err)
	}
}
