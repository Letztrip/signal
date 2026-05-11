# Signal — event analytics pipeline

A small Go ingestion service that captures user events over HTTPS, validates
them, scrubs PII, flattens them into a Pub/Sub Protobuf message, and lets a
native Pub/Sub → BigQuery subscription write rows straight to
`analytics.events`. No subscriber service. No emulators.

## 1. Architecture

```
clients ─┐
         ▼
    ┌──────────────┐    ┌──────────┐
    │  collector   │◄──►│  redis   │  (idempotency cache, 24h TTL)
    │  (Go, on VM) │    └──────────┘
    └──────┬───────┘
           ▼
   ┌───────────────┐
   │  Pub/Sub      │  topic with registered Protobuf schema
   │  topic        │
   └───────┬───────┘
           ▼
   ┌───────────────┐
   │  Pub/Sub BQ   │  native subscription, "use topic schema"
   │  subscription │
   └───────┬───────┘
           ▼
   ┌───────────────┐
   │   BigQuery    │  analytics.events  (DAY partition + clustering)
   └───────────────┘
```

## 2. Repo layout

| Path                  | Purpose                                                        |
| --------------------- | -------------------------------------------------------------- |
| `schemas/event.proto` | Pub/Sub topic schema (Protobuf 3).                             |
| `schemas/events.v1.json` | JSON Schema the collector validates incoming requests against. |
| `collector/`          | Go service. JSON → flatten → Protobuf → Pub/Sub.               |
| `collector/eventpb/`  | Generated Go bindings for the Protobuf wire format.            |
| `scripts/`            | `bootstrap-gcp.sh`, `create-service-account.sh`, `mint-write-key.sh`, `bq-schema.json` |
| `docker-compose.yml`  | Runs collector + Redis on a single VM.                         |
| `Makefile`            | `make proto`, `make up`, `make logs`, `make bq-count`, …       |

## 3. Provisioning the cloud

Two scripts. The first creates a service account and downloads its key.
The second creates every Pub/Sub / BigQuery / Secret Manager resource
the pipeline needs and grants the SA its three roles.

```sh
./scripts/create-service-account.sh
./scripts/bootstrap-gcp.sh
```

Both auto-resolve project, region, and VPC from `gcloud` and fall back
to the values pinned for `letztrip-production-account`. To override:

```sh
PROJECT=other-project REGION=us-east1 ./scripts/bootstrap-gcp.sh
```

The bootstrap is idempotent — re-running on existing resources is safe.

## 4. First-time write key

```sh
brew install argon2                       # macOS — Debian: apt install argon2
./scripts/mint-write-key.sh demo-app prod
```

Two outputs: the plaintext key (give to the client; store nowhere) and a
storage line. Append the line to the secret:

```sh
echo 'demo-app:$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>' | \
  gcloud secrets versions add write-keys --data-file=-
```

The collector points at `versions/latest` and refreshes every 5 minutes
via an atomic pointer swap, so rotation is just "add a new version."

## 5. Deploying the collector to a VM

```sh
# 1. From your laptop, copy the SA key produced by create-service-account.sh:
scp collector-vm-key.json USER@VM_IP:/tmp/key.json

# 2. On the VM:
sudo mkdir -p /etc/collector
sudo mv /tmp/key.json /etc/collector/key.json
sudo chown $USER:$USER /etc/collector/key.json
sudo chmod 600 /etc/collector/key.json

# 3. Clone this repo on the VM, then:
export GCP_PROJECT=letztrip-production-account
export PUBSUB_TOPIC=events-raw
export WRITE_KEYS_SECRET=projects/$GCP_PROJECT/secrets/write-keys/versions/latest
export GOOGLE_APPLICATION_CREDENTIALS_HOST=/etc/collector/key.json

docker compose up --build -d
docker compose logs -f collector

# 4. Back on your laptop, scrub the local copy of the key:
rm -P collector-vm-key.json   # macOS
# shred -u collector-vm-key.json   # Linux
```

To roll a new image: `git pull && docker compose up -d --build` on the VM.

## 6. Smoke test

