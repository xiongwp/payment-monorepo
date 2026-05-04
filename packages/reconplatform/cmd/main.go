package main

import (
	"context"
	"encoding/json"
	"log"

	"reconcile-system/internal/engine"
	"reconcile-system/internal/kafka"
	"reconcile-system/internal/model"
	"reconcile-system/internal/rule"
	"reconcile-system/internal/store"
)

func main() {

	store := store.New("localhost:6379")

	ruleEngine := rule.New()
	ruleEngine.Update("r1", "order.amount == payment.amount")
	ruleEngine.Update("r2", "order.amount > 0")

	engine := engine.New(store, ruleEngine)

	producer := kafka.NewProducer([]string{"localhost:9092"})
	consumer := kafka.NewConsumer([]string{"localhost:9092"}, "order", "payment")

	ctx := context.Background()
	go kafka.Consume(consumer, func(msg []byte) {

		var ev model.Event
		if err := json.Unmarshal(msg, &ev); err != nil {
			return
		}

		engine.Handle(ctx, ev)
	})

	for r := range engine.Output() {
		log.Println(r)
		producer.Send("reconcile_result", r)
	}
}
