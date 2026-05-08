#!/usr/bin/env bash
# Create the collector's service account and download a JSON key file.
# Idempotent: re-running on an existing SA just generates a new key.
#
# Treat the key file as a secret. Never commit it. After copying to the
# VM, delete the local copy:
#   shred -u collector-vm-key.json   (Linux)
#   rm -P collector-vm-key.json      (macOS)
#
# Defaults (override by exporting before running):
#   PROJECT     letztrip-production-account
#   SA_NAME     collector-vm
#   KEY_FILE    ./collector-vm-key.json
set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
PROJECT="${PROJECT:-letztrip-production-account}"
SA_NAME="${SA_NAME:-collector-vm}"
KEY_FILE="${KEY_FILE:-./collector-vm-key.json}"
SA_EMAIL="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"

echo "→ pinning gcloud config to $PROJECT"
gcloud config set project "$PROJECT" >/dev/null

echo "→ creating service account $SA_EMAIL"
if gcloud iam service-accounts describe "$SA_EMAIL" --project="$PROJECT" >/dev/null 2>&1; then
  echo "  already exists, skipping create"
else
  gcloud iam service-accounts create "$SA_NAME" \
    --display-name="Events collector VM" \
    --project="$PROJECT"
fi

if [ -e "$KEY_FILE" ]; then
  echo "→ refusing to overwrite existing $KEY_FILE — move it aside or set KEY_FILE=" >&2
  exit 1
fi

echo "→ generating key file at $KEY_FILE"
gcloud iam service-accounts keys create "$KEY_FILE" \
  --iam-account="$SA_EMAIL" \
  --project="$PROJECT"

chmod 600 "$KEY_FILE"

cat <<EOF

== done ==

Service account: $SA_EMAIL
Key file:        $KEY_FILE  (chmod 600, treat as a secret)

Next:
  1. Run scripts/bootstrap-gcp.sh with VM_SERVICE_ACCOUNT=$SA_EMAIL to grant
     this SA the right roles (publisher on the topic, accessor on the secret).

  2. Copy the key to the VM:
       scp $KEY_FILE USER@VM:/etc/collector/key.json
     Then on the VM tighten perms:
       sudo chmod 600 /etc/collector/key.json

  3. Delete the local copy after transferring:
       rm -P $KEY_FILE        # macOS
       shred -u $KEY_FILE     # Linux

  4. On the VM, point compose at the key path:
       export GOOGLE_APPLICATION_CREDENTIALS_HOST=/etc/collector/key.json
       export GCP_PROJECT=$PROJECT
       export PUBSUB_TOPIC=events-raw
       export WRITE_KEYS_SECRET=projects/$PROJECT/secrets/write-keys/versions/latest
       docker compose up --build -d
EOF