```sh
curl -sf http://VM_IP:8080/healthz && echo

curl -sS -X POST http://VM_IP:8080/v1/events \
  -H 'Content-Type: application/json' \
  -H "X-Write-Key: <plaintext key from §4>" \
  -H "Idempotency-Key: $(uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid)" \
  -d '{"batch":[{
    "event_id":"e_1","event_name":"page_viewed",
    "anonymous_id":"a_1","session_id":"s_1",
    "client_ts":"2026-05-09T12:00:00Z",
    "context":{"platform":"web","app_version":"1.0.0","sdk_version":"0.1.0"}
  }]}'

# Verify it landed:
GCP_PROJECT=letztrip-production-account make bq-count
```

## 7. HTTP contract

`POST /v1/events` accepts up to 100 events and 1 MB per request:

```json
{ "batch": [ { "event_id": "...", "event_name": "...", ... } ] }
```

Required headers: `X-Write-Key`, `Content-Type: application/json`.
Optional: `Idempotency-Key` (UUID; repeats within 24h replay the cached
202).

`GET /healthz` returns 200, no auth.

| Status | Code                  | Meaning                                |
| ------ | --------------------- | -------------------------------------- |
| 202    | —                     | Accepted into publisher buffer.        |
| 400    | `invalid_json`        | Body not parseable.                    |
| 400    | `invalid_batch_size`  | 0 or >100 events.                      |
| 400    | `schema_violation`    | An event failed JSON Schema.           |
| 400    | `pii_violation`       | A property value matched a PII regex.  |
| 401    | `invalid_write_key`   | Bad or missing `X-Write-Key`.          |
| 413    | `batch_too_large`     | Body exceeded 1 MB.                    |
| 503    | `publisher_saturated` | Pub/Sub flow control blocked.          |
| 5xx    | `internal`            | Anything else.                         |

Error body: `{"error":{"code":"...","message":"...","request_id":"..."}}`.

## 8. Adding a new event type

1. Add the value to the `event_name` enum in `schemas/events.v1.json`.
2. Ship the collector.

The Protobuf and BQ table do not change; `event_name` is a string column.

## 9. Adding a new column

1. Append a new field with a fresh tag number to `schemas/event.proto`
   (never reuse, never renumber).
2. `make proto`.
3. Populate the field in `collector/server.go`'s `buildProto`.
4. Append the matching nullable column to `scripts/bq-schema.json` and
   apply: `bq update analytics.events scripts/bq-schema.json`.
5. `git pull && docker compose up -d --build` on the VM.

Old rows have NULL for the new column; the BQ subscription's
`drop_unknown_fields = true` keeps everything moving during the rollout.

## 10. Operations

**Inspect the dead-letter queue:**

```sh
gcloud pubsub subscriptions pull events-raw-dlq-inspection \
  --auto-ack --limit=10 --format=json
```

**Alert thresholds** (start here, tighten with traffic):
- Collector 5xx rate > 0.5% sustained for 5 min.
- BQ subscription unacked message age > 5 min.
- DLQ message count > 0 on a 1-min window.

## 11. Event examples

Realistic JSON for each `event_name` in the schema, plus a full batch and a
single-event POST. All examples validate against `schemas/events.v1.json`
and pass the PII deny-list (no emails, phones, card-shaped strings, or
SSNs in `properties`).

### 11.1 `page_viewed` (web, anonymous user)

```json
{
  "event_id": "8f3b9c2a-1d4e-4a7f-b9c2-1a2b3c4d5e6f",
  "event_name": "page_viewed",
  "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
  "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
  "client_ts": "2026-05-09T10:23:45.123Z",
  "properties": {
    "name": "/products/mens-running-shoes",
    "title": "Men's Running Shoes - Letztrip",
    "category": "footwear",
    "subcategory": "running",
    "products_listed": 24,
    "filters_applied": ["size:10", "color:black"]
  },
  "context": {
    "platform": "web",
    "app_version": "1.4.2",
    "sdk_version": "0.1.0",
    "locale": "en-IN",
    "timezone": "Asia/Kolkata",
    "screen": { "width": 1920, "height": 1080 },
    "page": {
      "url": "https://letztrip.com/products/mens-running-shoes",
      "path": "/products/mens-running-shoes",
      "referrer": "https://letztrip.com/category/footwear",
      "title": "Men's Running Shoes - Letztrip"
    }
  }
}
```

