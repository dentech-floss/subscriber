# subscriber

Watermill subscriber that is setup to use our custom [dentech-floss/watermill-googlecloud-http](https://github.com/dentech-floss/watermill-googlecloud-http) lib to subscribe to messages delivered by http push subscriptions in GCP. But why push with a custom lib instead of pull using the official [watermill-googlecloud](https://github.com/ThreeDotsLabs/watermill-googlecloud) lib you may ask? That's because we're running on Cloud Run where we are limited to use push subscriptions (and want to be able to scale down to 0 instances).

The subscriber is preconfigured for distributed Opentelemetry tracing. For this we use both the official [watermill-opentelemetry](https://github.com/voi-oss/watermill-opentelemetry) project and our custom complement [dentech-floss/watermill-opentelemetry-go-extra](https://github.com/dentech-floss/watermill-opentelemetry-go-extra) lib to extract a propagated parent span and create a child span for this when we receive a message. With this support we get quite awesome observability of the system, since we can see and follow events flowing through the system in an APM of choice!

## Install

```
go get github.com/dentech-floss/subscriber@v0.1.1
```

## Usage

Create the subscriber and start subscribing to a topic/url using a router with support for tracing:

```go
package example

import (
    "github.com/dentech-floss/logging/pkg/logging"
    "github.com/dentech-floss/metadata/pkg/metadata"
    "github.com/dentech-floss/revision/pkg/revision"
    "github.com/dentech-floss/subscriber/pkg/subscriber"

    "github.com/go-chi/chi"
)

func main() {

    metadata := metadata.NewMetadata()

    logger := logging.NewLogger(
        &logging.LoggerConfig{
            OnGCP:       metadata.OnGCP,
            ServiceName: revision.ServiceName,
        },
    )
    defer logger.Sync() // flushes buffer, if any

    service := service.NewAppointmentBigQueryIngestionService(logger)

    httpRouter := chi.NewRouter() // it is not necessary to use chi, you can use your mux of choice

    _subscriber := subscriber.NewSubscriber(
        logger.Logger.Logger, // the *zap.Logger is wrapped like a matryoshka doll :)
        &subscriber.SubscriberConfig{}, // nothing required to provide here atm
        httpRouter.Handle, // register the http handler for the topic/url on chi
    )

    // this Watermill router have tracing middleware added to it
    router := subscriber.InitTracedRouter(logger.Logger.Logger) // the *zap.Logger is wrapped like a matryoshka doll :)

    router.AddNoPublisherHandler(
        "pubsub.Subscribe/appointment/claimed", // the name of our handler
        "/push-handlers/pubsub/appointment/claimed", // topic/url we're getting messages pushed to us on
        _subscriber,
        service.HandleAppointmentClaimedEvent, // our handler to invoke
    )

    ...
}
```

Handle the Watermill message by unmarshalling the payload and Ack/Nack the message:

```go
package example

import (
    "github.com/dentech-floss/logging/pkg/logging"
    "github.com/dentech-floss/subscriber/pkg/subscriber"

    appointment_service_v1 "go.buf.build/dentechse/go-grpc-gateway-openapiv2/dentechse/service-definitions/api/appointment/v1"
)

...

func (s *AppointmentBigQueryIngestionService) HandleAppointmentClaimedEvent(msg *message.Message) error {

    event := &appointment_service_v1.AppointmentEvent{}
    // HandleMessage will take care or marhshalling + ack/nack'ing the message for us
    err := subscriber.HandleMessage(msg, request, func(ctx context.Context) error {
        err := s.repo.InsertAppointmentClaimedEvent(ctx, event.GetAppointmentClaimed())
        if err != nil {
            s.logger.WithContext(ctx).Error(
                "Failed to insert 'AppointmentClaimedEvent'",
                logging.StringField("msg_uuid", msg.UUID),
                logging.ProtoField("request", request),
                logging.ErrorField(err),
            )
            return err
        }
        return nil
    },
    )
    if err != nil {
        s.logger.WithContext(msg.Context()).Error(
            "Failed to unmarshal 'AppointmentClaimedEvent', ack'ed the message get rid of it",
            logging.StringField("msg_uuid", msg.UUID),
            logging.StringField("payload", string(msg.Payload)),
            logging.ErrorField(err),
        )
    }
    return err
}
```