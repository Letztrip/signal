package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
	"google.golang.org/protobuf/proto"

	"github.com/example/event-pipeline/collector/eventpb"
)

func TestProtoRoundTrip(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	user := "u_42"
	ev := &Event{
		EventID:     "e_1",
		EventName:   "page_viewed",
		UserID:      &user,
		AnonymousID: "a_1",
		SessionID:   "s_1",
		ClientTS:    "2026-05-08T12:00:00.000Z",
		Properties:  json.RawMessage(`{"k":"v","n":7}`),
		Context:     json.RawMessage(`{"platform":"web","app_version":"1.0.0","sdk_version":"0.1.0","locale":"en-US"}`),
	}
	enr := newEnricher(httptest.NewRequest("POST", "/v1/events", nil), "demo-app")
	enr.Now = time.Date(2026, 5, 8, 12, 0, 1, 0, time.UTC)

	pb := buildProto(ev, enr, log)

	wire, err := proto.Marshal(pb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := &eventpb.AnalyticsEvent{}
	if err := proto.Unmarshal(wire, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.GetEventId() != "e_1" {
		t.Errorf("event_id: got %q", got.GetEventId())
	}
	if got.GetEventName() != "page_viewed" {
		t.Errorf("event_name: got %q", got.GetEventName())
	}
	if got.GetUserId() != "u_42" {
		t.Errorf("user_id: got %q", got.GetUserId())
	}
	if got.GetAppId() != "demo-app" {
		t.Errorf("app_id: got %q", got.GetAppId())
	}
	if got.GetPlatform() != "web" {
		t.Errorf("platform: got %q", got.GetPlatform())
	}
	if got.GetAppVersion() != "1.0.0" {
		t.Errorf("app_version: got %q", got.GetAppVersion())
	}
	if got.GetIngestVersion() != "v1" {
		t.Errorf("ingest_version: got %q", got.GetIngestVersion())
	}

	var props map[string]any
	if err := json.Unmarshal([]byte(got.GetProperties()), &props); err != nil {
		t.Fatalf("properties not valid JSON: %v", err)
	}
	if props["k"] != "v" {
		t.Errorf("properties.k: got %v", props["k"])
	}
	if got.GetClientTs() == "" {
		t.Errorf("client_ts not populated")
	} else if _, err := time.Parse(time.RFC3339Nano, got.GetClientTs()); err != nil {
		t.Errorf("client_ts not RFC3339: %q (%v)", got.GetClientTs(), err)
	}
	wantServerTS := enr.Now.UTC().Format(time.RFC3339Nano)
	if got.GetServerTs() != wantServerTS {
		t.Errorf("server_ts: got %q want %q", got.GetServerTs(), wantServerTS)
	}
}

func TestParseKeyStoreVerify(t *testing.T) {
	const plaintext = "wk_test_abc"
	salt := []byte("saltstartshere")
	const m, tCost = uint32(65536), uint32(3)
	const p = uint8(2)
	derived := argon2.IDKey([]byte(plaintext), salt, tCost, m, p, 32)

	enc := base64.RawStdEncoding
	phc := "$argon2id$v=19$m=65536,t=3,p=2$" + enc.EncodeToString(salt) + "$" + enc.EncodeToString(derived)
	secret := "demo-app:" + phc + "\n# trailing comment\n"

	ks, err := ParseKeyStore(secret)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	app, ok := ks.Verify(plaintext)
	if !ok || app != "demo-app" {
		t.Fatalf("verify: ok=%v app=%q", ok, app)
	}
	if _, ok := ks.Verify("wrong-key"); ok {
		t.Fatalf("verify accepted wrong key")
	}
}
