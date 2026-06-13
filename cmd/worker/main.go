package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"meta-tracking/internal/config"
	"meta-tracking/internal/crypto"
	"meta-tracking/internal/meta"
	"meta-tracking/internal/repository"
	"meta-tracking/internal/telegram"
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
	redisOpt := worker.RedisClientOpt(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	metaClient := meta.NewClient(cfg.MetaAPIVersion)
	telegramClient := telegram.NewClient(cfg.TelegramBotToken, cfg.TelegramChatID)

	mode := os.Getenv("WORKER_MODE")
	if mode == "" {
		mode = "all"
	}

	if mode == "scheduler" || mode == "all" {
		client := asynq.NewClient(redisOpt)
		defer client.Close()
		scheduler := worker.NewScheduler(store, client, telegramClient, cfg.SyncBatchDelay)
		go func() {
			if err := scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("scheduler stopped: %v", err)
			}
		}()
	}

	if mode == "server" || mode == "all" {
		server := asynq.NewServer(redisOpt, asynq.Config{Concurrency: 5})
		mux := asynq.NewServeMux()
		processor := worker.NewProcessor(store, metaClient, tokenCipher, telegramClient, cfg.SyncBatchSize)
		processor.Register(mux)
		go func() {
			if err := server.Run(mux); err != nil {
				log.Printf("worker server stopped: %v", err)
			}
		}()
		<-ctx.Done()
		server.Shutdown()
		return
	}

	<-ctx.Done()
}
