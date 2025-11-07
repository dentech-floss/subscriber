package subscriber

import (
	"context"
	"fmt"
	"reflect"

	googlecloud_http "github.com/dentech-floss/watermill-googlecloud-http/pkg/googlecloud/http"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/garsue/watermillzap"

	wotelfloss "github.com/dentech-floss/watermill-opentelemetry-go-extra/pkg/opentelemetry"
	wotel "github.com/voi-oss/watermill-opentelemetry/pkg/opentelemetry"

	"google.golang.org/protobuf/proto"

	"go.uber.org/zap"
)

type SubscriberConfig struct {
	// Nada here atm...
}

func (c *SubscriberConfig) setDefaults() {
	// Nada to do here atm...
}

type Subscriber struct {
	message.Subscriber
}

func NewSubscriber(
	logger *zap.Logger,
	config *SubscriberConfig,
	registerHttpHandler googlecloud_http.RegisterHttpHandler,
) *Subscriber {
	config.setDefaults()

	subscriber, err := googlecloud_http.NewSubscriber(
		googlecloud_http.SubscriberConfig{
			RegisterHttpHandler: registerHttpHandler,
		},
		watermillzap.NewLogger(logger),
	)
	if err != nil {
		panic(err)
	}

	return &Subscriber{subscriber}
}

func InitTracedRouter(logger *zap.Logger) *message.Router {
	router, err := message.NewRouter(message.RouterConfig{}, watermillzap.NewLogger(logger))
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
