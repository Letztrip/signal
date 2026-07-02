# scripts/ — GCP provisioning & write-key ops

Operator shell scripts for standing up and feeding the pipeline. Run from the
repo root. Parent: [../CLAUDE.md](../CLAUDE.md). These touch **real GCP** —
there is no emulator path (see `docs/architecture.md`).

| File | What it does | Run when |
|---|---|---|
| `bootstrap-gcp.sh` | One-shot, **idempotent** provisioning of the whole data plane. | Once per project, and after schema changes. |
| `create-service-account.sh` | Creates `collector-vm@<project>.iam.gserviceaccount.com` and downloads a JSON key. Idempotent (re-run mints a new key). | Once, before `bootstrap-gcp.sh`. |
| `mint-write-key.sh <app-id> [env]` | Mints a random write key + its argon2id PHC storage line. | Per client app; on key rotation. |
| `bq-schema.json` | BigQuery table schema for `analytics.events` — data file, not a script. | Read by `bootstrap-gcp.sh` step 5. |

## bootstrap-gcp.sh

`set -euo pipefail`. Resolves `PROJECT`/`REGION`/`BQ_LOCATION`/`VPC_NETWORK`/
`VM_SERVICE_ACCOUNT` from env → `gcloud config` → hard fallbacks
(`letztrip-production-account`, `us-central1`). Every step is "describe-or-create",
so re-running is safe. Steps:

1. Pub/Sub service-agent identity.
2. Protobuf schema `events-raw-schema` from `../schemas/event.proto`.
3. Topic `events-raw` (schema attached, binary encoding, 7-day retention).
4. DLQ topic `events-raw-dlq`.
5. BQ dataset `analytics` + table `events` (`bq mk` on create; `bq update` on existing — the only relax it needs is REQUIRED→NULLABLE).
6. Grant the Pub/Sub service agent `bigquery.dataEditor` + `metadataViewer`.
7. BQ subscription `events-raw-to-bq` (`--use-topic-schema --drop-unknown-fields`, DLQ after 5 delivery attempts).
8. DLQ inspection subscription `events-raw-dlq-inspection`.
9. Secret `SIGNAL_WRITE_KEY` (override via `WRITE_KEYS_SECRET_NAME`) with a placeholder version.
10. Grant the VM SA `pubsub.publisher` + `pubsub.viewer` on the topic (viewer is needed because the collector calls `topic.Exists()` at startup) and `secretmanager.secretAccessor` on the secret.

> **Deploy-target caveat.** This script's IAM grants and "Next" steps are written
> for a **VM + `docker compose`** deployment (it grants roles to `collector-vm@…`
> and prints `docker compose up` instructions). Production actually runs on
> **Cloud Run** via `../cloudbuild.yaml`, which uses the same SA
> (`_COLLECTOR_SA`) and secret but deploys differently. The Pub/Sub / BQ /
> Secret provisioning is identical either way — only the compute host differs.

## mint-write-key.sh

`/bin/sh`. Needs `openssl` + the `argon2` CLI (`brew install argon2` /
`apt install argon2`). Generates `wk_<env>_<32 chars>`, hashes with
`argon2 -id -m 16 -t 3 -p 2 -l 32 -e` (m=16 → 2^16 KiB = 64 MiB, matching the
collector's floor in `../collector/auth.go`), and prints two lines: the
**plaintext** (hand to the client, store nowhere) and the **storage line**
`<app-id>:<PHC>` (append to the `SIGNAL_WRITE_KEY` secret via
`gcloud secrets versions add`). The collector's 5-min refresh picks it up.

## Conventions for adding a script

- Idempotent by default (describe-or-create), `set -euo pipefail`.
- Never echo or persist secrets — plaintext keys and SA JSON are print-once. `.gitignore` already excludes `*-key.json`, `*.sa.json`, `.env`.
- Resolve project/region from env → `gcloud config` → fallback, same as the existing scripts.
