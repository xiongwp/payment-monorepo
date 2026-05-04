package kafka

import (
	"context"
	"log"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
)

// shadowSuffix 与 payment-util/shadow.Suffix 一致；reconplatform 不直接 import
// payment-util（保持对账平台与业务包解耦），这里硬编码同样的契约。
const shadowSuffix = "_shadow"

// NewConsumer 创建 Kafka consumer，订阅给定 topics。
//
// **shadow 隔离**：reconplatform 是对账平台，不参与压测流量对账（避免污染主对账
// 报表 + 浪费规则引擎算力）。本函数会**自动过滤掉**任何以 _shadow 结尾的 topic
// 名，并打 WARN 日志 — 这是防御性设计，应对运维 / config 失误把 shadow topic
// 误塞到订阅列表的场景。
//
// 真要让 reconplatform 跑 shadow 对账（独立报表场景）必须改这里 + 改下游
// 输出 topic 名（reconcile_result_shadow），不要走主流量同一份 result 流。
func NewConsumer(brokers []string, topics ...string) *kgo.Client {
	filtered := make([]string, 0, len(topics))
	for _, t := range topics {
		if strings.HasSuffix(t, shadowSuffix) {
			log.Printf("recon kafka: dropping shadow topic %q from subscription "+
				"(reconplatform does not reconcile load-test traffic)", t)
			continue
		}
		filtered = append(filtered, t)
	}

	cl, _ := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(filtered...),
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
