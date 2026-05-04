package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// MessageHandler 消息处理器
type MessageHandler func(ctx context.Context, msg Message) error

// Consumer Kafka消费者
type Consumer struct {
	reader  *kafka.Reader
	handler MessageHandler
	logger  *zap.Logger
}

// ConsumerConfig 消费者配置
type ConsumerConfig struct {
	Brokers       []string
	Topic         string
	GroupID       string
	MinBytes      int
	MaxBytes      int
	MaxWait       time.Duration
	StartOffset   int64
	CommitInterval time.Duration
}

// NewConsumer 创建消费者
func NewConsumer(config ConsumerConfig, handler MessageHandler, logger *zap.Logger) *Consumer {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        config.Brokers,
		Topic:          config.Topic,
		GroupID:        config.GroupID,
		MinBytes:       config.MinBytes,
		MaxBytes:       config.MaxBytes,
		MaxWait:        config.MaxWait,
		StartOffset:    config.StartOffset,
		CommitInterval: config.CommitInterval,
	})

	return &Consumer{
		reader:  reader,
		handler: handler,
		logger:  logger,
	}
}

// Start 启动消费者
func (c *Consumer) Start(ctx context.Context) error {
	c.logger.Info("kafka consumer started",
		zap.String("topic", c.reader.Config().Topic),
		zap.String("groupID", c.reader.Config().GroupID),
	)

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("kafka consumer stopped")
			return ctx.Err()
		default:
			if err := c.consumeMessage(ctx); err != nil {
				c.logger.Error("consume message failed", zap.Error(err))
				// 短暂休眠后重试，同时监听 ctx 取消信号以支持快速优雅退出
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					c.logger.Info("kafka consumer stopped during backoff")
					return ctx.Err()
				}
			}
		}
	}
}

// consumeMessage 消费单条消息
func (c *Consumer) consumeMessage(ctx context.Context) error {
	kafkaMsg, err := c.reader.FetchMessage(ctx)
	if err != nil {
		return fmt.Errorf("fetch message failed: %w", err)
	}

	// 解析消息
	var msg Message
	if err := json.Unmarshal(kafkaMsg.Value, &msg); err != nil {
		c.logger.Error("unmarshal message failed",
			zap.Error(err),
			zap.String("value", string(kafkaMsg.Value)),
		)
		// 提交offset，跳过错误消息
		c.reader.CommitMessages(ctx, kafkaMsg)
		return nil
	}

	c.logger.Debug("message received",
		zap.String("key", msg.Key),
		zap.String("type", msg.Type),
		zap.Int64("offset", kafkaMsg.Offset),
	)

	// 处理消息
	if err := c.handler(ctx, msg); err != nil {
		c.logger.Error("handle message failed",
			zap.Error(err),
			zap.String("key", msg.Key),
			zap.String("type", msg.Type),
		)
		// 处理失败不提交offset，下次重新消费
		return fmt.Errorf("handle message failed: %w", err)
	}

	// 提交offset
	if err := c.reader.CommitMessages(ctx, kafkaMsg); err != nil {
		c.logger.Error("commit message failed",
			zap.Error(err),
			zap.Int64("offset", kafkaMsg.Offset),
		)
		return fmt.Errorf("commit message failed: %w", err)
	}

	return nil
}

// Close 关闭消费者
func (c *Consumer) Close() error {
	if err := c.reader.Close(); err != nil {
		c.logger.Error("close consumer failed", zap.Error(err))
		return err
	}
	return nil
}
