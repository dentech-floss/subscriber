package subscriber

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/message"
)

type FakeSubscriber struct {
}

func NewFakeSubscriber() message.Subscriber {
	return &FakeSubscriber{}
}

func (s *FakeSubscriber) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	return nil, nil
}

func (s *FakeSubscriber) Close() error {
	return nil
}
