# WhatsApp Proxy

A lightweight Go service that multiplexes a single Meta WhatsApp Business number across multiple internal applications.

Meta's Cloud API allows only one webhook URL per phone number. This proxy solves that by:

- Accepting outbound message requests from multiple apps and forwarding them to Meta
- Receiving all inbound webhooks from Meta and routing status callbacks to the correct app based on which app sent the original message
- Routing inbound messages and other Meta events to a dedicated `message_receiver` webhook

## How it works

```
┌─────────────────┐   POST /v1/messages    ┌──────────────────────┐   POST /messages
│  App A          │ ─────────────────────▶ │                      │ ────────────────▶  Meta
│  App B          │   Authorization:        │   WhatsApp Proxy     │                    Cloud API
│  App N          │   Bearer <api-key>      │                      │ ◀────────────────
└─────────────────┘                         │  ┌────────────────┐  │   webhook events
                                            │  │ Stream Worker  │  │
                   ◀─────────────────────── │  └────────────────┘  │
       app webhook   status callback routed  └──────────────────────┘
       msg_receiver  by message_id → app_id
```

**Outbound flow** (app → Meta):

1. App sends `POST /v1/messages` with its API key
2. Proxy validates the key and checks rate limits (per-app and global)
3. Request is forwarded to Meta synchronously; Meta's `message_id` is returned
4. Proxy stores `message_id → app_id` in Redis (24 h TTL)

**Inbound flow** (Meta → apps):

1. Meta sends a webhook event to the proxy's `/webhook` endpoint
2. Proxy verifies the `X-Hub-Signature-256` signature
3. Proxy responds `200` to Meta immediately
4. For **status updates**: looks up `message_id → app_id` in Redis, enqueues delivery to that app's webhook
5. For **inbound messages and all other events**: enqueues delivery to the `message_receiver` webhook
6. A background worker reads from a Redis Stream and delivers payloads, retrying up to 5 times with exponential backoff

## Prerequisites

- Docker
- Docker Compose

## Installation

```bash
git clone https://github.com/itispx/whatsapp-proxy
cd whatsapp-proxy
cp .env.example .env   # fill in your Meta credentials
docker compose -f docker-compose.prod.yml up -d
```

## Configuration

Configuration is split across two files:

- **`.env`** — Meta credentials (secrets, never committed)
- **`config.yaml`** — everything else (safe to commit)

### `.env`

```bash
META_ACCESS_TOKEN=EAABs...
META_APP_SECRET=abc123...
META_VERIFY_TOKEN=my-secret-token
META_PHONE_NUMBER_ID=123456789
```

Copy `.env.example` to `.env` and fill in the values from your Meta App Dashboard.

### `config.yaml`

```yaml
meta:
  api_version: "v25.0" # optional; defaults to v25.0

redis:
  addr: "redis:6379"
  password: "" # optional
  db: 0 # optional; defaults to 0

proxy:
  port: 8080
  global_rate: 100 # requests per minute across all apps

message_receiver:
  webhook_url: "https://your-service.internal/webhook"

apps:
  - id: "550e8400-e29b-41d4-a716-446655440000"
    name: "billing-service"
    api_key_hash: "e3b0c44298fc..." # SHA-256 of the raw API key
    webhook_url: "https://billing.internal/whatsapp/status"
    rate: 10 # per-minute limit; omit to use global_rate

  - id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
    name: "notifications-service"
    api_key_hash: "a665a45920..."
    webhook_url: "https://notifications.internal/whatsapp/status"
```

### Configuration reference

**Environment variables (`.env`)**

| Variable               | Required | Description                                              |
| ---------------------- | -------- | -------------------------------------------------------- |
| `META_ACCESS_TOKEN`    | Yes      | Meta API access token                                    |
| `META_APP_SECRET`      | Yes      | Used to verify `X-Hub-Signature-256` on inbound webhooks |
| `META_VERIFY_TOKEN`    | Yes      | Arbitrary secret used during Meta webhook verification   |
| `META_PHONE_NUMBER_ID` | Yes      | WhatsApp Business phone number ID                        |

**`config.yaml`**