### 11.2 `button_clicked` (web, identified user)

```json
{
  "event_id": "1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d",
  "event_name": "button_clicked",
  "user_id": "u_42819",
  "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
  "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
  "client_ts": "2026-05-09T10:24:12.547Z",
  "properties": {
    "button_id": "add-to-cart",
    "button_text": "Add to Cart",
    "product_id": "PRD-7821",
    "product_sku": "SHOE-NIKE-AIR-10-BLK",
    "price_cents": 549900,
    "currency": "INR",
    "quantity": 1,
    "position": "above_fold",
    "ab_variant": "control"
  },
  "context": {
    "platform": "web",
    "app_version": "1.4.2",
    "sdk_version": "0.1.0",
    "locale": "en-IN",
    "timezone": "Asia/Kolkata",
    "screen": { "width": 1920, "height": 1080 },
    "page": {
      "url": "https://letztrip.com/products/PRD-7821",
      "path": "/products/PRD-7821",
      "referrer": "https://letztrip.com/products/mens-running-shoes",
      "title": "Nike Air 10 Black - Letztrip"
    }
  }
}
```

### 11.3 `form_submitted` (checkout flow, no PII in properties)

```json
{
  "event_id": "9e8d7c6b-5a4f-3e2d-1c0b-9a8f7e6d5c4b",
  "event_name": "form_submitted",
  "user_id": "u_42819",
  "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
  "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
  "client_ts": "2026-05-09T10:31:08.892Z",
  "properties": {
    "form_id": "checkout-shipping",
    "form_name": "Shipping Address",
    "step": 2,
    "total_steps": 4,
    "duration_ms": 47230,
    "fields_count": 7,
    "validation_errors": 0,
    "saved_for_later": false,
    "country": "IN",
    "state": "KA",
    "pincode_prefix": "560",
    "shipping_method": "standard"
  },
  "context": {
    "platform": "web",
    "app_version": "1.4.2",
    "sdk_version": "0.1.0",
    "locale": "en-IN",
    "timezone": "Asia/Kolkata",
    "page": {
      "url": "https://letztrip.com/checkout/shipping",
      "path": "/checkout/shipping",
      "referrer": "https://letztrip.com/cart",
      "title": "Shipping - Letztrip Checkout"
    }
  }
}
```

> Note: `pincode_prefix` is `"560"`, not the full pincode. Full Indian
> pincodes are 6 digits and won't match the phone regex (10–15), but
> storing only the area prefix is the privacy-conscious convention.
> Don't put email or full phone in `properties` — the PII scanner
> rejects the whole batch.

### 11.4 `identify` (login, with traits)

```json
{
  "event_id": "0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d",
  "event_name": "identify",
  "user_id": "u_42819",
  "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
  "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
  "client_ts": "2026-05-09T10:20:01.000Z",
  "properties": {
    "plan": "pro",
    "signup_source": "google_oauth",
    "loyalty_tier": "gold",
    "lifetime_orders": 27,
    "preferred_categories": ["footwear", "outdoors"],
    "marketing_opt_in": true,
    "country": "IN",
    "city": "Bengaluru"
  },
  "context": {
    "platform": "web",
    "app_version": "1.4.2",
    "sdk_version": "0.1.0",
    "locale": "en-IN",
    "timezone": "Asia/Kolkata"
  }
}
```

### 11.5 `session_started` (Flutter iOS, deep link)

