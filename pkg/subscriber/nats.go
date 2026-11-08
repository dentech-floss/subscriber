package subscriber

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wmjs "github.com/ThreeDotsLabs/watermill-nats/v2/pkg/jetstream"
	wmnats "github.com/ThreeDotsLabs/watermill-nats/v2/pkg/nats"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/dentech-floss/logging/pkg/logging"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Metadata keys that the NATS subscriber adds to each received message.
const (
	// MetadataNATSSubject is the subject the message was published on.
	MetadataNATSSubject = "_nats_subject"
	// MetadataNATSNumDelivered is how many times JetStream has delivered
	// the message, starting at 1.
	MetadataNATSNumDelivered = "_nats_num_delivered"

	natsMetadataPrefix = "_nats_"
)

// natsSubscriber receives from durable JetStream consumers that already
// exist (OpenTofu creates them). Each Subscribe binds one push URL of the
// service to its consumer through the mapping file.
//
// Handlers get a copy of each message. Acking the copy acks the message.
// Nacking it redelivers the message after the consumer's BackOff, except on
// the last delivery (MaxDeliver): then the message is published to the
// service's DLQ subject and acked.
type natsSubscriber struct {
	logger   *logging.Logger
	wmLogger watermill.LoggerAdapter
	service  string
	config   NATSConfig
	mapping  *natsMapping

	// nc looks up consumers and publishes to the DLQ. Each subscription
	// has its own connection, because the watermill-nats subscriber
	// drains its connection on Close.
	nc *nats.Conn
	js jetstream.JetStream

	mu      sync.Mutex
	closed  bool
	closing chan struct{}
	inner   []*wmjs.Subscriber
	wg      sync.WaitGroup
}

func newNATSSubscriber(logger *logging.Logger, service string, config NATSConfig) (*natsSubscriber, error) {
	if service == "" {
		return nil, errors.New("the service name is empty: set SubscriberConfig.Service or APP_NAME")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	mapping, err := loadNATSMapping(config.MappingFile)
	if err != nil {
		return nil, err
	}
	nc, err := config.connect(service)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	wmLogger := logging.NewWatermillAdapter(logger)
	return &natsSubscriber{
		logger:   logger,
		wmLogger: quietInfoLogger{wmLogger},
		service:  service,
		config:   config,
		mapping:  mapping,
		nc:       nc,
		js:       js,
		closing:  make(chan struct{}),
	}, nil
}

func (s *natsSubscriber) Subscribe(ctx context.Context, url string) (<-chan *message.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("subscriber is closed")
	}

	b, ok := s.mapping.binding(s.service, url)
	if !ok {
		return nil, fmt.Errorf("the NATS mapping has no consumer for service %q and %q", s.service, url)
	}

	// The consumer's settings live on the server, where OpenTofu put them.
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	consumer, err := s.js.Consumer(lookupCtx, b.Stream, b.Durable)
	if err != nil {
		return nil, fmt.Errorf("JetStream consumer %s > %s for %s (streams and consumers are created by OpenTofu): %w", b.Stream, b.Durable, url, err)
	}
	cfg := consumer.CachedInfo().Config
	if cfg.AckPolicy != jetstream.AckExplicitPolicy {
		return nil, fmt.Errorf("JetStream consumer %s > %s must use explicit acks, has %s", b.Stream, b.Durable, cfg.AckPolicy)
	}

	nc, err := s.config.connect(s.service + " " + b.Durable)
	if err != nil {
		return nil, err
	}
	inner, err := wmjs.NewSubscriber(wmjs.SubscriberConfig{
		Conn:           nc,
		Logger:         s.wmLogger,
		AckWaitTimeout: cfg.AckWait,
		ResourceInitializer: func(ctx context.Context, js jetstream.JetStream, _ string) (jetstream.Consumer, func(context.Context, watermill.LoggerAdapter), error) {
			c, err := js.Consumer(ctx, b.Stream, b.Durable)
			return c, nil, err
		},
		NakDelay:    backoffDelay(cfg.BackOff),
		Unmarshaler: natsUnmarshaler{},
	})
	if err != nil {
		nc.Close()
		return nil, err
	}
	in, err := inner.Subscribe(ctx, url)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	s.inner = append(s.inner, inner)

	out := make(chan *message.Message)
	s.wg.Add(1)
	go s.forward(in, out, b, cfg.MaxDeliver)
	return out, nil
}

// forward hands a copy of each message to the router and settles the
// original when the handler has acked or nacked the copy.
func (s *natsSubscriber) forward(in <-chan *message.Message, out chan<- *message.Message, b natsBinding, maxDeliver int) {
	defer s.wg.Done()
	defer close(out)
	for m := range in {
		c := m.CopyWithContext()
		select {
		case out <- c:
		case <-s.closing:
			return
		}
		select {
		case <-c.Acked():
			m.Ack()
		case <-c.Nacked():
			s.nacked(m, b, maxDeliver)
		case <-s.closing:
			return
		}
	}
}

