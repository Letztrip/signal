# Architecture — Signal

## Section 1 — High-level design

### Service purpose

Signal is the analytics ingest plane for Letztrip products. It accepts JSON
event batches over HTTPS from web and Flutter clients, validates them against
a versioned JSON Schema, scrubs PII, enriches each event with server-side
fields, flattens to a Pub/Sub Protobuf message, and lets a native Pub/Sub →
BigQuery subscription land rows in `analytics.events`. **There is no
subscriber service; no emulator path.** Production and local dev both talk
to the same GCP project (`letztrip-production-account`) — only the auth
mode (Secret Manager vs inline plaintext) and CORS scope differ.

### System context

```
clients (web SDK + Flutter SDK + curl + signal-integrate-instrumented apps)
                              │  HTTPS  POST /v1/events
                              ▼
                  ┌────────────────────────┐    ┌──────────────────────┐
                  │   collector            │◄──►│  Memorystore Redis   │
                  │   (Go, Cloud Run)      │    │  (idempotency, 24h)  │
                  └───────────┬────────────┘    └──────────────────────┘
                              │   Pub/Sub publish (Protobuf binary)
                              ▼
                ┌──────────────────────────┐
                │  events-raw (topic)      │
                │  schema: events-raw-     │
                │  schema (PROTOCOL_BUFFER)│
                └─────────────┬────────────┘
                              │  use_topic_schema = true
                              ▼
                ┌──────────────────────────┐    ┌──────────────────────┐
                │  events-raw-to-bq        │───►│ analytics.events     │
                │  (BQ subscription)       │    │ DAY-partitioned on   │
                │  max delivery 5 → DLQ    │    │ server_ts; clustered │
                └──────────────────────────┘    │ on event_name,app_id │
                              │                  └──────────────────────┘
                              ▼ on failure ×5
                ┌──────────────────────────┐
                │  events-raw-dlq          │  7-day retention
                └──────────────────────────┘
```

- **Upstream callers** — browser/native clients running the `track()` helper
  vendored by the `signal-integrate` skill, plus direct `curl` for smoke
  testing.
- **Downstream services** — none; the collector publishes to Pub/Sub and
  returns 202. All persistence is handled by the BQ subscription.
- **Infrastructure** — Pub/Sub (topic + DLQ), BigQuery (dataset `analytics`,
  table `events`), Secret Manager (`SIGNAL_WRITE_KEY`, `REDIS_HOST`,
  `REDIS_PORT`, `REDIS_PASSWORD`), Memorystore Redis.
- **Inbound API style** — REST. Single endpoint: `POST /v1/events` (auth'd
  via `X-Write-Key`). Plus `GET /healthz` (no auth) for liveness.

### Module boundaries

Single Go module: `github.com/example/event-pipeline/collector`. The
collector is the only deployable. All Go source lives flat in
`collector/`; one subpackage (`collector/eventpb/`) holds generated
Protobuf bindings.

