package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
)

const (
	defaultPort         = "8080"
	readTimeout         = 5 * time.Second
	writeTimeout        = 15 * time.Second
	idleTimeout         = 15 * time.Second
	shutdownGracePeriod = 15 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log := newLogger()

	project := mustEnv("GCP_PROJECT")
	topicName := mustEnv("PUBSUB_TOPIC")
	port := envOr("COLLECTOR_PORT", defaultPort)
	redisAddr := envOr("REDIS_ADDR", "")

	writeKeysSecret := os.Getenv("WRITE_KEYS_SECRET")
	writeKeysInline := os.Getenv("WRITE_KEYS_PLAINTEXT")
	if writeKeysSecret == "" && writeKeysInline == "" {
		return fmt.Errorf("set WRITE_KEYS_SECRET (production, Secret Manager) or WRITE_KEYS_PLAINTEXT (dev only)")
	}

	validator, err := NewValidator()
	if err != nil {
		return fmt.Errorf("validator: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var keyMgr *KeyManager
	if writeKeysInline != "" {
		keyMgr, err = NewKeyManagerStatic(writeKeysInline, log)
	} else {
		keyMgr, err = NewKeyManager(ctx, writeKeysSecret, log)
	}
	if err != nil {
		return fmt.Errorf("write keys: %w", err)
	}
	go keyMgr.Run(ctx)

	psClient, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return fmt.Errorf("pubsub client: %w", err)
	}
	defer psClient.Close()

	topic := psClient.Topic(topicName)
	exists, err := topic.Exists(ctx)
	if err != nil {
		return fmt.Errorf("check topic exists: %w", err)
	}
	if !exists {
		return fmt.Errorf("pubsub topic %q does not exist in project %q (run scripts/bootstrap-gcp.sh first)", topicName, project)
	}
	topic.EnableMessageOrdering = true
	topic.PublishSettings = pubsub.PublishSettings{
		DelayThreshold: 50 * time.Millisecond,
		CountThreshold: 100,
		ByteThreshold:  1 << 20,
		NumGoroutines:  4,
		Timeout:        10 * time.Second,
		FlowControlSettings: pubsub.FlowControlSettings{
			MaxOutstandingMessages: 10000,
			MaxOutstandingBytes:    256 << 20,
			LimitExceededBehavior:  pubsub.FlowControlBlock,
		},
	}
	defer topic.Stop()

	var idem IdempStore = NewRedisIdem(nil)
	if redisAddr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			log.Warn("redis ping failed; idempotency disabled", "err", err)
		} else {
			idem = NewRedisIdem(rdb)
			log.Info("redis connected", "addr", redisAddr)
		}
		cancel()
	} else {
		log.Warn("REDIS_ADDR not set; idempotency disabled")
	}

	srv := NewServer(validator, topic, idem, keyMgr, log)

	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	r.Use(recoverMiddleware(log))
	r.Use(loggingMiddleware(log))
	r.Get("/healthz", srv.handleHealth)
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware(keyMgr))
		r.Post("/v1/events", srv.handleEvents)
	})

	httpSrv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("collector listening", "port", port, "topic", topicName, "project", project)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		return fmt.Errorf("listen: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
	return nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(h).With("svc", "collector")
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required env: %s\n", name)
		os.Exit(2)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
