package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/handler"
	"github.com/itispx/whatsapp-proxy/meta"
	"github.com/itispx/whatsapp-proxy/metrics"
	"github.com/itispx/whatsapp-proxy/ratelimit"
	"github.com/itispx/whatsapp-proxy/router"
	"github.com/itispx/whatsapp-proxy/stream"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// Load .env into the process environment. No-op if the file doesn't exist
	// (e.g. when env vars are injected by Docker Compose).
	godotenv.Load(".env") //nolint:errcheck

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Error("redis connection failed", "err", err)
		os.Exit(1)
	}

	m := metrics.New(prometheus.DefaultRegisterer)

	metaClient := meta.NewClient(cfg.Meta.AccessToken, cfg.Meta.PhoneNumberID, cfg.Meta.APIVersion, log)
	limiter := ratelimit.New(rdb)
	rtr := router.New(rdb, log)
	producer := stream.NewProducer(rdb)
	worker := stream.NewWorker(rdb, cfg, m, log)

	mux := http.NewServeMux()

	messagesHandler := handler.NewMessages(cfg, metaClient, limiter, rtr, m, log)
	mux.Handle("POST /v1/messages",
		handler.Auth(cfg, log, m, messagesHandler),
	)

	webhookHandler := handler.NewWebhook(cfg, rtr, producer, m, log)
	mux.Handle("/webhook", webhookHandler)

	mux.Handle("GET /metrics", promhttp.Handler())

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Proxy.Port),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start stream worker in background.
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- worker.Start(ctx)
	}()

	// Start HTTP server in background.
	serverDone := make(chan error, 1)
	go func() {
		log.Info("server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverDone <- err
		}
		close(serverDone)
	}()

	// Wait for shutdown signal or a fatal error.
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serverDone:
		log.Error("server error", "err", err)
	case err := <-workerDone:
		log.Error("worker error", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown error", "err", err)
	}

	<-workerDone
	log.Info("shutdown complete")
}
