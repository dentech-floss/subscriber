# subscriber

Watermill subscriber that is setup to use our custom [dentech-floss/watermill-googlecloud-http](https://github.com/dentech-floss/watermill-googlecloud-http) lib to subscribe to messages delivered by http push subscriptions in GCP. How come push instead of pull using the "watermill-googlecloud" you may ask? That's because we're running on Cloud Run (which limits us to use push subscriptions) and want to be able to scale down to 0 instances.

The subscriber is preconfigured for distributed Opentelemetry tracing. For this we use both the official [watermill-opentelemetry](https://github.com/voi-oss/watermill-opentelemetry) project and our custom complement [dentech-floss/watermill-opentelemetry-go-extra](https://github.com/dentech-floss/watermill-opentelemetry-go-extra) to extract a propagated parent span and create a child span for this when we receive a message. With this support we get quite awesome observability of the system, since we can see and follow events flowing through the system in an APM of choice!

## Install

```
go get github.com/dentech-floss/subscriber@v0.1.0
```

## Usage

Create the subscriber:

```go
package example

import (
    "github.com/dentech-floss/logging/pkg/logging"
    "github.com/dentech-floss/subscriber/pkg/subscriber"

    appointment_service_v1 "go.buf.build/dentechse/go-grpc-gateway-openapiv2/dentechse/service-definitions/api/appointment/v1"

    "github.com/go-chi/chi"
)

func main() {
    logger := logging.NewLogger(&logging.LoggerConfig{})
    defer logger.Sync()

    httpRouter := chi.NewRouter() // it is not necessary to use chi, you can use your mux of choice

    _subscriber := subscriber.NewSubscriber(
        logger.Logger.Logger, // the *zap.Logger is wrapped like a matryoshka doll :)
        &subscriber.SubscriberConfig{
            OnGCP: true,
        },
        httpRouter.Handle, // register the http handler for the topic/url on chi
    )

    ...
```

Start subscribing to a topic (a url in this case since we're using http push) using a router with support for tracing:

```go
    ...

    // this Watermill router has centralized tracing support setup for us
    router := subscriber.InitTracedRouter(logger.Logger.Logger) // the *zap.Logger is wrapped like a matryoshka doll :)

    // this Watermill router has centralized tracing support setup for us
	router.AddNoPublisherHandler(
        "pubsub.Subscribe/appointment/claimed", // the name of our handler
        "/push-handlers/pubsub/appointment/claimed", // topic/url we're getting messages pushed to us on
        _subscriber,
        func(msg *message.Message) error {

            // To receive the next message, `Ack()` must be called on the received message.
            // If message processing failed and message should be redelivered `Nack()` should be called.

            ctx := msg.Context() // will contain the trace/span
            logWithContext := logger.WithContext(ctx) // To ensure trace information is part of the logs

            event := &appointment_service_v1.AppointmentEvent{}
            err := subscriber.UnmarshalPayload(msg.Payload, event)
            if err != nil {
                logWithContext.Error("Failed to unmarshal message", logging.ErrorField(err))

                // We will never be able to handle this message so we don't want to nack it because
                // then it will be redelivered so just log and ack to get rid of it instead.

                msg.Ack()
                return nil
            }

            // pass on the ctx for continued tracing...
            err = someService.ActOnAppointmentClaimed(ctx, event.GetAppointmentClaimed())
            if err != nil {
                msg.Nack()
                return err
            }

            msg.Ack()
            return nil
        },
    )

    ...
}
```