```json
{
  "event_id": "3c4d5e6f-7a8b-9c0d-1e2f-3a4b5c6d7e8f",
  "event_name": "session_started",
  "anonymous_id": "a_8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e",
  "session_id": "s_1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f",
  "client_ts": "2026-05-09T11:02:33.421Z",
  "properties": {
    "entry_source": "deep_link",
    "deep_link_path": "/products/PRD-7821",
    "campaign": "spring_sale_2026",
    "campaign_medium": "push_notification",
    "first_open": false,
    "days_since_last_session": 3
  },
  "context": {
    "platform": "flutter_ios",
    "app_version": "2.1.0",
    "sdk_version": "0.1.0",
    "locale": "en-IN",
    "timezone": "Asia/Kolkata",
    "screen": { "width": 393, "height": 852 },
    "device": {
      "model": "iPhone15,2",
      "os_version": "17.4.1",
      "manufacturer": "Apple"
    }
  }
}
```

### 11.6a `scroll_depth` (web, content-heavy page)

Fired automatically by `trackScrollDepth()` (web) / `ScrollDepthTracker`
(Flutter) at 25/50/75/100% milestones, once per page.

```json
{
  "event_id": "7e4d5c6b-8a9f-1e2d-3c4b-5a6f7e8d9c0b",
  "event_name": "scroll_depth",
  "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
  "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
  "client_ts": "2026-05-09T10:25:33.412Z",
  "properties": {
    "percent": 75,
    "path": "/blog/2026/spring-collection",
    "name": "/blog/2026/spring-collection"
  },
  "context": {
    "platform": "web",
    "app_version": "1.4.2",
    "sdk_version": "0.1.0",
    "locale": "en-IN",
    "page": {
      "url": "https://letztrip.com/blog/2026/spring-collection",
      "path": "/blog/2026/spring-collection",
      "referrer": "https://letztrip.com/blog",
      "title": "Spring Collection 2026 - Letztrip"
    }
  }
}
```

Each user generates **at most 4** scroll events per page (one per
milestone). Skip on forms, dialogs, and short pages — every milestone is
a row in BigQuery.

### 11.6 `error_occurred` (Flutter Android, in-app crash recovery)

```json
{
  "event_id": "4d5e6f7a-8b9c-0d1e-2f3a-4b5c6d7e8f9a",
  "event_name": "error_occurred",
  "user_id": "u_19384",
  "anonymous_id": "a_4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e",
  "session_id": "s_2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a",
  "client_ts": "2026-05-09T11:05:17.834Z",
  "properties": {
    "error_type": "ApiTimeout",
    "error_message": "checkout/place-order timed out after 8000ms",
    "error_code": "TIMEOUT_8S",
    "endpoint": "/v1/orders",
    "http_status": 0,
    "retry_count": 2,
    "screen": "CheckoutReviewPage",
    "fatal": false,
    "recovered": true,
    "stack_trace_hash": "a1b2c3d4e5f6"
  },
  "context": {
    "platform": "flutter_android",
    "app_version": "2.1.0",
    "sdk_version": "0.1.0",
    "locale": "hi-IN",
    "timezone": "Asia/Kolkata",
    "device": {
      "model": "Pixel 8",
      "os_version": "14",
      "manufacturer": "Google",
      "network_type": "4g"
    }
  }
}
```

> **Why no full stack trace?** Long stack strings frequently trip the
> card-like / phone regex on hex digits and consume body budget.
> Hash + symbolicate server-side. If you must capture more, send it to
> a separate `/errors` ingestion path.

### 11.7 Full POST request: a batch

This is what an SDK actually sends — wraps events in `{"batch":[...]}`.
Up to 100 events, ≤ 1 MB total.

```json
{
  "batch": [
    {
      "event_id": "8f3b9c2a-1d4e-4a7f-b9c2-1a2b3c4d5e6f",
      "event_name": "session_started",
      "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
      "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
      "client_ts": "2026-05-09T10:23:45.000Z",
      "properties": { "first_open": false, "entry_source": "direct" },
      "context": {
        "platform": "web",
        "app_version": "1.4.2",
        "sdk_version": "0.1.0",
        "locale": "en-IN",
        "page": {
          "url": "https://letztrip.com/",
          "path": "/",
          "referrer": "",
          "title": "Letztrip"
        }
      }
    },
    {
      "event_id": "9e8d7c6b-5a4f-3e2d-1c0b-9a8f7e6d5c4b",
      "event_name": "page_viewed",
      "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
      "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
      "client_ts": "2026-05-09T10:23:46.235Z",
      "properties": { "name": "/", "title": "Letztrip" },
      "context": {
        "platform": "web",
        "app_version": "1.4.2",
        "sdk_version": "0.1.0",
        "locale": "en-IN"
      }
    },
    {
      "event_id": "0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d",
      "event_name": "button_clicked",
      "anonymous_id": "a_2f8a91b3c4d5e6f7a8b9c0d1e2f3a4b5",
      "session_id": "s_5b4a3c2d1e0f9a8b7c6d5e4f3a2b1c0d",
      "client_ts": "2026-05-09T10:24:01.001Z",
      "properties": {
        "button_id": "nav-shop",
        "button_text": "Shop"
      },
      "context": {
        "platform": "web",
        "app_version": "1.4.2",
        "sdk_version": "0.1.0",
        "locale": "en-IN"
      }
    }
  ]
}
```

