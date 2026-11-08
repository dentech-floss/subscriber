# subscriber

Watermill subscriber that is setup to use our custom [dentech-floss/watermill-googlecloud-http](https://github.com/dentech-floss/watermill-googlecloud-http) lib to subscribe to messages delivered by http push subscriptions in GCP. But why push with a custom lib instead of pull using the official [watermill-googlecloud](https://github.com/ThreeDotsLabs/watermill-googlecloud) lib you may ask? That's because we're running on Cloud Run where we are limited to use push subscriptions (and want to be able to scale down to 0 instances).

The subscriber is preconfigured for distributed Opentelemetry tracing. For this we use both the official [watermill-opentelemetry](https://github.com/voi-oss/watermill-opentelemetry) project and our custom complement [dentech-floss/watermill-opentelemetry-go-extra](https://github.com/dentech-floss/watermill-opentelemetry-go-extra) lib to extract a propagated parent span and create a child span for this when we receive a message. With this support we get quite awesome observability of the system, since we can see and follow events flowing through the system in an APM of choice!

## Install

```
go get github.com/dentech-floss/subscriber@v0.3.2
```

## Broker

`SubscriberConfig.Broker` (or the `BROKER` environment variable, if the field is empty) picks where messages come from:

- `pubsub` (default): Google Cloud Pub/Sub push subscriptions. Each topic is an HTTP path registered on your mux, as below.
- `nats`: durable NATS JetStream consumers, see below. **No HTTP push handlers are registered**, so the push paths don't exist.

Any other value panics in `NewSubscriber`. The service code is the same for both: `AddConsumerHandler` with the push URL as the topic, and `HandleMessage` in the handler.

## NATS JetStream

With `BROKER=nats`, each push URL the service subscribes to is bound to a durable pull consumer that **must already exist** on the server (create streams and consumers with your infrastructure code, not from the services). The library never creates or changes streams or consumers.

Settings (each `SubscriberConfig` field falls back to the environment variable):

| Field | Environment | |
| --- | --- | --- |
| `Service` | `APP_NAME` | The service's name, which picks its consumers in the mapping file |
| `NATS.URL` | `NATS_URL` | e.g. `tls://nats.example:4222` |
| `NATS.CAFile` | `NATS_CA_FILE` | Optional PEM file with the server's CA |
| `NATS.User`, `NATS.Password` | `NATS_USER`, `NATS_PASSWORD` | Log in with a user from the server's config |
| `NATS.CredsFile` | `NATS_CREDS_FILE` | Or log in with a JWT + NKey creds file (operator mode). Not together with a user |
| `NATS.MappingFile` | `NATS_MAPPING_FILE` | The mapping file below |

The mapping file binds (service, push URL) to a stream and durable consumer. Other fields in it (for example the consumer settings your infrastructure code uses) are ignored:

```yaml
consumers:
  - service: example-service
    url: /push-handlers/pubsub/appointment/claimed
    stream: APPOINTMENTS
    durable: example-service_appointment-claimed
```

How messages are handled:

- **Settings come from the server:** `MaxDeliver`, `BackOff` and `AckWait` are read from the consumer when the router subscribes.
- **Ack** (the handler returns nil, or `HandleMessage` acks): the message is done.
- **Nack** (the handler returns an error, or `HandleMessage` nacks): the message is redelivered after the consumer's `BackOff` step for that delivery (JetStream itself applies `BackOff` only to messages that aren't acked within `AckWait`, so the library sends the delay with the nack). Without a `BackOff`, at once.
- **Last delivery:** when a nack comes on delivery number `MaxDeliver`, the message is published to `dlq.<service>.<durable>` and acked. The DLQ message keeps the payload, the UUID and the metadata, plus `dlq_subject`, `dlq_stream`, `dlq_consumer`, `dlq_service` and `dlq_deliveries`. A stream must capture `dlq.>`.
- **Metadata:** NATS headers become message metadata (the trace context too, so tracing works across services as with Pub/Sub), plus `_nats_subject` and `_nats_num_delivered`.
- **Fails loudly:** `NewSubscriber` panics on a missing setting, an invalid mapping file or when it can't connect. `router.Run` returns an error if a URL isn't in the mapping or its consumer doesn't exist.
- **Connection:** reconnects forever, so a NATS restart doesn't stop the service. Each subscription has its own connection.

Under the hood this is the [watermill-nats](https://github.com/ThreeDotsLabs/watermill-nats) JetStream subscriber, bound to the existing consumer.

## Usage

Create the subscriber and start subscribing to a topic/url using a router with support for tracing:

```go
package example

import (
    "github.com/dentech-floss/logging/pkg/logging"
    "github.com/dentech-floss/metadata/pkg/metadata"
    "github.com/dentech-floss/revision/pkg/revision"
    "github.com/dentech-floss/subscriber/pkg/subscriber"

    "github.com/ThreeDotsLabs/watermill/message/router/plugin"

    "github.com/go-chi/chi"
)

func main() {

    metadata := metadata.NewMetadata()

    logger := logging.NewLogger(
        &logging.LoggerConfig{
            ServiceName: revision.ServiceName,
            MinLevel:    logging.InfoLevel,
        },
    )
    defer logger.Sync()

    ...

    bqIngestionService := service.NewAppointmentBigQueryIngestionService(repo, logger)

    httpRouter := chi.NewRouter() // it is not necessary to use chi, you can use your mux of choice

    _subscriber := subscriber.NewSubscriber(
        logger, // pass our logger
        &subscriber.SubscriberConfig{}, // BROKER, APP_NAME and NATS_* come from the environment
        httpRouter.Handle, // register the http handler for the topic/url on chi
    )

    // this Watermill router have tracing middleware added to it
    router := subscriber.InitTracedRouter(logger) // adds tracing middleware to the router
    router.AddPlugin(plugin.SignalsHandler) // kills the router after SIGINT or SIGTERM is sent to the process

    router.AddConsumerHandler(
        "pubsub.Subscribe/appointment/claimed", // the name of our handler
        "/push-handlers/pubsub/appointment/claimed", // topic/url we're getting messages pushed to us on
        _subscriber,
        bqIngestionService.HandleAppointmentClaimedEvent, // our handler to invoke
    )

    go func() {
        err := router.Run(ctx)
        if err != nil {
            panic(err)
        }
    }()

    err := http.ListenAndServe(":"+strconv.Itoa(*config.Port), httpRouter)
    if errors.Is(err, http.ErrServerClosed) {
        logger.Info("Server closed")
    } else {
        logger.Error("Server failed", logging.ErrorField(err))
    }
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

    // HandleMessage will take care or marshalling + ack/nack'ing the message for us
    err := subscriber.HandleMessage(msg, s.handleAppointmentClaimedEvent)
    if err != nil {
        s.logger.WithContext(msg.Context()).Error(
            "Failed to unmarshal 'AppointmentClaimedEvent', ack'ed the message to get rid of it",
            logging.StringField("msg_uuid", msg.UUID),
            logging.StringField("payload", string(msg.Payload)),
            logging.ErrorField(err),
        )
    }
    return err
}

func (s *AppointmentBigQueryIngestionService) handleAppointmentClaimedEvent(
  ctx context.Context,
  event *appointment_service_v1.AppointmentEvent,
) error {
        err := s.repo.InsertAppointmentClaimedEvent(ctx, event.GetAppointmentClaimed())
        if err != nil {
            s.logger.WithContext(ctx).Error(
                "Failed to insert 'AppointmentClaimedEvent'",
                logging.StringField("msg_uuid", msg.UUID),
                logging.ProtoField("event", event),
                logging.ErrorField(err),
            )
            return err
        }
        return nil
    }
```
