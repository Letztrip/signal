package main

import (
	"context"
	"testing"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// newOrderedTopic returns a topic on an in-memory Pub/Sub with ordering on, as
// main.go configures it, plus the fake server and client so a test can create
// or inspect topics.
func newOrderedTopic(t *testing.T, create bool) (*pubsub.Client, *pubsub.Topic, *pstest.Server) {
	t.Helper()
	ctx := context.Background()

	srv := pstest.NewServer()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake pubsub: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client, err := pubsub.NewClient(ctx, "test-project", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatalf("pubsub client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	topic := client.Topic("events")
	if create {
		if topic, err = client.CreateTopic(ctx, "events"); err != nil {
			t.Fatalf("create topic: %v", err)
		}
	}
	topic.EnableMessageOrdering = true
	t.Cleanup(topic.Stop)
	return client, topic, srv
}

func msgFor(key string) *pubsub.Message {
	return &pubsub.Message{Data: []byte(`{}`), OrderingKey: key}
}

// INTERNAL-133. A visitor closing the tab cancels the request context. The
// publish used to inherit it, the library's flow-control acquire failed on the
// cancelled context and paused that visitor's ordering key — so every later
// event from them failed with ErrPublishingPaused on that pod until a restart.
func TestPublishAllSurvivesAClientThatHungUp(t *testing.T) {
	_, topic, srv := newOrderedTopic(t, true)
	s := &Server{topic: topic}

	gone, cancel := context.WithCancel(context.Background())
	cancel() // the client hung up before the handler reached publish

	if err := s.publishAll(gone, []*pubsub.Message{msgFor("anon-1")}); err != nil {
		t.Fatalf("publish after client hang-up: %v", err)
	}
	// And the visitor's next request is not stuck behind a paused key.
	if err := s.publishAll(context.Background(), []*pubsub.Message{msgFor("anon-1")}); err != nil {
		t.Fatalf("next publish for the same visitor: %v", err)
	}
	if got := len(srv.Messages()); got != 2 {
		t.Fatalf("delivered %d messages, want 2", got)
	}
}

// A publish that genuinely fails also pauses its key. Resuming it lets the
// client's retry through instead of failing that visitor until the pod restarts.
func TestPublishAllResumesAKeyAfterAFailedPublish(t *testing.T) {
	ctx := context.Background()
	client, topic, _ := newOrderedTopic(t, false) // topic missing: publish fails NotFound
	s := &Server{topic: topic}

	if err := s.publishAll(ctx, []*pubsub.Message{msgFor("anon-2")}); err == nil {
		t.Fatal("publish to a missing topic succeeded; the test needs a failure")
	}

	if _, err := client.CreateTopic(ctx, "events"); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	if err := s.publishAll(ctx, []*pubsub.Message{msgFor("anon-2")}); err != nil {
		t.Fatalf("retry for the same key after the failure: %v", err)
	}
}

// Every failed key in a batch is resumed, not just the first one hit.
func TestPublishAllResumesEveryFailedKeyInABatch(t *testing.T) {
	ctx := context.Background()
	client, topic, _ := newOrderedTopic(t, false)
	s := &Server{topic: topic}

	batch := []*pubsub.Message{msgFor("anon-a"), msgFor("anon-b"), msgFor("anon-c")}
	if err := s.publishAll(ctx, batch); err == nil {
		t.Fatal("batch to a missing topic succeeded; the test needs a failure")
	}

	if _, err := client.CreateTopic(ctx, "events"); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	for _, key := range []string{"anon-a", "anon-b", "anon-c"} {
		if err := s.publishAll(ctx, []*pubsub.Message{msgFor(key)}); err != nil {
			t.Errorf("key %s still paused after the failed batch: %v", key, err)
		}
	}
}