| Path | Owns |
|---|---|
| `collector/main.go` | Process entry. Env wiring, dependency construction, chi router setup, signal-based graceful shutdown. |
| `collector/server.go` | HTTP handlers (`handleHealth`, `handleEvents`). The `buildProto` flatten from SDK JSON envelope → wire format lives here. |
| `collector/auth.go` | CORS middleware, request-id middleware, recover middleware, logging middleware, auth middleware, `KeyStore`/`KeyManager` for write-key verification. |
| `collector/validator.go` | Embedded JSON Schema 2020-12 validator (`//go:embed schemas/events.v1.json`). |
| `collector/pii.go` | Recursive deny-list scan over `properties` for email / phone / card-shape / SSN. |
| `collector/idempotency.go` | Redis-backed 24h response cache keyed by `(app_id, idempotency_key)`. |
| `collector/enrich.go` | Server-side enrichment (`server_ts`, UA family/OS, geo placeholder). |
| `collector/errors.go` | `writeErr` helper — single error envelope shape across all 4xx/5xx. |
| `collector/eventpb/` | `protoc-gen-go` output. Never hand-edited. |
| `collector/schemas/events.v1.json` | Copy of the root JSON Schema, embedded into the binary. Kept in sync with `schemas/events.v1.json` manually. |
| `schemas/event.proto` | Pub/Sub topic schema (PROTOCOL_BUFFER). 17 fields, all string except… all string actually (Pub/Sub Schema Registry can't resolve external imports, so `client_ts` / `server_ts` are RFC3339 strings, not `google.protobuf.Timestamp`). |
| `schemas/events.v1.json` | JSON Schema 2020-12 — the input contract clients hit. |
| `scripts/` | Operator scripts: `bootstrap-gcp.sh`, `create-service-account.sh`, `mint-write-key.sh`, `bq-schema.json`. |
| `.claude/skills/signal-integrate/` | Claude Code skill that vendors a `track()` helper into target frontend repos and instruments them in place. Lives in the signal repo so the SDK code is co-located with the collector. |

### Architecture philosophy

Reliability-first, but **for a write-only ingest endpoint that can lose individual events without user-visible consequence**. Concretely:

- **Validate at the edge, trust downstream.** JSON Schema + PII scan run on every event before Pub/Sub publish. Once an event is in Pub/Sub, it's GCP's problem to deliver.
- **Idempotency is best-effort.** Redis is optional — if it pings-fails at startup the collector logs a warning and continues without it. Duplicates in BQ are caller-fixable (`event_id` deduplication in queries).
- **Drop 4xx, retry 5xx.** Clients implementing the helper retry 5xx and network errors with exponential backoff; 4xx drops silently. Status codes are stable contract.
- **Server hashes; clients never see the hash.** Write keys are stored argon2id-hashed in Secret Manager; refresh happens every 5 min via `atomic.Pointer[KeyStore]` swap so rotation is hot.
- **No abstraction layer in client code.** The `signal-integrate` skill does NOT install an SDK — it vendors a single `track()` helper file per repo and instruments call sites in place. This keeps the client integration debuggable from the call site, not from inside an opaque class.

### Data flow (request lifecycle)

For `POST /v1/events`:

1. **CORS middleware** intercepts. If `OPTIONS`, returns 204 with `Access-Control-Allow-Origin`/`-Methods`/`-Headers`/`Max-Age` and exits. Otherwise tags response with `ACAO` based on `Origin`.
2. **Request-ID middleware** reads `X-Request-ID` or mints a UUID, sets response header, attaches to context.
3. **Recover middleware** wraps panics → 500 `internal` error.
4. **Logging middleware** records method/path/status/duration/request-id at info.
5. **Auth middleware** (only on `/v1/events`, not `/healthz`) reads `X-Write-Key`, looks up against the current `KeyStore` snapshot via argon2id verify, returns 401 on miss; on hit, attaches `app_id` to context.
6. **`handleEvents`** runs the data plane:
   - Check `Idempotency-Key` against Redis. Hit → return cached 202 body, set `X-Idempotent-Replay: 1`.
   - Read body bounded by `http.MaxBytesReader` (1 MB). Oversize → 413 `batch_too_large`.
   - JSON-decode envelope. Bad JSON → 400 `invalid_json`. Batch size 0 or >100 → 400 `invalid_batch_size`.
   - **For each event**: validate against embedded JSON Schema (400 `schema_violation`), then recursive PII scan over `properties` (400 `pii_violation`), then `buildProto` to flatten into `eventpb.AnalyticsEvent`. `client_ts` and `server_ts` are RFC3339 strings; `properties` and `context` are JSON-stringified server-side.
   - Publish each event via `pubsub.Topic.Publish` with `OrderingKey = anonymous_id` and attributes `event_name`, `app_id`, `platform`. Failed publish → 503 `publisher_saturated`.
   - Cache the 202 body into Redis under the idempotency key.
   - Respond `202 {"accepted": N, "request_id": "..."}`.

The handler returns 202 **once events are accepted into the Pub/Sub
publisher's buffer**. It does *not* wait for broker ack — the publish
results are awaited inline, but the publisher's batching means most
events join an in-flight batch.

### Cross-cutting concerns

| Concern | Implementation | File |
|---|---|---|
| Multi-tenancy | One `app_id` per write key; attached to context by auth, included as Pub/Sub attribute and in the published event payload. No request-scoped tenant header. | `auth.go` |
| Error envelope | Single `errEnvelope{Error: errBody{Code, Message, RequestID}}` — every 4xx/5xx response uses `writeErr`. | `errors.go` |
| Auth | argon2id PHC strings stored in Secret Manager `SIGNAL_WRITE_KEY`; collector parses, holds in immutable `KeyStore`; `KeyManager` refreshes every 5 min via `atomic.Pointer[KeyStore]` swap so rotation is hot. Floor params (`m=65536, t=3, p=2`, hash length 32) enforced at parse time — weaker hashes rejected. Dev path: `WRITE_KEYS_PLAINTEXT` env var, hashed at startup. | `auth.go` |
| CORS | `corsMiddleware` is first in the chain. Allow-list is comma-separated `CORS_ALLOWED_ORIGINS` env, or `*` (default). Auth is via `X-Write-Key`, no cookies — credentials-mode CORS is intentionally not enabled, so `*` is safe. | `auth.go` |
| Logging | `log/slog` JSON to stderr. Levels via `LOG_LEVEL` env (`debug`/`info`/`warn`/`error`). Every line carries `svc=collector`. | `main.go` `newLogger` |
| Observability | slog JSON + **Sentry** (errors + tracing; no-op unless `ENVIRONMENT=production` and `SENTRY_DSN` set) + **Prometheus `/metrics`** (Bearer-authed via `METRICS_AUTH_TOKEN`, scraped by vmagent → Grafana). No OTel. Health is `/healthz` returning 200; Cloud Run startup probe is the formal liveness signal. | `observability.go` |

## Section 2 — Low-level details

### Entry points

| Module | Entry | What it wires |
|---|---|---|
| `collector` | `collector/main.go:run` | Reads env (`GCP_PROJECT`, `PUBSUB_TOPIC`, `WRITE_KEYS_SECRET`/`WRITE_KEYS_PLAINTEXT`, `CORS_ALLOWED_ORIGINS`, `REDIS_HOST`/`REDIS_PORT`/`REDIS_PASSWORD`, `LOG_LEVEL`, `COLLECTOR_PORT`); constructs `KeyManager`, Pub/Sub client + topic (with publish settings: `DelayThreshold=50ms`, `CountThreshold=100`, `ByteThreshold=1MB`, `NumGoroutines=4`, `Timeout=10s`, flow-control block), Redis client (best-effort ping; warn-on-fail), chi router with the middleware chain (CORS → RequestID → Recover → [Sentry] → Metrics → Logging); runs `KeyManager.Run` for 5-min refresh; starts `http.Server` (read 5s / write 15s / idle 15s); `signal.NotifyContext` for SIGINT/SIGTERM → 15s graceful shutdown. |

The collector calls `topic.Exists(ctx)` at startup and **fails fast** if
the topic is missing — provisioning is the bootstrap script's job, not
the application's.

### Layering & key types

The collector is intentionally flat — no service/repository layers. The
five files in `collector/` are peers, not stacked.

- **Handler layer** (`server.go`): `Server` struct holds validator,
  topic, idem store, key manager, logger. Two handlers: `handleHealth`,
  `handleEvents`.
- **Wire-format layer** (`eventpb/event.pb.go`): generated; `AnalyticsEvent` 17-field flat proto.
- **Middleware layer**: `corsMiddleware`, `requestIDMiddleware`, `recoverMiddleware`, `loggingMiddleware`, `authMiddleware` in `auth.go`; `sentryMiddleware`, `metricsMiddleware` in `observability.go`. All return `func(http.Handler) http.Handler`.
- **Plumbing** (`validator.go`, `pii.go`, `enrich.go`, `idempotency.go`, `errors.go`): single-purpose helpers; no shared state.

### API surface

| Method | Path | Handler | Middleware chain |
|---|---|---|---|
| `GET` | `/healthz` | `Server.handleHealth` | CORS → RequestID → Recover → [Sentry] → Metrics → Logging |
| `GET` | `/metrics` | `metricsHandler` | same chain; own Bearer auth (`METRICS_AUTH_TOKEN`), not `authMiddleware` |
| `OPTIONS` | `*` | (handled by `corsMiddleware`, 204) | CORS only — short-circuits before route match |
| `POST` | `/v1/events` | `Server.handleEvents` | CORS → RequestID → Recover → [Sentry] → Metrics → Logging → Auth |

The `Sentry` middleware is present only when Sentry initialised
(`ENVIRONMENT=production` + `SENTRY_DSN`).

Registered in `collector/main.go` around `chi.NewRouter()`. Auth is in a
`r.Group(...)` so `/healthz` stays unauthenticated.

### Wire format & status codes

`POST /v1/events` accepts `{"batch": [event, event, ...]}` with up to
**100 events and 1 MB total**. Required headers: `X-Write-Key`,
`Content-Type: application/json`. Optional: `Idempotency-Key` (UUID;
repeats within 24h replay the cached response with status 202).

| Status | Code | Meaning |
|---|---|---|
| 202 | — | Accepted into publisher buffer. |
| 400 | `invalid_json` | Body not parseable. |
| 400 | `invalid_batch_size` | 0 or >100 events. |
| 400 | `schema_violation` | An event failed JSON Schema. |
| 400 | `pii_violation` | A property value matched a PII regex. |
| 401 | `invalid_write_key` | Bad or missing `X-Write-Key`. |
| 413 | `batch_too_large` | Body exceeded 1 MB. |
| 503 | `publisher_saturated` | Pub/Sub flow control blocked. |
| 5xx | `internal` | Anything else. |

Error envelope: `{"error":{"code":"...","message":"...","request_id":"..."}}`.

### Data model & persistence

The collector owns no persistent state of its own. Two stores it talks
to:

| Store | Schema / key shape | Access path |
|---|---|---|
| BigQuery `analytics.events` | 17 columns mirroring `schemas/event.proto`. `properties` + `context` are `JSON` columns. DAY-partitioned on `server_ts`; clustered on `event_name, app_id`. | Collector never reads/writes BQ directly. The Pub/Sub BQ subscription (`events-raw-to-bq`) writes rows; the subscription is configured with `use_topic_schema=true`, `write_metadata=false`, `drop_unknown_fields=true`. Provisioned by `scripts/bootstrap-gcp.sh`. |
| Memorystore Redis | Keys: `idem:<app_id>:<idempotency_key>` → JSON response body. TTL: 24h. | `collector/idempotency.go` — `RedisIdem.Get`/`.Set` from `handleEvents`. `redis.Nil` is not-found, not error. |

### Wire format details

Pub/Sub Schema Registry **rejects external imports**
(`import "google/protobuf/timestamp.proto"` doesn't resolve), so
`client_ts` and `server_ts` are `string` (RFC3339) — not
`google.protobuf.Timestamp`. The BQ subscription parses RFC3339 strings
into `TIMESTAMP` columns natively.

Proto field numbers are **immutable**. Adding a column means appending a
new tag number in `schemas/event.proto`, running `make proto`, populating
in `buildProto`, then appending a nullable column to
`scripts/bq-schema.json` and running `bq update analytics.events ...`.
Removing or renaming is a new table.

### Critical invariants & gotchas

- **Pub/Sub BQ subscription treats all proto3 scalars as nullable in topic.** This means BQ table columns MUST be `NULLABLE`, even fields like `event_id` that the collector enforces as required at the JSON Schema layer. The `REQUIRED` constraint was tried once and rejected with `Incompatible schema: field event_id is required in table, but nullable in topic`. See `scripts/bq-schema.json` — every column is `NULLABLE`; required-ness is enforced by the JSON Schema validator at ingest, not by BQ.
- **CORS must be the first middleware.** If it sits behind request-id/logging/auth, preflight `OPTIONS` hits chi's route matcher and returns 405. The middleware order in `collector/main.go` is load-bearing.
- **`pulse_session_id` is an external key**, owned by the Letztrip Flutter shell's `in_app_webview_page.dart`. The signal-integrate web helper reads from it for webview-rendered pages and falls back to `x-session-id` (canonical web key set by host's `getSessionId()`). Do not rename — see `.claude/skills/signal-integrate/SKILL.md` step 3a.
- **Argon2id param floor enforced at parse time** (`m=65536, t=3, p=2`, hash length 32). Weaker hashes in Secret Manager are rejected with `argon2 params below floor`. To rotate stronger, mint with new params, push a new secret version, and the 5-min refresh picks it up.
- **First-deploy of Cloud Run requires `--no-traffic` to be omitted** — Cloud Run forces the initial revision to take 100%. The cloudbuild pipeline detects this via `gcloud run services describe` exit status and skips the canary stage on first deploy (`cloudbuild.yaml` step "Deploy (no traffic)").
- **`--set-env-vars` uses comma as separator.** Values containing commas (e.g. a multi-origin CORS whitelist) must be passed with the `^@^` alternate-delimiter prefix. See cloudbuild.yaml — `ENV_VARS` is built with `@` separators, prefixed `^@^` on the gcloud call.
- **PII deny list never scans `context`.** Only `properties` (recursively, including arrays and nested objects). The four regexes — email, phone (10-15 digits), card-like (13-16 with separators), SSN — match strings only.

### Configuration touch points

| Env var | Where read | What it controls |
|---|---|---|
| `GCP_PROJECT` | `main.go:run` | Pub/Sub + (transitively) Secret Manager project. Required. |
| `PUBSUB_TOPIC` | `main.go:run` | Topic name. Required. Existence checked at startup. |
| `WRITE_KEYS_SECRET` | `main.go:run` → `auth.go:NewKeyManager` | Secret Manager resource path. Production path. |
| `WRITE_KEYS_PLAINTEXT` | `main.go:run` → `auth.go:NewKeyManagerStatic` | Inline `<app-id>:<plaintext>` entries (comma- or newline-separated). Dev only — logs a loud `DEV MODE` warning. |
| `CORS_ALLOWED_ORIGINS` | `main.go:run` → `auth.go:corsMiddleware` | Comma-separated origins, or `*`. Default `*`. |
| `REDIS_HOST` / `REDIS_PORT` / `REDIS_PASSWORD` | `main.go:run` | Memorystore. Wired via Secret Manager in production (`cloudbuild.yaml` --set-secrets). Optional — idempotency disabled if `REDIS_HOST` unset. |
| `COLLECTOR_PORT` | `main.go:run` | HTTP port. Default 8080. |
| `LOG_LEVEL` | `main.go:newLogger` | slog level: `debug`/`info`/`warn`/`error`. Default info. |
| `ENVIRONMENT` | `observability.go:isProduction` | Gates Sentry on and `/metrics` closed-by-default. `production` (case-insensitive) = production behaviour. |
| `SENTRY_DSN` / `SENTRY_RELEASE` / `SENTRY_TRACES_SAMPLE_RATE` | `observability.go:initSentry` | Enable + configure Sentry. No DSN (or non-prod) → Sentry disabled. Sample rate default 1.0. |
| `METRICS_AUTH_TOKEN` | `observability.go:metricsHandler` | Bearer token for `/metrics`. Unset → 503 in production, open in dev. |
| `COLLECTOR_VERSION` | `observability.go` / `cloudbuild.yaml` | `signal_build_info` version label + Sentry release fallback. Set to the git tag at deploy. |

Either `WRITE_KEYS_SECRET` or `WRITE_KEYS_PLAINTEXT` must be set;
plaintext wins when both are present (with a warning). Both unset →
fail at startup with `set WRITE_KEYS_SECRET ... or WRITE_KEYS_PLAINTEXT ...`.

### Schema evolution rules

- **Add a new `event_name`**: edit the enum in `schemas/events.v1.json` and `collector/schemas/events.v1.json`. No proto change, no BQ change.
- **Add a new column**: append proto field with new tag number, `make proto`, populate in `buildProto`, append `NULLABLE` BQ column, `bq update`, redeploy collector. Old rows get NULL.
- **Never** rename, renumber, or remove proto fields. That's a new table.

### Deployment

Cloud Run service `signal-collector` in `us-central1`, behind the
`api-signal.travafa.com` domain. Provisioning is one-shot via
`scripts/bootstrap-gcp.sh` (Pub/Sub schema, topic, DLQ, BQ subscription,
secret, IAM bindings). Image builds and rollouts go through
`cloudbuild.yaml`, triggered by tag push (`vX.Y.Z`). Pipeline stages:
gitleaks scan → go vet+test → docker build → trivy scan → push →
`gcloud run deploy --no-traffic --tag=verify` → revision-ready check via
admin API → 10% canary (skipped on first deploy) → 100% promote.

Local dev runs the collector + a Redis sidecar via `docker compose up`,
talking to the **same** GCP project as production. Auth source flipped
to `WRITE_KEYS_PLAINTEXT` for the dev workflow.

### Test coverage

| Test | What it covers |
|---|---|
| `TestProtoRoundTrip` (`server_test.go`) | `buildProto` flatten + `proto.Marshal`/`Unmarshal` round-trip preserves all fields including server-side timestamps and JSON-stringified properties. |
| `TestParseKeyStoreVerify` (`server_test.go`) | argon2id PHC parse + verify against a known plaintext. Floor params enforced. |
| `TestCORSPreflight_AllowAll` (`cors_test.go`) | `OPTIONS` short-circuits with 204 + `ACAO: *` + allow methods/headers. |
| `TestCORSPreflight_WhitelistMatch` (`cors_test.go`) | Whitelist mode echoes the matching origin + `Vary: Origin`. |
| `TestCORSPreflight_WhitelistMiss` (`cors_test.go`) | Non-whitelisted origin: 204 but no `ACAO` header — browser will block the subsequent fetch. |
| `TestCORS_ActualPOST_AddsAllowOriginHeader` (`cors_test.go`) | Real (non-preflight) requests flow through with `ACAO` attached. |
| `TestCORS_NoOriginHeader_NoCORSHeaders` (`cors_test.go`) | Same-origin requests (no `Origin` header) pass through without CORS headers. |

No integration tests against real Pub/Sub or BQ — the bootstrap script
+ a manual smoke test serves that role.

<!-- meesho-init: generated-at=2026-05-13T11:06:08Z base-sha=d84dcc9cbbbc13ebde610a0b247ede03f4df41d0 -->
