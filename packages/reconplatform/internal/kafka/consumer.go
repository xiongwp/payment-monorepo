package kafka

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"
)

func NewConsumer(brokers []string, topics ...string) *kgo.Client {

	cl, _ := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(topics...),
		kgo.ConsumerGroup("reconcile"),
	)

	return cl
}

func Consume(cl *kgo.Client, handler func([]byte)) {

	for {
		fetches := cl.PollFetches(context.Background())

		fetches.EachRecord(func(r *kgo.Record) {
			handler(r.Value)
		})
	}
}
