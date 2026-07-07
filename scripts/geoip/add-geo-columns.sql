-- Adds city-level GeoIP columns to analytics.events (proto tags 18-21).
-- All NULLABLE (required-ness lives in schemas/events.v1.json, never in BQ).
-- Idempotent: IF NOT EXISTS makes re-runs safe. Old rows get NULL.
-- Run in the BigQuery console query editor, or: bq query --use_legacy_sql=false < this file.
ALTER TABLE `letztrip-production-account.analytics.events`
  ADD COLUMN IF NOT EXISTS geo_region STRING,
  ADD COLUMN IF NOT EXISTS geo_city   STRING,
  ADD COLUMN IF NOT EXISTS geo_lat    STRING,
  ADD COLUMN IF NOT EXISTS geo_lon    STRING;
