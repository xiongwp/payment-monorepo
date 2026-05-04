package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
)

// Producer Kafka 生产者。
//
// 主 / 影子流量按 ctx 路由到不同 topic（base / base_shadow）：
//
//   - Writer 不设 Topic（保留为空），让每条 kafka.Message 自带 Topic
//   - SendMessage / SendBatch 内部 shadow.KafkaTopic(ctx, baseTopic) 切换
//
// 影子流量的 ledger entry 不会进主 topic，下游消费方按订阅哪个 topic 决定
// 是否处理压测数据。
type Producer struct {
	writer    *kafka.Writer
	baseTopic string // 主 topic 名；影子流量自动加 _shadow 后缀
	logger    *zap.Logger
}

// ProducerConfig 生产者配置
type ProducerConfig struct {
	Brokers      []string
	Topic        string
	BatchSize    int
	BatchTimeout time.Duration
	MaxAttempts  int
}

// NewProducer 创建生产者。Writer.Topic 留空，由每条 message.Topic 决定主 / 影路由。
func NewProducer(config ProducerConfig, logger *zap.Logger) *Producer {
	writer := &kafka.Writer{
		Addr:         kafka.TCP(config.Brokers...),
		// Topic 留空：让每条 message 上自带 Topic（主 / 影路由用）
		Balancer:     &kafka.Hash{}, // 使用 Hash 分区策略
		BatchSize:    config.BatchSize,
		BatchTimeout: config.BatchTimeout,
		MaxAttempts:  config.MaxAttempts,
		Async:        false, // 同步发送，确保消息可靠性
	}

	return &Producer{
		writer:    writer,
		baseTopic: config.Topic,
		logger:    logger,
	}
}

// Message 消息
type Message struct {
	Key       string      `json:"key"`
	Type      string      `json:"type"`
	Data      interface{} `json:"data"`
	Timestamp int64       `json:"timestamp"`
}

// SendMessage 发送消息。topic 由 ctx 决定（主 / 影）。
func (p *Producer) SendMessage(ctx context.Context, key string, msgType string, data interface{}) error {
	msg := Message{
		Key:       key,
		Type:      msgType,
		Data:      data,
		Timestamp: time.Now().Unix(),
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		p.logger.Error("marshal message failed", zap.Error(err))
		return fmt.Errorf("marshal message failed: %w", err)
	}

	topic := shadow.KafkaTopic(ctx, p.baseTopic)
	kafkaMsg := kafka.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: msgBytes,
		Time:  time.Now(),
	}

	if err := p.writer.WriteMessages(ctx, kafkaMsg); err != nil {
		p.logger.Error("send message failed",
			zap.Error(err),
			zap.String("key", key),
			zap.String("type", msgType),
			zap.String("topic", topic),
		)
		return fmt.Errorf("send message failed: %w", err)
	}

	p.logger.Debug("message sent",
		zap.String("key", key),
		zap.String("type", msgType),
		zap.String("topic", topic),
	)

	return nil
}

// SendBatch 批量发送消息。整批走同一 ctx 的 topic（不允许混发主 / 影）。
func (p *Producer) SendBatch(ctx context.Context, messages []Message) error {
	topic := shadow.KafkaTopic(ctx, p.baseTopic)
	kafkaMessages := make([]kafka.Message, len(messages))

	for i, msg := range messages {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			p.logger.Error("marshal message failed", zap.Error(err), zap.Int("index", i))
			return fmt.Errorf("marshal message failed at index %d: %w", i, err)
		}

		kafkaMessages[i] = kafka.Message{
			Topic: topic,
			Key:   []byte(msg.Key),
			Value: msgBytes,
			Time:  time.Now(),
		}
	}

	if err := p.writer.WriteMessages(ctx, kafkaMessages...); err != nil {
		p.logger.Error("send batch messages failed",
			zap.Error(err),
			zap.Int("count", len(messages)),
			zap.String("topic", topic),
		)
		return fmt.Errorf("send batch messages failed: %w", err)
	}

	p.logger.Debug("batch messages sent", zap.Int("count", len(messages)), zap.String("topic", topic))

	return nil
}

// Close 关闭生产者
func (p *Producer) Close() error {
	if err := p.writer.Close(); err != nil {
		p.logger.Error("close producer failed", zap.Error(err))
		return err
	}
	return nil
}
