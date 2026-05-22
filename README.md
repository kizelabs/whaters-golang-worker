# WhatsApp Endpoint Routing Services

Dockerized WhatsMeow HTTP service for KizeLabs WhatERS with HighAvailability Mode supported.

## Data Scope

This service is a routing gateway, not a private conversation archive.

- It forwards inbound events to your configured `WORKER_WEBHOOK_URL`.
- It does not provide app-level storage for your private/business chat history.
- Persistent data in this service is operational only (session/auth state and HA coordination metadata).

## Setup

Copy the environment template:

```bash
cp .env.example .env
```

Fill:

```bash
WORKER_WEBHOOK_URL=https://<your-worker.workers.dev>/webhooks/whatsapp
SERVICE_TOKEN=the-same-service-token-used-by-cloudflare-worker
```

For HA mode with Neon/Supabase/Postgres:

```bash
SESSION_STORE_DRIVER=postgres
DATABASE_URL=<pooled-url>
LEASE_TTL_SECONDS=30
LEASE_HEARTBEAT_SECONDS=10
LEASE_STALE_GRACE_SECONDS=5
```

`INSTANCE_ID` is set per instance by compose using:
- `WA1_INSTANCE_ID`
- `WA2_INSTANCE_ID`

Run:

```bash
docker compose up -d --build
```

This compose runs two containers:
- `wa1` on `WA1_HOST_PORT` (default `18081`)
- `wa2` on `WA2_HOST_PORT` (default `18082`)

Important:
- Multi-instance mode requires `SESSION_STORE_DRIVER=postgres` (shared store + lease coordination).
- SQLite should be used in single-instance mode only. Running sqlite with two load-balanced instances causes split local state and unstable session/QR behavior.

Data volumes are env-driven:
- `WA1_DATA_DIR` (default `./data/wa1`)
- `WA2_DATA_DIR` (default `./data/wa2`)

## Public URL

Cloudflare Worker must be able to reach this service over HTTPS for:

- `GET /health`
- `POST /sessions/{sessionId}`
- `GET /sessions/{sessionId}/qr`
- `POST /sessions/{sessionId}/logout`
- `POST /messages/send`

Recommended: put Caddy in front and expose one URL, for example `https://wa.yourdomain.com`, then set Worker `WA_SERVICE_URL` to that endpoint.

Example Caddy config:

```caddy
wa.yourdomain.com {
  encode gzip zstd

  reverse_proxy 127.0.0.1:18081 127.0.0.1:18082 {
    lb_policy round_robin
    health_uri /health
    health_interval 10s
    health_timeout 3s
  }
}
```

## Persistence

- If `SESSION_STORE_DRIVER=sqlite`, session DB files are in:
  - `WA1_DATA_DIR`
  - `WA2_DATA_DIR`
- If `SESSION_STORE_DRIVER=postgres`, lease coordination and session-device mapping are stored in Postgres.

Back up local data dirs when using SQLite fallback.

## Auth

All private service routes require:

```http
X-Service-Token: <SERVICE_TOKEN>
```
