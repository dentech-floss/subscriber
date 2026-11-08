package subscriber

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wmnats "github.com/ThreeDotsLabs/watermill-nats/v2/pkg/nats"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/dentech-floss/logging/pkg/logging"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	testService = "test-service"
	testStream  = "EVENTS"
	testDurable = "test-service_test-created"
	testSubject = "events.test.created"
	testDLQ     = "dlq." + testService + "." + testDurable
)

type natsEnv struct {
	url     string
	mapping string
	js      jetstream.JetStream
}

// setupNATS starts a JetStream server with the stream, the DLQ stream and
// one durable consumer (as OpenTofu would), and writes a mapping file that
// binds testPath of testService to that consumer.
func setupNATS(t *testing.T, consumer jetstream.ConsumerConfig) natsEnv {
	t.Helper()

	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      testStream,
		Subjects:  []string{"events.>"},
		Retention: jetstream.InterestPolicy,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "DLQ",
		Subjects: []string{"dlq.>"},
	}); err != nil {
		t.Fatal(err)
	}
	consumer.Durable = testDurable
	consumer.AckPolicy = jetstream.AckExplicitPolicy
	consumer.FilterSubject = testSubject
	if _, err := js.CreateConsumer(ctx, testStream, consumer); err != nil {
		t.Fatal(err)
	}

	mapping := filepath.Join(t.TempDir(), "mapping.yaml")
	writeFile(t, mapping, `
consumers:
  - service: `+testService+`
    url: `+testPath+`
    stream: `+testStream+`
    durable: `+testDurable+`
    filter_subject: `+testSubject+` # for OpenTofu, ignored here
    max_deliver: 5
  - service: other-service
    url: `+testPath+`
    stream: `+testStream+`
    durable: other-service_test-created
`)
	return natsEnv{url: srv.ClientURL(), mapping: mapping, js: js}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e natsEnv) config() *SubscriberConfig {
	return &SubscriberConfig{
		Broker:  BrokerNATS,
		Service: testService,
		NATS:    NATSConfig{URL: e.url, MappingFile: e.mapping},
	}
}

// publish sends a message the way the publisher library does.
func (e natsEnv) publish(t *testing.T, uuid, payload string, metadata map[string]string) {
	t.Helper()
	msg := message.NewMessage(uuid, []byte(payload))
	for k, v := range metadata {
		msg.Metadata.Set(k, v)
	}
	nm, err := (&wmnats.NATSMarshaler{}).Marshal(testSubject, msg)
	if err != nil {
		t.Fatal(err)
	}
	nm.Header.Set(nats.MsgIdHdr, uuid)
	if _, err := e.js.PublishMsg(context.Background(), nm); err != nil {
		t.Fatal(err)
	}
}

// runHandler runs a traced router with one handler on testPath until the
// test ends. It fails the test if a push handler is registered.
func runHandler(t *testing.T, config *SubscriberConfig, handler message.NoPublishHandlerFunc) {
	t.Helper()
	logger := logging.NewLogger(&logging.LoggerConfig{ServiceName: "test"})
	sub := NewSubscriber(logger, config, func(pattern string, _ http.Handler) {
		t.Errorf("registered push handler %s", pattern)
	})
	router := InitTracedRouter(logger)
	router.AddConsumerHandler("test", testPath, sub, handler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- router.Run(ctx) }()
	select {
	case <-router.Running():
	case err := <-done:
		cancel()
		t.Fatalf("router.Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("router didn't start")
	}
	t.Cleanup(func() {
		cancel()
		if err := sub.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("router didn't stop")
		}
	})
}