| Field                          | Required | Default       | Description                                                    |
| ------------------------------ | -------- | ------------- | -------------------------------------------------------------- |
| `meta.api_version`             | No       | `v25.0`       | Meta Graph API version                                         |
| `redis.addr`                   | Yes      | —             | Redis address (`host:port`)                                    |
| `redis.password`               | No       | `""`          | Redis AUTH password                                            |
| `redis.db`                     | No       | `0`           | Redis database index                                           |
| `proxy.port`                   | No       | `8080`        | Port the proxy listens on                                      |
| `proxy.global_rate`            | Yes      | —             | Max requests per minute across all apps                        |
| `message_receiver.webhook_url` | Yes      | —             | Destination for inbound messages and non-status Meta events    |
| `apps[].id`                    | Yes      | —             | Unique identifier for this app                                 |
| `apps[].name`                  | No       | —             | Label used in log output                                       |
| `apps[].api_key_hash`          | Yes      | —             | SHA-256 hex of the raw API key given to this app               |
| `apps[].webhook_url`           | Yes      | —             | Where to deliver status callbacks for this app's messages      |
| `apps[].rate`                  | No       | `global_rate` | Per-app requests per minute; `0` or omitted uses `global_rate` |

## Provisioning an API key for an app

API keys are stored as SHA-256 hashes in `config.yaml`. The raw key is never stored anywhere.

**Step 1 — Generate a raw key:**

```bash
SECRET=$(uuidgen | tr '[:upper:]' '[:lower:]')
echo "Bearer token: $SECRET"
```

**Step 2 — Hash it:**

```bash
echo -n "$SECRET" | sha256sum | awk '{print $1}'
```

**Step 3 — Add to `config.yaml`:**

```yaml
apps:
  - id: "550e8400-e29b-41d4-a716-446655440000"
    api_key_hash: "<hash from step 2>"
    webhook_url: "https://myapp.internal/whatsapp/status"
```

**Step 4 — Give the raw key to the app owner.** They use it as their Bearer token.

## Registering the webhook with Meta

