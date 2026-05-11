#!/usr/bin/env bash
# Idempotent setup of all GCP resources the events pipeline needs, for teams
# that already have a project + VPC + Pub/Sub + BigQuery enabled and are
# running the collector on a VM (not Cloud Run).
#
# Re-running is safe: every step is "create or exit 0 if it already exists."
#
# Defaults are read live from `gcloud config` and discoverable resources.
# Override anything explicitly by exporting before running:
#   PROJECT, REGION, BQ_LOCATION, VPC_NETWORK, VM_SERVICE_ACCOUNT
set -euo pipefail

# 1. Resolve project: explicit env > gcloud config > hard fallback.
PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
PROJECT="${PROJECT:-letztrip-production-account}"

echo "→ pinning gcloud config to $PROJECT"
gcloud config set project "$PROJECT" >/dev/null

# 2. Resolve region: explicit env > us-central1 (matches the existing `pulse`
# analytics dataset; `gcloud config get-value compute/region` is intentionally
# ignored here because it can drift).
REGION="${REGION:-us-central1}"
gcloud config set compute/region "$REGION" >/dev/null

# 3. BQ location colocates with REGION so we can join across analytics
# datasets without a cross-region copy job.
BQ_LOCATION="${BQ_LOCATION:-$REGION}"

# 4. Discover the VPC: explicit env > first non-default network > known fallback.
if [ -z "${VPC_NETWORK:-}" ]; then
  VPC_NETWORK=$(gcloud compute networks list --project="$PROJECT" \
    --format='value(name)' --filter='name!=default' --limit=1 2>/dev/null || true)
  VPC_NETWORK="${VPC_NETWORK:-letztrip-private-network}"
fi

VM_SERVICE_ACCOUNT="${VM_SERVICE_ACCOUNT:-collector-vm@${PROJECT}.iam.gserviceaccount.com}"