// runError runs a router that is expected to fail to start, and returns
// its error.
func runError(t *testing.T, config *SubscriberConfig) error {
	t.Helper()
	logger := logging.NewLogger(&logging.LoggerConfig{ServiceName: "test"})
	sub := NewSubscriber(logger, config, func(string, http.Handler) {})
	defer sub.Close()
	router := InitTracedRouter(logger)
	router.AddConsumerHandler("test", testPath, sub, func(*message.Message) error { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return router.Run(ctx)
}

// waitSettled waits until the consumer has nothing pending or unacked.
func (e natsEnv) waitSettled(t *testing.T) *jetstream.ConsumerInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := e.js.Consumer(context.Background(), testStream, testDurable)
		if err != nil {
			t.Fatal(err)
		}
		info, err := c.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.NumPending == 0 && info.NumAckPending == 0 && info.Delivered.Consumer > 0 {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumer not settled: pending %d, ack pending %d", info.NumPending, info.NumAckPending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNATSDelivery(t *testing.T) {
	env := setupNATS(t, jetstream.ConsumerConfig{})

	got := make(chan *message.Message, 10)
	runHandler(t, env.config(), func(msg *message.Message) error {
		got <- msg
		return nil
	})

	env.publish(t, "uuid-1", "payload-1", map[string]string{
		"event_type": "EVENT_TYPE_CREATED",
		"trace_id":   "0af7651916cd43dd8448eb211c80319c",
		"span_id":    "b7ad6b7169203331",
	})

	select {
	case msg := <-got:
		if msg.UUID != "uuid-1" || string(msg.Payload) != "payload-1" {
			t.Errorf("got %s %q", msg.UUID, msg.Payload)
		}
		for k, want := range map[string]string{
			"event_type":             "EVENT_TYPE_CREATED",
			"trace_id":               "0af7651916cd43dd8448eb211c80319c",
			MetadataNATSSubject:      testSubject,
			MetadataNATSNumDelivered: "1",
		} {
			if v := msg.Metadata.Get(k); v != want {
				t.Errorf("metadata %s = %q, want %q", k, v, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}

	info := env.waitSettled(t)
	if info.Delivered.Consumer != 1 {
		t.Errorf("delivered %d times, want 1", info.Delivered.Consumer)
	}
	select {
	case msg := <-got:
		t.Errorf("acked message delivered again: %s", msg.UUID)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNATSRedeliveryAfterBackOff(t *testing.T) {
	backoff := 400 * time.Millisecond
	env := setupNATS(t, jetstream.ConsumerConfig{
		MaxDeliver: 3,
		BackOff:    []time.Duration{backoff, backoff},
	})

	var mu sync.Mutex
	var times []time.Time
	done := make(chan struct{})
	runHandler(t, env.config(), func(msg *message.Message) error {
		mu.Lock()
		defer mu.Unlock()
		times = append(times, time.Now())
		if len(times) == 1 {
			return errors.New("first try fails")
		}
		close(done)
		return nil
	})
	env.publish(t, "uuid-2", "payload", nil)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not redelivered")
	}
	env.waitSettled(t)
	mu.Lock()
	defer mu.Unlock()
	if gap := times[1].Sub(times[0]); gap < backoff-50*time.Millisecond {
		t.Errorf("redelivered after %s, want the BackOff of %s", gap, backoff)
	}
}

func TestNATSDeadLetterAfterMaxDeliver(t *testing.T) {
	env := setupNATS(t, jetstream.ConsumerConfig{
		MaxDeliver: 2,
		BackOff:    []time.Duration{100 * time.Millisecond},
	})

	var mu sync.Mutex
	calls := 0
	runHandler(t, env.config(), func(msg *message.Message) error {
		mu.Lock()
		calls++
		mu.Unlock()
		msg.Nack() // like HandleMessage when the handler fails
		return nil
	})
	env.publish(t, "uuid-3", "bad payload", map[string]string{"event_type": "EVENT_TYPE_CREATED"})

	dlq, err := env.js.CreateConsumer(context.Background(), "DLQ", jetstream.ConsumerConfig{
		FilterSubject: testDLQ,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := dlq.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var dead jetstream.Msg
	for m := range batch.Messages() {
		dead = m
	}
	if dead == nil {
		t.Fatal("nothing on the DLQ subject")
	}
	if string(dead.Data()) != "bad payload" {
		t.Errorf("DLQ payload %q", dead.Data())
	}
	h := dead.Headers()
	for k, want := range map[string]string{
		wmnats.WatermillUUIDHdr: "uuid-3",
		"event_type":            "EVENT_TYPE_CREATED",
		"dlq_subject":           testSubject,
		"dlq_stream":            testStream,
		"dlq_consumer":          testDurable,
		"dlq_service":           testService,
		"dlq_deliveries":        "2",
	} {
		if v := h.Get(k); v != want {
			t.Errorf("DLQ header %s = %q, want %q", k, v, want)
		}
	}
	for k := range h {
		if strings.HasPrefix(k, natsMetadataPrefix) {
			t.Errorf("DLQ message has internal header %s", k)
		}
	}

	env.waitSettled(t) // the original is acked once it's on the DLQ
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("handler called %d times, want MaxDeliver = 2", calls)
	}
}

func TestNATSMessageWithoutWatermillHeaders(t *testing.T) {
	env := setupNATS(t, jetstream.ConsumerConfig{})

	got := make(chan *message.Message, 1)
	runHandler(t, env.config(), func(msg *message.Message) error {
		got <- msg
		return nil
	})

	// Published with the nats CLI or another client: no Watermill UUID,
	// and a header with two values.
	nm := nats.NewMsg(testSubject)
	nm.Data = []byte("raw")
	nm.Header.Set(nats.MsgIdHdr, "msg-id-1")
	nm.Header.Add("event_type", "first")
	nm.Header.Add("event_type", "second")
	if _, err := env.js.PublishMsg(context.Background(), nm); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-got:
		if msg.UUID != "msg-id-1" {
			t.Errorf("UUID %q, want the Nats-Msg-Id", msg.UUID)
		}
		if v := msg.Metadata.Get("event_type"); v != "first" {
			t.Errorf("event_type %q, want the first value", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
	env.waitSettled(t)
}

func TestNATSMissingConsumerFailsToStart(t *testing.T) {
	env := setupNATS(t, jetstream.ConsumerConfig{})
	config := env.config()
	config.Service = "other-service" // mapped, but OpenTofu made no consumer
	err := runError(t, config)
	if err == nil || !strings.Contains(err.Error(), "other-service_test-created") {
		t.Fatalf("router.Run: %v, want an error naming the missing consumer", err)
	}
}

func TestNATSUnmappedURLFailsToStart(t *testing.T) {
	env := setupNATS(t, jetstream.ConsumerConfig{})
	config := env.config()
	config.Service = "unknown-service"
	err := runError(t, config)
	if err == nil || !strings.Contains(err.Error(), "no consumer for service") {
		t.Fatalf("router.Run: %v, want an unmapped-URL error", err)
	}
}

func TestNATSConfigFromEnvironment(t *testing.T) {
	env := setupNATS(t, jetstream.ConsumerConfig{})
	t.Setenv("BROKER", "nats")
	t.Setenv("APP_NAME", testService)
	t.Setenv("NATS_URL", env.url)
	t.Setenv("NATS_MAPPING_FILE", env.mapping)

	got := make(chan *message.Message, 1)
	runHandler(t, &SubscriberConfig{}, func(msg *message.Message) error {
		got <- msg
		return nil
	})
	env.publish(t, "uuid-env", "payload", nil)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func TestNATSConfigErrorsPanic(t *testing.T) {
	env := setupNATS(t, jetstream.ConsumerConfig{})
	badMapping := filepath.Join(t.TempDir(), "bad.yaml")
	writeFile(t, badMapping, "consumers:\n  - service: a\n    url: /x\n")

	for name, tc := range map[string]struct {
		mutate func(*SubscriberConfig)
		want   string
	}{
		"no service":       {func(c *SubscriberConfig) { c.Service = "" }, "APP_NAME"},
		"no URL":           {func(c *SubscriberConfig) { c.NATS.URL = "" }, "NATS_URL"},
		"no mapping":       {func(c *SubscriberConfig) { c.NATS.MappingFile = "" }, "NATS_MAPPING_FILE"},
		"user and creds":   {func(c *SubscriberConfig) { c.NATS.User, c.NATS.Password, c.NATS.CredsFile = "u", "p", "/creds" }, "not both"},
		"user no password": {func(c *SubscriberConfig) { c.NATS.User = "u" }, "NATS_PASSWORD"},
		"bad mapping":      {func(c *SubscriberConfig) { c.NATS.MappingFile = badMapping }, "needs service, url, stream and durable"},
		"server down":      {func(c *SubscriberConfig) { c.NATS.URL = "nats://127.0.0.1:1" }, "connecting to NATS"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, v := range []string{"APP_NAME", "NATS_URL", "NATS_USER", "NATS_PASSWORD", "NATS_CREDS_FILE", "NATS_MAPPING_FILE", "NATS_CA_FILE"} {
				t.Setenv(v, "")
			}
			config := env.config()
			tc.mutate(config)
			defer func() {
				r := recover()
				if r == nil || !strings.Contains(r.(string), tc.want) {
					t.Fatalf("panic %v, want one mentioning %q", r, tc.want)
				}
			}()
			logger := logging.NewLogger(&logging.LoggerConfig{ServiceName: "test"})
			NewSubscriber(logger, config, func(string, http.Handler) {})
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	d := backoffDelay{time.Second, time.Minute}
	for delivered, want := range map[uint64]time.Duration{0: time.Second, 1: time.Second, 2: time.Minute, 9: time.Minute} {
		if got := d.WaitTime(delivered); got != want {
			t.Errorf("WaitTime(%d) = %s, want %s", delivered, got, want)
		}
	}
	if got := (backoffDelay(nil)).WaitTime(3); got != 0 {
		t.Errorf("no BackOff: %s, want 0 (nak at once)", got)
	}
}
