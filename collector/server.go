package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"cloud.google.com/go/pubsub"
	"google.golang.org/protobuf/proto"

	"github.com/example/event-pipeline/collector/eventpb"
)

const (
	maxBatchBytes  = 1 << 20 // 1 MB
	maxBatchSize   = 100
	publishTimeout = 10 * time.Second
)

type Batch struct {
	Batch []json.RawMessage `json:"batch"`
}

// Event mirrors the SDK envelope. Server-side this is only used for parsing
// the incoming JSON before flattening into the Protobuf wire shape.
type Event struct {
	EventID     string          `json:"event_id"`
	EventName   string          `json:"event_name"`
	UserID      *string         `json:"user_id,omitempty"`
	AnonymousID string          `json:"anonymous_id"`
	SessionID   string          `json:"session_id"`
	ClientTS    string          `json:"client_ts"`
	Properties  json.RawMessage `json:"properties,omitempty"`
	Context     json.RawMessage `json:"context"`
}

type Server struct {
	validator *Validator
	topic     *pubsub.Topic
	idem      IdempStore
	keys      *KeyManager
	geo       *geoResolver
	log       *slog.Logger
}

func NewServer(v *Validator, topic *pubsub.Topic, idem IdempStore, keys *KeyManager, geo *geoResolver, log *slog.Logger) *Server {
	return &Server{validator: v, topic: topic, idem: idem, keys: keys, geo: geo, log: log}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := requestIDFromCtx(ctx)
	appID := appIDFromCtx(ctx)

	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		cached, hit, err := s.idem.Get(ctx, appID, idemKey)
		if err != nil {
			s.log.Warn("idem get failed", "err", err, "request_id", reqID)
		} else if hit {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Idempotent-Replay", "1")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write(cached)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBatchBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "batch_too_large", "body exceeds 1 MB", reqID)
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid_json", "failed to read body", reqID)
		return
	}

	var batch Batch
	if err := json.Unmarshal(body, &batch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "body not parseable", reqID)
		return
	}
	n := len(batch.Batch)
	if n == 0 || n > maxBatchSize {
		writeErr(w, http.StatusBadRequest, "invalid_batch_size", "batch must contain 1..100 events", reqID)
		return
	}

	enr := newEnricher(r, appID, s.geo)
	results := make([]*pubsub.PublishResult, 0, n)

	for i, raw := range batch.Batch {
		if err := s.validator.Validate(raw); err != nil {
			writeErr(w, http.StatusBadRequest, "schema_violation", fmt.Sprintf("event[%d]: %s", i, err), reqID)
			return
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			writeErr(w, http.StatusBadRequest, "schema_violation", fmt.Sprintf("event[%d]: decode failed", i), reqID)
			return
		}
		propsRaw := extractField(raw, "properties")
		if hit := scanPII(propsRaw); hit != "" {
			s.log.Warn("pii rejected", "event_index", i, "path", hit, "request_id", reqID, "app_id", appID)
			piiRejectedTotal.Inc()
			writeErr(w, http.StatusBadRequest, "pii_violation", fmt.Sprintf("event[%d]: pii match at %s", i, hit), reqID)
			return
		}

		pb := buildProto(&ev, enr, s.log)
		data, err := proto.Marshal(pb)
		if err != nil {
			s.log.Error("proto marshal", "err", err, "request_id", reqID)
			captureErr(ctx, err)
			writeErr(w, http.StatusInternalServerError, "internal", "marshal", reqID)
			return
		}
		pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		res := s.topic.Publish(pubCtx, &pubsub.Message{
			Data:        data,
			OrderingKey: ev.AnonymousID,
			Attributes: map[string]string{
				"event_name": ev.EventName,
				"app_id":     appID,
				"platform":   pb.GetPlatform(),
			},
		})
		cancel()
		results = append(results, res)
	}

	for _, res := range results {
		pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		_, err := res.Get(pubCtx)
		cancel()
		if err != nil {
			s.log.Error("publish failed", "err", err, "request_id", reqID)
			captureErr(ctx, err)
			eventsPublishedTotal.WithLabelValues("error").Inc()
			writeErr(w, http.StatusServiceUnavailable, "publisher_saturated", "publisher buffer unavailable", reqID)
			return
		}
	}

	eventsReceivedTotal.Add(float64(n))
	eventsPublishedTotal.WithLabelValues("ok").Add(float64(n))

	respBody, _ := json.Marshal(struct {
		Accepted  int    `json:"accepted"`
		RequestID string `json:"request_id"`
	}{Accepted: n, RequestID: reqID})

	if idemKey != "" {
		if err := s.idem.Set(ctx, appID, idemKey, respBody); err != nil {
			s.log.Warn("idem set failed", "err", err, "request_id", reqID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(respBody)
}

// buildProto flattens an SDK Event into the wire-format AnalyticsEvent.
// `properties` and `context` are JSON-stringified here, server-side, so the
// SDKs never have to think about it.
func buildProto(ev *Event, enr *enricher, log *slog.Logger) *eventpb.AnalyticsEvent {
	return &eventpb.AnalyticsEvent{
		EventId:       ev.EventID,
		EventName:     ev.EventName,
		UserId:        deref(ev.UserID),
		AnonymousId:   ev.AnonymousID,
		SessionId:     ev.SessionID,
		ClientTs:      parseTime(ev.ClientTS, enr.Now, log).UTC().Format(time.RFC3339Nano),
		ServerTs:      enr.Now.UTC().Format(time.RFC3339Nano),
		Platform:      stringFromContext(ev.Context, "platform"),
		AppVersion:    stringFromContext(ev.Context, "app_version"),
		SdkVersion:    stringFromContext(ev.Context, "sdk_version"),
		AppId:         enr.AppID,
		UaFamily:      enr.UA.Name,
		UaOs:          enr.UA.OS,
		GeoCountry:    enr.Country,
		GeoRegion:     enr.Region,
		GeoCity:       enr.City,
		GeoLat:        enr.Lat,
		GeoLon:        enr.Lon,
		IngestVersion: ingestVersion,
		Properties:    mustJSONString(ev.Properties),
		Context:       mustJSONString(ev.Context),
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func parseTime(s string, fallback time.Time, log *slog.Logger) time.Time {
	if s == "" {
		log.Warn("clock_skew_total", "reason", "empty_client_ts")
		return fallback
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		log.Warn("clock_skew_total", "reason", "unparseable_client_ts", "value", s, "err", err)
		return fallback
	}
	return t
}

// stringFromContext pulls a top-level string field out of the raw context blob.
// Only used for Pub/Sub message attributes / the flat columns we surface; the
// full context still goes through verbatim as a JSON string.
func stringFromContext(ctxRaw json.RawMessage, key string) string {
	if len(ctxRaw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(ctxRaw, &m); err != nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// mustJSONString returns valid JSON text. Empty / invalid input becomes "{}".
func mustJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	// Round-trip to ensure the stored text is canonical JSON.
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// extractField pulls a top-level JSON field as RawMessage. Returns nil if absent.
func extractField(raw json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m[key]
}
