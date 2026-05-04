package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Producer Kafka生产者
type Producer struct {
	writer *kafka.Writer
	logger *zap.Logger
}

// ProducerConfig 生产者配置
type ProducerConfig struct {
	Brokers      []string
	Topic        string
	BatchSize    int
	BatchTimeout time.Duration
	MaxAttempts  int
}

// NewProducer 创建生产者
func NewProducer(config ProducerConfig, logger *zap.Logger) *Producer {
	writer := &kafka.Writer{
		Addr:         kafka.TCP(config.Brokers...),
		Topic:        config.Topic,
		Balancer:     &kafka.Hash{}, // 使用Hash分区策略
		BatchSize:    config.BatchSize,
		BatchTimeout: config.BatchTimeout,
		MaxAttempts:  config.MaxAttempts,
		Async:        false, // 同步发送，确保消息可靠性
	}

	return &Producer{
		writer: writer,
		logger: logger,
	}
}

// Message 消息
type Message struct {
	Key   string      `json:"key"`
	Type  string      `json:"type"`
	Data  interface{} `json:"data"`
	Timestamp int64    `json:"timestamp"`
}

// SendMessage 发送消息
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

	kafkaMsg := kafka.Message{
		Key:   []byte(key),
		Value: msgBytes,
		Time:  time.Now(),
	}

	if err := p.writer.WriteMessages(ctx, kafkaMsg); err != nil {
		p.logger.Error("send message failed",
			zap.Error(err),
			zap.String("key", key),
			zap.String("type", msgType),
		)
		return fmt.Errorf("send message failed: %w", err)
	}

	p.logger.Debug("message sent",
		zap.String("key", key),
		zap.String("type", msgType),
	)

	return nil
}

// SendBatch 批量发送消息
func (p *Producer) SendBatch(ctx context.Context, messages []Message) error {
	kafkaMessages := make([]kafka.Message, len(messages))

	for i, msg := range messages {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			p.logger.Error("marshal message failed", zap.Error(err), zap.Int("index", i))
			return fmt.Errorf("marshal message failed at index %d: %w", i, err)
		}

		kafkaMessages[i] = kafka.Message{
			Key:   []byte(msg.Key),
			Value: msgBytes,
			Time:  time.Now(),
		}
	}

	if err := p.writer.WriteMessages(ctx, kafkaMessages...); err != nil {
		p.logger.Error("send batch messages failed",
			zap.Error(err),
			zap.Int("count", len(messages)),
		)
		return fmt.Errorf("send batch messages failed: %w", err)
	}

	p.logger.Debug("batch messages sent", zap.Int("count", len(messages)))

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
