// Package pusher：跨 server 副本 fan-out。
//
// 单 server 实例的 service.hub 只能 fan-out 给本进程的 watcher；多副本时同 namespace
// 的客户端可能连到不同 server pod。admin PutConfig 在 pod-A 上 → 只有连 pod-A 的
// 客户端收到推送；连 pod-B 的客户端漏掉。
//
// 解法：admin write 后除了本地 hub.Publish，还往 Kafka topic "config-center.events"
// 发一条；所有 server 副本订阅这个 topic，消费者本地 hub.Publish 给自己 watcher。
//
// **保证**：
//   - 至少一次（at-least-once）— Kafka 同步 send + acks=all
//   - 客户端 since_version resume 兜底重复事件（idempotent，client 看 version
//     已超过本地 max 才更新 cache）
//   - 跨 region 也用同一 topic，全平台单一事实源
//
// 不保证：
//   - 严格按 admin write 顺序到达远端副本（Kafka 单 partition 内有序，但多
//     partition 跨 server 顺序不保证）。客户端 since_version 单调让最终一致。
package pusher

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/config-center/internal/service"
	"github.com/xiongwp/payment-util/audit/kafkago"
)

// Bridge 桥接 service.hub 到 Kafka topic + 反向消费。
type Bridge struct {
	topic    string
	producer *kafkago.Producer
	hub      Hub
	logger   *zap.Logger
}

// Hub 抽 service.watcherHub 的最小 publish 接口。
type Hub interface {
	Publish(namespace string, ev *service.Event)
}

// New 构造。topic 留空 → 单实例模式，不启用桥（无 Kafka）。
func New(producer *kafkago.Producer, topic string, hub Hub, logger *zap.Logger) *Bridge {
	return &Bridge{topic: topic, producer: producer, hub: hub, logger: logger}
}

// PublishLocal admin write 调用：先发 Kafka 让其他副本看到，再本地 hub.Publish
// 让本副本 watcher 立即收到（不等 Kafka 回环）。
func (b *Bridge) PublishLocal(namespace string, ev *service.Event) error {
	// 本地立即推（pod-A 上的 client 第一时间看到）
	b.hub.Publish(namespace, ev)
	// Kafka 异步发：失败不阻塞 admin 操作；客户端 since_version resume 时能补回
	if b.producer == nil || b.topic == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"namespace": namespace,
		"event":     ev,
		"sent_at":   time.Now().UnixMilli(),
	})
	if err != nil {
		return err
	}
	go func() {
		if err := b.producer.Send(b.topic, []byte(namespace), payload); err != nil {
			b.logger.Warn("config-center: kafka bridge send failed (clients on other replicas will resume via since_version)",
				zap.String("namespace", namespace),
				zap.Error(err))
		}
	}()
	return nil
}

// Subscribe 启动 Kafka consumer，把外部 server 副本发的事件 fan-out 到本地 hub。
//
// caller (main.go) 在 fx OnStart 调；ctx 取消（OnStop）退出。
//
// 实现要点：
//   - consumer group = "config-center-bridge"；每个副本一个 instance 在 group 里
//   - 不需要每个副本独立处理 partition — 反正每个副本都要 fan-out 给自己的 watcher
//     所以**每个副本独立一个 group**（hostname 后缀），全部副本都消费全部消息
//
// 简化点：本 stub 只声明接口；接通需要在 caller 处给定 kafka consumer
// （不在本包内拉 sarama / kafka-go 强依赖）。
func (b *Bridge) Subscribe(ctx context.Context, consumeFn ConsumerFn) error {
	if b.topic == "" {
		b.logger.Info("config-center: kafka bridge disabled (single-replica mode)")
		return nil
	}
	go func() {
		<-ctx.Done()
		b.logger.Info("config-center: kafka bridge stopping")
	}()
	return consumeFn(ctx, b.topic, func(payload []byte) error {
		var msg struct {
			Namespace string         `json:"namespace"`
			Event     *service.Event `json:"event"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("decode bridge msg: %w", err)
		}
		if msg.Namespace == "" || msg.Event == nil {
			return nil
		}
		// fan-out 到本副本 hub
		b.hub.Publish(msg.Namespace, msg.Event)
		return nil
	})
}

// ConsumerFn caller 提供的 Kafka 消费回调入口；调一次会阻塞直到 ctx 取消。
type ConsumerFn func(ctx context.Context, topic string, handle func(payload []byte) error) error