Sending it with `curl`:

```sh
export SIGNAL_WRITE_KEY=wk_dev_...   # paste the plaintext key you received
curl -sS -X POST http://localhost:8080/v1/events \
  -H 'Content-Type: application/json' \
  -H "X-Write-Key: $SIGNAL_WRITE_KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d @batch.json
```

Expected response:

```json
{ "accepted": 3, "request_id": "..." }
```

### 11.8 Field rules at a glance

| Field | Required | Notes |
| --- | --- | --- |
| `event_id` | ✓ | UUID v4 per event. Used as the dedup key. |
| `event_name` | ✓ | One of: `page_viewed`, `button_clicked`, `form_submitted`, `identify`, `session_started`, `error_occurred`, `scroll_depth`. |
| `user_id` | optional | String or null. Empty string when anonymous. |
| `anonymous_id` | ✓ | Persistent per-device id. SDK rotates on `reset()`. |
| `session_id` | ✓ | Rolls after 30 min of inactivity. |
| `client_ts` | ✓ | RFC3339 / ISO8601 with millisecond precision. UTC strongly preferred. |
| `properties` | optional | Free-form object, ≤ 50 keys. **No emails, phones, card-shaped strings, or SSNs** — collector rejects with `pii_violation`. |
| `context` | ✓ | Must include `platform`, `app_version`, `sdk_version`. |
| `context.platform` | ✓ | One of: `web`, `flutter_ios`, `flutter_android`. |
| `context.app_version` | ✓ | Your app's semver, e.g. `1.4.2`. |
| `context.sdk_version` | ✓ | The SDK's version. Pin and bump deliberately. |

### 11.9 Common reject reasons

| Send this | Get | Why |
| --- | --- | --- |
| Wrong `event_name` (e.g. `"clicked"`) | `400 schema_violation` | Not in the enum. |
| Email in `properties` (e.g. `"email": "x@y.com"`) | `400 pii_violation` | PII regex matches. |
| 13–16 digit number in properties (even an order id) | `400 pii_violation` | Card regex is greedy. Use shorter ids or prefix with letters. |
| `properties` with 51+ keys | `400 schema_violation` | Cap is 50. |
| `client_ts` not RFC3339 | `400 schema_violation` | Use `new Date().toISOString()` (web) or `DateTime.now().toUtc().toIso8601String()` (Dart). |
| 101+ events in `batch` | `400 invalid_batch_size` | Cap is 100. Split. |
| Body > 1 MB | `413 batch_too_large` | Split. |

## 12. Instrumenting client apps with the `signal-integrate` skill

The signal repo ships a Claude Code skill at
[`.claude/skills/signal-integrate/`](.claude/skills/signal-integrate/) that
instruments any frontend repo with Signal events. It does **not** install
an SDK — it drops a single ~150-line `track()` helper file into the
target repo and then walks every page, button, link, form, and auth-state
change to insert tracking calls at each call site.

Stacks supported:
- Next.js (App Router or Pages Router)
- React SPA (Vite or CRA, with react-router)
- Flutter (Navigator or `go_router`)

### 12.1 One-time setup

Symlink the skill from this repo into `~/.claude/skills/` so Claude Code
auto-discovers it from any target repo:

