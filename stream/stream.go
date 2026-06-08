package stream

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/metrics"
)

const (
	streamKey       = "webhook:delivery"
	consumerGroup   = "proxy-workers"
	consumerName    = "worker-1"
	messageReceiver = "message_receiver"
	maxRetries      = 5
	blockTimeout    = 5 * time.Second
)

// Producer pushes webhook delivery events onto the Redis Stream.
type Producer struct {
	rdb *redis.Client
}

func NewProducer(rdb *redis.Client) *Producer {
	return &Producer{rdb: rdb}
}

// Enqueue adds a delivery task to the stream keyed by appID.
// Use the sentinel value "message_receiver" for the message receiver webhook.
func (p *Producer) Enqueue(ctx context.Context, appID string, payload []byte) error {
	err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{
			"app_id":  appID,
			"payload": payload,
		},
	}).Err()
	if err != nil {
		return fmt.Errorf("xadd: %w", err)
	}
	return nil
}

// Worker reads from the Redis Stream and delivers payloads to app webhooks.
type Worker struct {
	rdb        *redis.Client
	cfg        *config.Config
	httpClient *http.Client
	metrics    *metrics.Metrics
	log        *slog.Logger
}

func NewWorker(rdb *redis.Client, cfg *config.Config, m *metrics.Metrics, log *slog.Logger) *Worker {
	return &Worker{
		rdb:        rdb,
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		metrics:    m,
		log:        log,
	}
}

// Start initialises the consumer group and begins processing in a blocking loop.
// It returns when ctx is cancelled.
func (w *Worker) Start(ctx context.Context) error {
	err := w.rdb.XGroupCreateMkStream(ctx, streamKey, consumerGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create consumer group: %w", err)
	}

	w.log.Info("stream worker started")

	w.log.Debug("worker: checking for pending un-acked messages")
	w.recoverPending(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		msgs, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    consumerGroup,
			Consumer: consumerName,
			Streams:  []string{streamKey, ">"},
			Count:    10,
			Block:    blockTimeout,
		}).Result()

		if err != nil {
			if err == redis.Nil || err == context.DeadlineExceeded {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			w.log.Error("xreadgroup error", "err", err)
			time.Sleep(time.Second)
			continue
		}

		for _, stream := range msgs {
			for _, msg := range stream.Messages {
				w.log.Debug("worker: new message received from stream", "stream_id", msg.ID)
				w.process(ctx, msg)
			}
		}
	}
}

func (w *Worker) recoverPending(ctx context.Context) {
	msgs, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    consumerGroup,
		Consumer: consumerName,
		Streams:  []string{streamKey, "0"},
		Count:    100,
	}).Result()
	if err != nil && err != redis.Nil {
		w.log.Error("recover pending error", "err", err)
		return
	}
	for _, stream := range msgs {
		for _, msg := range stream.Messages {
			w.log.Debug("worker: replaying pending message", "stream_id", msg.ID)
			w.process(ctx, msg)
		}
	}
}

func (w *Worker) process(ctx context.Context, msg redis.XMessage) {
	appID, _ := msg.Values["app_id"].(string)
	payload, _ := msg.Values["payload"].(string)

	w.log.Debug("worker: processing stream entry", "stream_id", msg.ID, "app_id", appID)

	webhookURL, ok := w.resolveURL(appID)
	if !ok {
		w.log.Warn("app not found in config, dropping stream entry", "app_id", appID, "stream_id", msg.ID)
	} else {
		w.log.Debug("worker: resolved webhook url", "app_id", appID, "webhook_url", webhookURL)
		if err := w.deliver(ctx, webhookURL, []byte(payload), appID, msg.ID); err != nil {
			w.metrics.WebhookDeliveries.WithLabelValues(appID, "dead_letter").Inc()
			w.log.Error("dead letter",
				"stream_id", msg.ID,
				"app_id", appID,
				"webhook_url", webhookURL,
				"err", err,
			)
		} else {
			w.metrics.WebhookDeliveries.WithLabelValues(appID, "success").Inc()
		}
	}

	w.log.Debug("worker: acking stream entry", "stream_id", msg.ID)
	if err := w.rdb.XAck(ctx, streamKey, consumerGroup, msg.ID).Err(); err != nil {
		w.log.Error("xack error", "stream_id", msg.ID, "err", err)
	}
}

func (w *Worker) resolveURL(appID string) (string, bool) {
	if appID == messageReceiver {
		return w.cfg.MessageReceiver.WebhookURL, true
	}
	app, ok := w.cfg.AppByID(appID)
	if !ok {
		return "", false
	}
	return app.WebhookURL, true
}

func (w *Worker) deliver(ctx context.Context, webhookURL string, payload []byte, appID, streamID string) error {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during backoff")
			}
		}

		err := w.post(ctx, webhookURL, payload)
		if err == nil {
			w.log.Info("webhook delivered",
				"stream_id", streamID,
				"app_id", appID,
				"webhook_url", webhookURL,
				"attempt", attempt+1,
			)
			return nil
		}

		lastErr = err
		w.log.Warn("webhook delivery failed",
			"stream_id", streamID,
			"app_id", appID,
			"webhook_url", webhookURL,
			"attempt", attempt+1,
			"err", err,
		)
	}

	return fmt.Errorf("all %d attempts failed, last error: %w", maxRetries, lastErr)
}

func (w *Worker) post(ctx context.Context, webhookURL string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}
