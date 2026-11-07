package subscriber

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/dentech-floss/logging/pkg/logging"
)

const testPath = "/push-handlers/pubsub/test/created"

// runRouter adds one handler on testPath, runs the router until it's running,
// and returns the paths that were registered on the HTTP mux.
func runRouter(t *testing.T, config *SubscriberConfig) []string {
	t.Helper()

	logger := logging.NewLogger(&logging.LoggerConfig{ServiceName: "test"})

	var mu sync.Mutex
	var registered []string
	register := func(pattern string, _ http.Handler) {
		mu.Lock()
		defer mu.Unlock()
		registered = append(registered, pattern)
	}

	sub := NewSubscriber(logger, config, register)

	router := InitTracedRouter(logger)
	router.AddConsumerHandler(
		"test",
		testPath,
		sub,
		func(msg *message.Message) error { return nil },
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- router.Run(ctx) }()

	select {
	case <-router.Running():
	case <-time.After(5 * time.Second):
		t.Fatal("router didn't start")
	}

	// The handlers must keep running: a subscriber whose channels close
	// would stop the router (and the service with it).
	time.Sleep(100 * time.Millisecond)
	if router.IsClosed() {
		t.Fatal("router stopped on its own")
	}

	cancel()
	// The router waits for its handlers, which stop when the subscriber's
	// channels close.
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("router.Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("router didn't stop")
	}

	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), registered...)
}

func TestPubSubRegistersPushHandler(t *testing.T) {
	t.Setenv("BROKER", "")
	got := runRouter(t, &SubscriberConfig{})
	if len(got) != 1 || got[0] != testPath {
		t.Fatalf("registered %v, want [%s]", got, testPath)
	}
}

func TestExplicitPubSub(t *testing.T) {
	t.Setenv("BROKER", BrokerNATS) // the config wins over the environment
	got := runRouter(t, &SubscriberConfig{Broker: BrokerPubSub})
	if len(got) != 1 || got[0] != testPath {
		t.Fatalf("registered %v, want [%s]", got, testPath)
	}
}

func TestNATSRegistersNothing(t *testing.T) {
	t.Setenv("BROKER", BrokerNATS)
	got := runRouter(t, &SubscriberConfig{})
	if len(got) != 0 {
		t.Fatalf("registered %v, want nothing", got)
	}
}

func TestUnknownBrokerPanics(t *testing.T) {
	t.Setenv("BROKER", "kafka")
	defer func() {
		if recover() == nil {
			t.Fatal("no panic for an unknown broker")
		}
	}()
	logger := logging.NewLogger(&logging.LoggerConfig{ServiceName: "test"})
	NewSubscriber(logger, &SubscriberConfig{}, func(string, http.Handler) {})
}

func TestIdleSubscriberClose(t *testing.T) {
	s := newIdleSubscriber()
	ch, err := s.Subscribe(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after Close")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	late, _ := s.Subscribe(context.Background(), "b")
	if _, ok := <-late; ok {
		t.Fatal("Subscribe after Close returned an open channel")
	}
}
