package kafka

import (
	"context"
	"encoding/json"

	"github.com/twmb/franz-go/pkg/kgo"
)

type Producer struct {
	cl *kgo.Client
}

func NewProducer(brokers []string) *Producer {
	cl, _ := kgo.NewClient(kgo.SeedBrokers(brokers...))
	return &Producer{cl: cl}
}

func (p *Producer) Send(topic string, v interface{}) {

	b, _ := json.Marshal(v)

	p.cl.Produce(context.Background(), &kgo.Record{
		Topic: topic,
		Value: b,
	}, nil)
}
