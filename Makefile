SHELL := /bin/bash

.PHONY: proto up down logs test bq-count bq-sample

proto:
	cd schemas && protoc --go_out=../collector --go_opt=paths=source_relative event.proto
	mkdir -p collector/eventpb && mv collector/event.pb.go collector/eventpb/

up:
	docker compose up --build

down:
	docker compose down

logs:
	docker compose logs -f collector

test:
	cd collector && go test ./...

# Real BigQuery — uses the bq CLI with the project from $GCP_PROJECT.
bq-count:
	bq query --use_legacy_sql=false --project_id=$$GCP_PROJECT \
	  'SELECT event_name, COUNT(*) c FROM analytics.events WHERE server_ts > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR) GROUP BY 1 ORDER BY c DESC'

bq-sample:
	bq query --use_legacy_sql=false --project_id=$$GCP_PROJECT \
	  'SELECT * FROM analytics.events ORDER BY server_ts DESC LIMIT 1'
