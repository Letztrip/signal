# collector/eventpb/ — generated proto bindings

**Generated code. Never hand-edit.** `event.pb.go` is `protoc-gen-go` output from
[`../../schemas/event.proto`](../../schemas/CLAUDE.md). Regenerate with `make proto`
from the repo root, never by editing this file.

- `AnalyticsEvent` — the flat 17-field wire message published to the `events-raw`
  Pub/Sub topic. All fields are `string` (timestamps are RFC3339 strings, not
  `Timestamp` — Pub/Sub Schema Registry rejects external imports).
- Populated in [`../server.go:buildProto`](../server.go); marshalled in `handleEvents`.

To change the message, edit `../../schemas/event.proto` and follow "Adding a new
column" in [../../schemas/CLAUDE.md](../../schemas/CLAUDE.md) — field numbers are an
immutable wire contract.