1. In the [Meta App Dashboard](https://developers.facebook.com/apps), go to **WhatsApp → Configuration → Webhook**
2. Set **Callback URL** to `https://your-proxy-domain/webhook`
3. Set **Verify Token** to the value of `META_VERIFY_TOKEN` in your `.env`
4. Subscribe to the **messages** field under `whatsapp_business_account`
5. Click **Verify and Save** — the proxy handles the hub challenge automatically
6. Subscribe your app to the WhatsApp Business Account via the Graph API:
   ```bash
   curl -X POST "https://graph.facebook.com/v25.0/{WABA_ID}/subscribed_apps" \
     -H "Authorization: Bearer $META_ACCESS_TOKEN"
   ```

> The proxy must be publicly reachable (or behind a tunnel like ngrok) before you complete this step.

## Running

```bash
docker compose -f docker-compose.prod.yml up -d
```

```bash
# View logs
docker compose -f docker-compose.prod.yml logs -f proxy

# Stop
docker compose -f docker-compose.prod.yml down
```

The proxy starts two concurrent processes: the HTTP server and the Redis Stream worker. Both shut down gracefully on `SIGINT` or `SIGTERM`, waiting up to 10 seconds for in-flight requests to complete.

## Docker

| File                            | Purpose                                                     |
| ------------------------------- | ----------------------------------------------------------- |
| `Dockerfile.prod`               | Production image (multi-stage, minimal Alpine, non-root)    |
| `Dockerfile.dev`                | Dev image (`golang:1.25-alpine` + `air` live reload)        |
| `Dockerfile.receiver`           | Lightweight webhook sink used by integration tests          |
| `docker-compose.yml`            | Dev environment with live reloading and receiver containers |
| `docker-compose.prod.yml`       | Production — pulls the published image                      |
| `docker-compose.prod-local.yml` | Overlay for testing the prod build locally                  |

Both compose files read secrets from `.env` via `env_file`.

> When running inside Docker Compose, set `redis.addr: "redis:6379"` — the service name, not `localhost`.

### Development (live reload)

`docker-compose.yml` mounts the source tree into the container. [`air`](https://github.com/air-verse/air) watches for `.go` and `.yaml` changes, rebuilds, and restarts the proxy automatically.

```bash
docker compose up --build
```

```bash
# Stop
docker compose down

# Stop and wipe volumes (Redis data)
docker compose down -v
```

### Testing the production build locally

Use `docker-compose.prod-local.yml` as an overlay. It builds the proxy from `Dockerfile.prod` and adds the receiver containers needed for integration tests:

```bash
docker compose -f docker-compose.prod.yml -f docker-compose.prod-local.yml up --build
```

Then run the integration tests as normal:

```bash
go test -count=1 ./integration/... -v
```

## Development

### 1. Fill in `.env`

```bash
cp .env.example .env
```

Edit `.env` with your Meta credentials (`META_ACCESS_TOKEN`, `META_APP_SECRET`, `META_VERIFY_TOKEN`, `META_PHONE_NUMBER_ID`).

### 2. Generate a test API key

```bash
SECRET=$(uuidgen | tr '[:upper:]' '[:lower:]')
echo "Bearer token: $SECRET"
echo -n "$SECRET" | sha256sum | awk '{print $1}'  # add this hash to config.yaml
```

### 3. Run the proxy

```bash
docker compose up --build
```

### 4. Expose the webhook to Meta (required for inbound events)

```bash
ngrok http 8080
# Forwarding: https://abc123.ngrok-free.app -> http://localhost:8080
```

Register `https://abc123.ngrok-free.app/webhook` in the Meta App Dashboard. Verify the handshake manually:

```bash
curl "http://localhost:8080/webhook?hub.mode=subscribe&hub.verify_token=YOUR_VERIFY_TOKEN&hub.challenge=test"
# Expected: test
```

### 5. Send a test message

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Authorization: Bearer $SECRET" \
  -H "Content-Type: application/json" \
  -d '{"messaging_product":"whatsapp","to":"5511999999999","type":"text","text":{"body":"dev test"}}'
```

### 6. Check health

```bash
curl http://localhost:8080/health
# ok
```

### Reading logs

```bash
docker compose logs -f proxy | jq .
```

| `msg` field                       | Meaning                                         |
| --------------------------------- | ----------------------------------------------- |
| `"message sent"`                  | Outbound message forwarded to Meta successfully |
| `"webhook delivered"`             | Status callback delivered to an app webhook     |
| `"webhook delivery failed"`       | Delivery attempt failed; worker will retry      |
| `"dead letter"`                   | All 5 retry attempts exhausted                  |
| `"no app mapping for message_id"` | Status callback arrived after 24 h TTL expired  |
| `"invalid webhook signature"`     | Inbound POST to `/webhook` failed HMAC check    |

## Prometheus integration

The proxy exposes a standard Prometheus scrape endpoint at `GET /metrics`. Add it to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: whatsapp-proxy
    static_configs:
      - targets: ["your-proxy-host:8080"]
```

Available metrics:

| Metric                                         | Type      | Labels             | Description                                                                                |
| ---------------------------------------------- | --------- | ------------------ | ------------------------------------------------------------------------------------------ |
| `whatsapp_proxy_messages_sent_total`           | Counter   | `app_id`, `status` | Outbound messages forwarded to Meta. `status`: `success`, `rate_limited`, `upstream_error` |
| `whatsapp_proxy_auth_failures_total`           | Counter   | `reason`           | Auth rejections on `POST /v1/messages`. `reason`: `missing`, `invalid`                     |
| `whatsapp_proxy_webhook_deliveries_total`      | Counter   | `app_id`, `status` | Stream worker delivery outcomes. `status`: `success`, `dead_letter`                        |
| `whatsapp_proxy_webhook_events_total`          | Counter   | `type`             | Inbound Meta events processed. `type`: `status_update`, `inbound_message`                  |
| `whatsapp_proxy_message_send_duration_seconds` | Histogram | `app_id`           | Latency of outbound Meta API calls                                                         |

Go runtime and process metrics (memory, GC, goroutines, etc.) are also included automatically by the Prometheus client.

## API reference

### Send a message

```
POST /v1/messages
Authorization: Bearer <raw-api-key>
Content-Type: application/json
```

The request body is forwarded directly to Meta's [`POST /{phone-number-id}/messages`](https://developers.facebook.com/docs/whatsapp/cloud-api/reference/messages) endpoint without modification.

**Example:**

```bash
curl -X POST https://your-proxy/v1/messages \
  -H "Authorization: Bearer 550e8400-e29b-41d4-a716-446655440000" \
  -H "Content-Type: application/json" \
  -d '{"messaging_product":"whatsapp","to":"5511999999999","type":"text","text":{"body":"Hello"}}'
```

**Error responses:**

| Status | Reason                                    |
| ------ | ----------------------------------------- |
| `401`  | Missing or invalid `Authorization` header |
| `429`  | Rate limit exceeded (per-app or global)   |
| `502`  | Meta API returned an error                |

### Webhook (Meta → proxy)

```
GET  /webhook   # hub.challenge verification
POST /webhook   # Meta event delivery
```

Called by Meta only — do not call these from your apps.

### Health check

```
GET /health  →  200 OK  "ok"
```

### Metrics

```
GET /metrics  →  200 OK  (Prometheus text format)
```

See the [Prometheus integration](#prometheus-integration) section for the full metrics reference.

## Event routing

| Meta event                                            | Routed to                                                               |
| ----------------------------------------------------- | ----------------------------------------------------------------------- |
| Status update (`sent`, `delivered`, `read`, `failed`) | App that sent the original message, looked up via `message_id` in Redis |
| Inbound message                                       | `message_receiver.webhook_url`                                          |
| Any other Meta event                                  | `message_receiver.webhook_url`                                          |

Mappings expire after **24 hours**. Status updates for expired mappings are logged as a warning and dropped.

The proxy forwards the **raw Meta payload** without modification.

## Rate limiting

Sliding window algorithm backed by Redis sorted sets, measured in **requests per minute**.

- **`global_rate`** — shared across all apps
- **`apps[].rate`** — per-app limit; when set, both the per-app and global buckets are checked

When exceeded: immediate `429`. No queue — the calling app should retry.

## Webhook delivery

| Attempt | Delay before |
| ------- | ------------ |
| 1       | immediate    |
| 2       | 1 s          |
| 3       | 2 s          |
| 4       | 4 s          |
| 5       | 8 s          |

After 5 failures the event is dead-lettered (logged, acknowledged, discarded). Your webhook must respond `200` within 10 seconds.

**Crash safety:** un-acknowledged stream entries are replayed on restart. **At-least-once delivery** — deduplicate using Meta's native `message.id` / `status.id` fields if needed.

## Redis data model

| Key                 | Type       | TTL         | Purpose                           |
| ------------------- | ---------- | ----------- | --------------------------------- |
| `rate:global`       | Sorted set | 2 min       | Global sliding window counter     |
| `rate:app:{app_id}` | Sorted set | 2 min       | Per-app sliding window counter    |
| `msg:{message_id}`  | String     | 24 h        | `message_id → app_id` routing map |
| `webhook:delivery`  | Stream     | Until ACKed | Async delivery queue              |

## Project structure

```
whatsapp-proxy/
├── cmd/
│   ├── proxy/main.go          # Entry point: wires components, starts server and worker
│   └── receiver/main.go       # Lightweight webhook sink used by integration tests
├── config/config.go           # YAML loading, env var injection, key hashing
├── handler/
│   ├── middleware.go          # Bearer auth middleware
│   ├── messages.go            # POST /v1/messages
│   ├── webhook.go             # GET+POST /webhook
│   └── util.go                # JSON error helper
├── integration/               # Black-box integration tests (against localhost:8080)
├── meta/client.go             # Meta Cloud API HTTP client
├── metrics/metrics.go         # Prometheus metric definitions
├── ratelimit/ratelimit.go     # Redis sliding window rate limiter
├── router/router.go           # message_id → app_id storage and lookup
├── stream/stream.go           # Redis Stream producer + delivery worker
├── config.yaml                # Non-secret configuration (safe to commit)
├── .env                       # Secret credentials (never committed)
└── .env.example               # Template for .env
```

## Dependencies

| Package                               | Purpose                                        |
| ------------------------------------- | ---------------------------------------------- |
| `github.com/redis/go-redis/v9`        | Redis client (Streams, sorted sets, pipeline)  |
| `github.com/prometheus/client_golang` | Prometheus metrics and `/metrics` HTTP handler |
| `github.com/joho/godotenv`            | `.env` file loading                            |
| `gopkg.in/yaml.v3`                    | YAML config parsing                            |