```sh
# from the signal repo root
mkdir -p ~/.claude/skills
ln -sfn "$PWD/.claude/skills/signal-integrate" ~/.claude/skills/signal-integrate
```

Re-run on each machine that needs the skill. Updates to the canonical
copy in this repo are picked up live through the symlink.

### 12.2 Use it from any target frontend repo

```sh
cd /path/to/your-app          # the Next.js / Flutter / React app — NOT signal
claude
```

In the prompt:

> "Integrate signal — collector at `https://signal-collector-xxxxx.run.app`"

(Use `http://localhost:8080` for local-collector smoke testing.)

Claude follows the workflow below.

### 12.3 What the skill does, step by step

**Step 1 — detect the stack.** Claude greps `pubspec.yaml`, `package.json`,
the presence of `app/` vs `pages/`, lockfile name, and routing libraries.
Classifies into `flutter` / `nextjs-app` / `nextjs-pages` / `react-spa`,
plus router (Navigator vs `go_router`) and package manager
(pnpm/yarn/npm). Confirms with you before proceeding.

**Step 2 — drop the helper file.** One file, no abstractions:

| Stack | Destination |
|---|---|
| Next.js (root has `lib/`) | `lib/track.ts` |
| Next.js (root has `src/`) | `src/lib/track.ts` |
| React SPA | `src/lib/track.ts` |
| Flutter | `lib/track.dart` |

The helper handles anon id, session id (30-min rollover), batched flush
(every 5s or 20 events), retries on 5xx, drop on 4xx, persistence
(`localStorage` for web, Hive for Flutter), and `pagehide`/lifecycle
flush. It exposes only top-level functions: `track`, `setUserId`, `reset`.

For Flutter, Claude also runs:

```sh
flutter pub add hive_ce hive_ce_flutter http uuid
```

For web there's no extra dependency — `localStorage` is used directly.

**Step 3 — wire init in one place, reusing the host app's existing session id.**

