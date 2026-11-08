package subscriber

import (
	"context"
	"fmt"
	"os"
	"reflect"

	"github.com/dentech-floss/logging/pkg/logging"
	googlecloud_http "github.com/dentech-floss/watermill-googlecloud-http/pkg/googlecloud/http"

	"github.com/ThreeDotsLabs/watermill/message"

	wotelfloss "github.com/dentech-floss/watermill-opentelemetry-go-extra/pkg/opentelemetry"
	wotel "github.com/voi-oss/watermill-opentelemetry/pkg/opentelemetry"

	"google.golang.org/protobuf/proto"
)

const (
	// BrokerPubSub receives messages from Google Cloud Pub/Sub push
	// subscriptions: each topic is an HTTP path registered on the service's
	// mux. This is the default.
	BrokerPubSub = "pubsub"
	// BrokerNATS receives messages from durable NATS JetStream consumers,
	// one per push URL, found through the NATS mapping file. No HTTP push
	// handlers are registered, so the push paths don't exist.
	BrokerNATS = "nats"
)

type SubscriberConfig struct {
	// Broker is BrokerPubSub or BrokerNATS. Defaults to the BROKER
	// environment variable, and to BrokerPubSub if that's unset.
	Broker string
	// Service is the service's name, which picks its consumers in the
	// NATS mapping. Defaults to the APP_NAME environment variable. Only
	// used with BrokerNATS.
	Service string
	// NATS is the connection and mapping file. Only used with BrokerNATS.
	NATS NATSConfig
}

func (c *SubscriberConfig) setDefaults() {
	if c.Broker == "" {
		c.Broker = os.Getenv("BROKER")
	}
	if c.Broker == "" {
		c.Broker = BrokerPubSub
	}
	if c.Service == "" {
		c.Service = os.Getenv("APP_NAME")
	}
	c.NATS.setDefaults()
}

type Subscriber struct {
	message.Subscriber
}

func NewSubscriber(
	logger *logging.Logger,
	config *SubscriberConfig,
	registerHttpHandler googlecloud_http.RegisterHttpHandler,
) *Subscriber {
	config.setDefaults()

	switch config.Broker {
	case BrokerPubSub:
	case BrokerNATS:
		subscriber, err := newNATSSubscriber(logger, config.Service, config.NATS)
		if err != nil {
			panic(fmt.Sprintf("subscriber: BROKER=nats: %v", err))
		}
		return &Subscriber{subscriber}
	default:
		panic(fmt.Sprintf("subscriber: unknown broker %q (BROKER must be %q or %q)", config.Broker, BrokerPubSub, BrokerNATS))
	}

	subscriber, err := googlecloud_http.NewSubscriber(
		googlecloud_http.SubscriberConfig{
			RegisterHttpHandler: registerHttpHandler,
		},
		logging.NewWatermillAdapter(logger),
	)
	if err != nil {
		panic(err)
	}

	return &Subscriber{subscriber}
}

func InitTracedRouter(logger *logging.Logger) *message.Router {
	router, err := message.NewRouter(
		message.RouterConfig{},
		logging.NewWatermillAdapter(logger),
	)
	if err != nil {
		panic(err)
	}

	// First extract a propagated parent trace/span, then start a child span
	router.AddMiddleware(wotelfloss.ExtractRemoteParentSpanContext())
	router.AddMiddleware(wotel.Trace())

	return router
}

// HandleMessage - Handles a message by first unmarshalling its payload to the provided 'target'
// and then invoking the provided handler with the context of the message. If the
// unmarshalling fails then we will never be able to handle this message so we ack
// it to "get rid of it" and return an error. But if the provided handler fails then
// we the message will be nack'ed (to trigger a redelivery) and no error is returned
// (it's up to the provided handler to log this error).
//
// The rules are as follows: To receive the next message, `Ack()` must be called on
// the received message, but if the message processing failed and message should be
// redelivered then `Nack()` shall be called.
func HandleMessage[T proto.Message](
	msg *message.Message,
	handler func(ctx context.Context, target T) error,
) error {
	var zero T
	t := reflect.TypeOf(zero)
	if t.Kind() != reflect.Pointer {
		msg.Ack()
		return fmt.Errorf("generic type T must be a pointer to a proto.Message, got %s", t.Kind())
	}

	elemType := t.Elem()
	target := reflect.New(elemType).Interface().(T)

	err := UnmarshalPayload(msg.Payload, target)
	if err != nil {
		msg.Ack()
		return err
	}

	err = handler(msg.Context(), target) // tracing...
	if err != nil {
		msg.Nack()
		return nil
	}

	msg.Ack()

	return nil
}

func UnmarshalPayload(
	payload []byte,
	target proto.Message,
) error {
	return proto.Unmarshal(payload, target)
}
