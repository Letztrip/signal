# schemas/ — the wire contracts

Two contracts define what Signal accepts and what it stores. **Both are
load-bearing and evolve under strict rules** — see the root
[CLAUDE.md](../CLAUDE.md) "NEVER DO" and `docs/architecture.md`
"Schema evolution rules".

| File | Role | Consumed by |
|---|---|---|
| `events.v1.json` | JSON Schema 2020-12. The **input contract** clients POST to `/v1/events`. Validation runs in the collector. | Embedded copy at `../collector/schemas/events.v1.json` (`//go:embed`). |
| `event.proto` | proto3 wire format for the `events-raw` Pub/Sub topic. Registered as the topic schema; the BQ subscription reads it via `use_topic_schema`. | `../collector/eventpb/event.pb.go` (via `make proto`); Pub/Sub schema registry (via `scripts/bootstrap-gcp.sh`). |

## events.v1.json

- `required`: `event_id`, `event_name`, `client_ts`, `anonymous_id`, `session_id`, `context`. `additionalProperties: false` at the top level.
- `event_name` is an **enum** — `page_viewed`, `button_clicked`, `form_submitted`, `identify`, `session_started`, `error_occurred`, `scroll_depth`.
- `context` requires `platform` (enum `web`/`flutter_ios`/`flutter_android`), `app_version`, `sdk_version`; allows optional `locale`/`timezone`/`screen`/`page`/`device`.
- `properties` is a free object, `maxProperties: 50`.

**Required-ness lives here, not in BigQuery.** The Pub/Sub→BQ subscription treats
every proto3 scalar as nullable in-topic, so `scripts/bq-schema.json` columns must
all be `NULLABLE` — this JSON Schema is the only place required fields are enforced.

### Adding a new event type
1. Append the value to the `event_name` enum here.
2. Copy the change to `../collector/schemas/events.v1.json` — **the two must stay in sync** (the collector embeds the copy, not this file).
3. Build/test/ship the collector. No proto change, no BQ change (`event_name` is a plain string column).

## event.proto

- `message AnalyticsEvent` — 17 fields, **all `string`** (tags 1–17). Timestamps (`client_ts`, `server_ts`) are RFC3339 strings, *not* `google.protobuf.Timestamp`: Pub/Sub Schema Registry rejects external imports, and BQ parses RFC3339 into `TIMESTAMP` columns natively.
- `go_package` and `package analytics.v1` are placeholders from the scaffold — matched by `../collector/go.mod`'s `github.com/example/event-pipeline/collector` module path. Don't "fix" them.

### The wire contract is immutable
**Never renumber, rename, or remove a field number.** Old messages sitting in the
Pub/Sub backlog would misread the wire format and corrupt `analytics.events`.

### Adding a new column
1. Append a field with a **fresh** tag number to `event.proto` (never reuse a freed one).
2. `make proto` → regenerates `../collector/eventpb/event.pb.go`.
3. Populate it in `../collector/server.go:buildProto`.
4. Append a matching `NULLABLE` column to `../scripts/bq-schema.json`, then `bq update analytics.events scripts/bq-schema.json`.
5. Ship the collector. Old rows get `NULL`.

Field order in `event.proto` ↔ column order in `scripts/bq-schema.json` ↔ population
in `buildProto` must all correspond.