The helper deliberately does NOT mint its own session id when the host
already has one. Web reads it from `sessionStorage` (the same place
`getSessionId()` / the API client's `X-Session-Id` header pulls from).
Flutter receives it via `initAnalytics(sessionId: ...)` from the host's
`applicationVariables.sessionID`. This keeps a **single session id**
across native HTTP requests, webview-injected storage
(`pulse_session_id`), and analytics events — no double-counting.

| Stack | Where | What goes there |
|---|---|---|
| Next.js App Router | New `app/AnalyticsBoot.tsx` client component, mounted in `app/layout.tsx` | Helper auto-detects session id from `sessionStorage['pulse_session_id']` (webview) or `sessionStorage['x-session-id']` (canonical web). |
| Next.js Pages Router | Edit `pages/_app.tsx` | Same auto-detection. |
| React SPA | Edit `src/App.tsx` or `src/main.tsx` | Same auto-detection. |
| Flutter | Edit `lib/main.dart` | `await initAnalytics(sessionId: applicationVariables.sessionID);` before `runApp()` (after `WidgetsFlutterBinding.ensureInitialized()`). |

If the host gets a new session id at runtime, call `setSessionId(newId)`
once — subsequent events carry the new id.

**Step 4 — wire page-view auto-capture (router-driven, one place).** No
manual `track('page_viewed')` per page; the router fires it for every
route change.

| Stack | Mechanism |
|---|---|
| Next.js App Router | `usePathname` + `useSearchParams` listener inside `AnalyticsBoot` |
| Next.js Pages Router | `router.events.on('routeChangeComplete', …)` |
| React SPA | `useLocation` listener in a `<RouteTracker />` |
| Flutter Navigator | `TrackNavigatorObserver()` in `MaterialApp.navigatorObservers` |
| Flutter `go_router` | `TrackNavigatorObserver()` in `GoRouter(observers: [...])` |

**Step 5 — walk and instrument every interactive element.** This is the
bulk of the work. Claude greps for:

- **Buttons / links / clickables**: `onClick`, `onPressed`, `onTap`,
  `<button>`, `<Link>`, `ElevatedButton`, `InkWell`, `IconButton`, …
- **Forms**: `<form>`, `Form(`, `onSubmit`
- **Auth state hooks**: `useSession`, `onAuthStateChanged`, `useAuth`, …

Then rewrites each call site to fire `track(...)` first:

```tsx
// before
<button onClick={handleSave}>Save</button>

// after
<button
  onClick={(e) => { track('button_clicked', { id: 'save' }); handleSave(e); }}
>
  Save
</button>
```

```dart
// before
ElevatedButton(onPressed: handleSave, child: Text('Save'))

// after
ElevatedButton(
  onPressed: () {
    track('button_clicked', {'id': 'save'});
    handleSave();
  },
  child: Text('Save'),
)
```

Auth state — once per repo, in your `useSession` provider /
`FirebaseAuth.authStateChanges()` listener:

```tsx
useEffect(() => {
  if (session?.user?.id) {
    setUserId(session.user.id);
    track('identify', { plan: session.user.plan });
  } else {
    setUserId(null);
  }
}, [session]);
```

**Reviewable in batches.** If 50+ files need editing, Claude does one
feature area at a time (e.g. `app/checkout/`), shows you the diff, and
waits for your "go" before continuing. Not a 200-file-in-one-shot
rewrite.

**Step 6 — env-var documentation.**

Web — append to `.env.local.example` (create if missing):

```
NEXT_PUBLIC_ANALYTICS_ENDPOINT=https://signal-collector.example.run.app
NEXT_PUBLIC_ANALYTICS_WRITE_KEY=wk_dev_change_me
NEXT_PUBLIC_APP_VERSION=
```

For Vite use `VITE_*`; for CRA use `REACT_APP_*`. The helper auto-detects
which.

Flutter — append to README:

```sh
flutter run \
  --dart-define=ANALYTICS_ENDPOINT=https://signal-collector.example.run.app \
  --dart-define=ANALYTICS_WRITE_KEY=wk_dev_change_me \
  --dart-define=APP_VERSION=1.0.0
```

**Step 7 — verify.** The skill runs the target repo's existing checks:
`pnpm typecheck` / `npm run typecheck` / `npx tsc --noEmit` for web;
`dart analyze` for Flutter. Won't declare done if the verify fails — it
fixes import paths, missing `'use client'` directives, etc., until
green.

**Step 8 — smoke test.** The skill prints a runnable smoke-test:

```sh
# Web — start dev server, click around, watch the Network tab for POSTs
# to /v1/events with 202 responses.

# Flutter — flutter run with --dart-define vars set, navigate, observe
# the collector's logs.

# Both: confirm rows in BigQuery
bq query --use_legacy_sql=false --project_id=letztrip-production-account \
  'SELECT event_id, event_name, anonymous_id, properties, server_ts
   FROM analytics.events
   WHERE server_ts > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 10 MINUTE)
   ORDER BY server_ts DESC LIMIT 20'
```

### 12.4 What the skill explicitly does NOT do

- Does **not** install an SDK or a package. The helper is one vendored file.
- Does **not** introduce a class-based `Analytics` API. Just top-level
  `track`, `setUserId`, `reset`.
- Does **not** add a global click listener that fires for every DOM
  click. Each button is instrumented in place so each event has
  meaningful properties (id, text, surrounding context).
- Does **not** instrument `onChange` per keystroke. Only `onSubmit`,
  `onBlur`, or explicit value commits.
- Does **not** track loading states, render cycles, or programmatic
  navigation that isn't a user action.
- Does **not** auto-replace an existing analytics SDK. If it sees
  `@example/analytics-web` (or similar) already imported, it stops and
  asks you to remove the old SDK before adding inline `track()` calls.

### 12.5 Adding a new stack

If the skill's detection step lands on `unknown` for a stack you care
about (Svelte, Solid, Vue, vanilla JS, Angular, …), the helper file
itself (`helpers/track.ts`) is framework-agnostic — only the route
listener and instrumentation patterns differ. Open
[`.claude/skills/signal-integrate/SKILL.md`](.claude/skills/signal-integrate/SKILL.md)
and add a detection branch in §1 plus a wiring snippet in §3 / §4 for
the new stack. Submit a PR.