func (s *natsSubscriber) nacked(m *message.Message, b natsBinding, maxDeliver int) {
	delivered, _ := strconv.Atoi(m.Metadata.Get(MetadataNATSNumDelivered))
	if maxDeliver <= 0 || delivered < maxDeliver {
		m.Nack() // redelivered after the consumer's BackOff
		return
	}
	fields := []any{
		logging.StringField("msg_uuid", m.UUID),
		logging.StringField("subject", m.Metadata.Get(MetadataNATSSubject)),
		logging.StringField("consumer", b.Durable),
		logging.StringField("dlq_subject", b.dlqSubject()),
	}
	if err := s.publishDLQ(m, b, delivered); err != nil {
		// JetStream won't deliver it again (MaxDeliver), so it stays in
		// the stream until MaxAge without being handled.
		s.logger.Error("Failed to publish to the DLQ after the last delivery; the message is not handled",
			append(fields, logging.ErrorField(err))...)
		m.Nack()
		return
	}
	s.logger.Warn("Handler failed on the last delivery, moved the message to the DLQ", fields...)
	m.Ack()
}

func (s *natsSubscriber) publishDLQ(m *message.Message, b natsBinding, delivered int) error {
	dm := message.NewMessage(m.UUID, m.Payload)
	for k, v := range m.Metadata {
		if !strings.HasPrefix(k, natsMetadataPrefix) {
			dm.Metadata.Set(k, v)
		}
	}
	dm.Metadata.Set("dlq_subject", m.Metadata.Get(MetadataNATSSubject))
	dm.Metadata.Set("dlq_stream", b.Stream)
	dm.Metadata.Set("dlq_consumer", b.Durable)
	dm.Metadata.Set("dlq_service", s.service)
	dm.Metadata.Set("dlq_deliveries", strconv.Itoa(delivered))

	nm, err := (&wmnats.NATSMarshaler{}).Marshal(b.dlqSubject(), dm)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = s.js.PublishMsg(ctx, nm)
	return err
}

func (s *natsSubscriber) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closing)
	inner := s.inner
	s.mu.Unlock()

	var errs []error
	for _, in := range inner {
		errs = append(errs, in.Close())
	}
	s.wg.Wait()
	s.nc.Close()
	return errors.Join(errs...)
}

// backoffDelay is the consumer's BackOff applied to nacks: JetStream itself
// uses BackOff only for messages that aren't acked in time, and redelivers
// a plain nack at once.
type backoffDelay []time.Duration

func (d backoffDelay) WaitTime(numDelivered uint64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(numDelivered) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(d) {
		i = len(d) - 1
	}
	return d[i]
}

// natsReservedHeaders aren't copied to the message metadata.
var natsReservedHeaders = map[string]bool{
	wmnats.WatermillUUIDHdr:     true,
	nats.MsgIdHdr:               true,
	nats.ExpectedLastMsgIdHdr:   true,
	nats.ExpectedStreamHdr:      true,
	nats.ExpectedLastSubjSeqHdr: true,
	nats.ExpectedLastSeqHdr:     true,
}

// natsUnmarshaler never fails: watermill-nats drops into a nil message on an
// unmarshal error. Headers become metadata (the first value of each), and
// the subject and delivery count are added.
type natsUnmarshaler struct{}

func (natsUnmarshaler) Unmarshal(msg jetstream.Msg) (*message.Message, error) {
	hdr := msg.Headers()
	id := hdr.Get(wmnats.WatermillUUIDHdr)
	if id == "" {
		id = hdr.Get(nats.MsgIdHdr)
	}
	if id == "" {
		id = watermill.NewUUID()
	}
	m := message.NewMessage(id, msg.Data())
	for k, v := range hdr {
		if !natsReservedHeaders[k] && len(v) > 0 {
			m.Metadata.Set(k, v[0])
		}
	}
	m.Metadata.Set(MetadataNATSSubject, msg.Subject())
	if md, err := msg.Metadata(); err == nil {
		m.Metadata.Set(MetadataNATSNumDelivered, strconv.FormatUint(md.NumDelivered, 10))
	}
	return m, nil
}

// quietInfoLogger logs watermill-nats' per-message Info lines ("got msg") at
// Debug, so busy consumers don't flood the logs.
type quietInfoLogger struct {
	watermill.LoggerAdapter
}

func (l quietInfoLogger) Info(msg string, fields watermill.LogFields) {
	l.LoggerAdapter.Debug(msg, fields)
}

func (l quietInfoLogger) With(fields watermill.LogFields) watermill.LoggerAdapter {
	return quietInfoLogger{l.LoggerAdapter.With(fields)}
}
