package subscriber

import (
	googlecloud_http "github.com/dentech-floss/watermill-googlecloud-http/pkg/googlecloud/http"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/garsue/watermillzap"

	wotelfloss "github.com/dentech-floss/watermill-opentelemetry-go-extra/pkg/opentelemetry"
	wotel "github.com/voi-oss/watermill-opentelemetry/pkg/opentelemetry"

	"google.golang.org/protobuf/proto"

	"go.uber.org/zap"
)

type SubscriberConfig struct {
	OnGCP bool
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

	var subscriber message.Subscriber
	var err error

	if config.OnGCP {
		subscriber, err = googlecloud_http.NewSubscriber(
			googlecloud_http.SubscriberConfig{
				RegisterHttpHandler: registerHttpHandler,
			},
			watermillzap.NewLogger(logger),
		)
	} else {
		subscriber = NewFakeSubscriber()
	}
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

func UnmarshalPayload(payload []byte, target proto.Message) error {
	return proto.Unmarshal(payload, target)
}