cat <<EOF
== resolved configuration ==
  PROJECT             $PROJECT
  REGION              $REGION
  BQ_LOCATION         $BQ_LOCATION
  VPC_NETWORK         $VPC_NETWORK   (informational only — VM-deployment doesn't touch this)
  VM_SERVICE_ACCOUNT  $VM_SERVICE_ACCOUNT
EOF

TOPIC="events-raw"
DLQ_TOPIC="events-raw-dlq"
BQ_SUB="events-raw-to-bq"
DLQ_SUB="events-raw-dlq-inspection"
SCHEMA="events-raw-schema"
DATASET="analytics"
TABLE="events"
SECRET="${WRITE_KEYS_SECRET_NAME:-SIGNAL_WRITE_KEY}"

PROTO_FILE="$(cd "$(dirname "$0")/.." && pwd)/schemas/event.proto"
BQ_SCHEMA_FILE="$(cd "$(dirname "$0")" && pwd)/bq-schema.json"
PROJECT_NUMBER="$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')"
PUBSUB_AGENT="service-${PROJECT_NUMBER}@gcp-sa-pubsub.iam.gserviceaccount.com"

echo "== using project=$PROJECT region=$REGION dataset=$DATASET =="

step() { echo; echo "→ $*"; }
exists_or_create() {
  if eval "$1" >/dev/null 2>&1; then
    echo "  already exists, skipping"
  else
    eval "$2"
  fi
}

step "0. provision the Pub/Sub service agent identity"
gcloud beta services identity create \
  --service=pubsub.googleapis.com --project="$PROJECT" >/dev/null 2>&1 || true

step "1. register the Protobuf schema $SCHEMA"
exists_or_create \
  "gcloud pubsub schemas describe $SCHEMA --project=$PROJECT" \
  "gcloud pubsub schemas create $SCHEMA \
     --type=protocol-buffer \
     --definition-file=$PROTO_FILE \
     --project=$PROJECT"

step "2. create primary topic $TOPIC with the schema attached"
exists_or_create \
  "gcloud pubsub topics describe $TOPIC --project=$PROJECT" \
  "gcloud pubsub topics create $TOPIC \
     --schema=$SCHEMA \
     --message-encoding=binary \
     --message-retention-duration=7d \
     --project=$PROJECT"

step "3. create dead-letter topic $DLQ_TOPIC"
exists_or_create \
  "gcloud pubsub topics describe $DLQ_TOPIC --project=$PROJECT" \
  "gcloud pubsub topics create $DLQ_TOPIC \
     --message-retention-duration=7d \
     --project=$PROJECT"

step "4. create BigQuery dataset $DATASET in $BQ_LOCATION"
if bq --project_id="$PROJECT" --location="$BQ_LOCATION" show "$DATASET" >/dev/null 2>&1; then
  echo "  already exists, skipping"
else
  bq --project_id="$PROJECT" --location="$BQ_LOCATION" mk \
    --description "Event analytics data plane." \
    "$DATASET"
fi

step "5. create or update BigQuery table $DATASET.$TABLE"
if bq --project_id="$PROJECT" show "$DATASET.$TABLE" >/dev/null 2>&1; then
  # Relax columns / add new ones — bq update is a no-op when schema matches.
  # REQUIRED → NULLABLE is the only relax move BigQuery allows; that's what
  # we need so the Pub/Sub BQ subscription's "all proto3 scalars are
  # nullable in topic" rule is satisfied.
  bq --project_id="$PROJECT" update "$DATASET.$TABLE" "$BQ_SCHEMA_FILE"
else
  bq --project_id="$PROJECT" mk --table \
    --time_partitioning_type=DAY \
    --time_partitioning_field=server_ts \
    --clustering_fields=event_name,app_id \
    "$DATASET.$TABLE" \
    "$BQ_SCHEMA_FILE"
fi

step "6. grant the Pub/Sub service agent BQ roles"
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:$PUBSUB_AGENT" \
  --role="roles/bigquery.dataEditor" \
  --condition=None --quiet >/dev/null
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:$PUBSUB_AGENT" \
  --role="roles/bigquery.metadataViewer" \
  --condition=None --quiet >/dev/null

step "7. create the BigQuery subscription $BQ_SUB"
exists_or_create \
  "gcloud pubsub subscriptions describe $BQ_SUB --project=$PROJECT" \
  "gcloud pubsub subscriptions create $BQ_SUB \
     --topic=$TOPIC \
     --bigquery-table=$PROJECT:$DATASET.$TABLE \
     --use-topic-schema \
     --drop-unknown-fields \
     --dead-letter-topic=$DLQ_TOPIC \
     --max-delivery-attempts=5 \
     --ack-deadline=30 \
     --message-retention-duration=7d \
     --project=$PROJECT"

step "8. create the DLQ inspection subscription $DLQ_SUB"
exists_or_create \
  "gcloud pubsub subscriptions describe $DLQ_SUB --project=$PROJECT" \
  "gcloud pubsub subscriptions create $DLQ_SUB \
     --topic=$DLQ_TOPIC \
     --ack-deadline=60 \
     --message-retention-duration=7d \
     --project=$PROJECT"

step "9. create the $SECRET secret with a placeholder version"
exists_or_create \
  "gcloud secrets describe $SECRET --project=$PROJECT" \
  "gcloud secrets create $SECRET --replication-policy=automatic --project=$PROJECT && \
   printf '# replace via scripts/mint-write-key.sh; one <app-id>:<phc-hash> per line\n' | \
     gcloud secrets versions add $SECRET --data-file=- --project=$PROJECT"

step "10. grant the VM service account its roles"
# publisher → topics.publish (write events)
# viewer    → topics.get (collector calls topic.Exists() at startup)
gcloud pubsub topics add-iam-policy-binding "$TOPIC" \
  --member="serviceAccount:$VM_SERVICE_ACCOUNT" \
  --role="roles/pubsub.publisher" \
  --project="$PROJECT" --quiet >/dev/null
gcloud pubsub topics add-iam-policy-binding "$TOPIC" \
  --member="serviceAccount:$VM_SERVICE_ACCOUNT" \
  --role="roles/pubsub.viewer" \
  --project="$PROJECT" --quiet >/dev/null
gcloud secrets add-iam-policy-binding "$SECRET" \
  --member="serviceAccount:$VM_SERVICE_ACCOUNT" \
  --role="roles/secretmanager.secretAccessor" \
  --project="$PROJECT" --quiet >/dev/null
echo "  (Memorystore is optional — see compose-redis below; if you use it,"
echo "   grant roles/redis.editor to $VM_SERVICE_ACCOUNT.)"

cat <<EOF

== done ==

Next:
  1. Mint a write key:
       scripts/mint-write-key.sh demo-app prod
     Copy the storage line into the secret:
       echo '<app-id>:\$argon2id...' | gcloud secrets versions add $SECRET --data-file=- --project=$PROJECT

  2. On the VM, set env and run docker compose:
       export GCP_PROJECT=$PROJECT
       export PUBSUB_TOPIC=$TOPIC
       export WRITE_KEYS_SECRET=projects/$PROJECT/secrets/$SECRET/versions/latest
       docker compose up --build -d
EOF
