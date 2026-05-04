package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
)

// MessageHandler 消息处理器。
//
// 影子流量进入 handler 时 ctx 已经被 WithShadow(true) 标记；handler 内部访问
// SQL / Redis / 出站 RPC 都自动按 ctx 走 _shadow 路径。
type MessageHandler func(ctx context.Context, msg Message) error

// Consumer Kafka 消费者。
//
// 每个 Consumer 实际持两个 reader：
//   - 主 reader 订阅 baseTopic
//   - 影子 reader 订阅 baseTopic + "_shadow"
//
// 影子 reader 拿到消息时，传给 handler 的 ctx 已经 IsShadow=true。两个 reader
// 用独立 GroupID（baseTopic + "_shadow"）防止主 / 影 offset 混淆。
type Consumer struct {
	mainReader   *kafka.Reader
	shadowReader *kafka.Reader
	handler      MessageHandler
	logger       *zap.Logger
}

// ConsumerConfig 消费者配置
type ConsumerConfig struct {
	Brokers        []string
	Topic          string
	GroupID        string
	MinBytes       int
	MaxBytes       int
	MaxWait        time.Duration
	StartOffset    int64
	CommitInterval time.Duration
}

// NewConsumer 创建消费者，同时订阅主 + 影子 topic。
func NewConsumer(config ConsumerConfig, handler MessageHandler, logger *zap.Logger) *Consumer {
	mainReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        config.Brokers,
		Topic:          config.Topic,
		GroupID:        config.GroupID,
		MinBytes:       config.MinBytes,
		MaxBytes:       config.MaxBytes,
		MaxWait:        config.MaxWait,
		StartOffset:    config.StartOffset,
		CommitInterval: config.CommitInterval,
	})
	shadowReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        config.Brokers,
		Topic:          config.Topic + shadow.Suffix,
		GroupID:        config.GroupID + shadow.Suffix, // 独立 group 防 offset 混
		MinBytes:       config.MinBytes,
		MaxBytes:       config.MaxBytes,
		MaxWait:        config.MaxWait,
		StartOffset:    config.StartOffset,
		CommitInterval: config.CommitInterval,
	})

	return &Consumer{
		mainReader:   mainReader,
		shadowReader: shadowReader,
		handler:      handler,
		logger:       logger,
	}
}

// Start 同时启动主 + 影子两个消费 goroutine。任何一个 ctx done 都会一起退出。
func (c *Consumer) Start(ctx context.Context) error {
	c.logger.Info("kafka consumer started (main + shadow)",
		zap.String("main_topic", c.mainReader.Config().Topic),
		zap.String("shadow_topic", c.shadowReader.Config().Topic),
		zap.String("groupID", c.mainReader.Config().GroupID),
	)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		c.consumeLoop(ctx, c.mainReader, false /*isShadow*/)
	}()
	go func() {
		defer wg.Done()
		c.consumeLoop(ctx, c.shadowReader, true /*isShadow*/)
	}()

	wg.Wait()
	c.logger.Info("kafka consumer stopped (both readers)")
	return ctx.Err()
}

// consumeLoop 单个 reader 的消费循环。isShadow 决定传给 handler 的 ctx 是否标 shadow。
func (c *Consumer) consumeLoop(ctx context.Context, reader *kafka.Reader, isShadow bool) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := c.consumeMessage(ctx, reader, isShadow); err != nil {
				c.logger.Error("consume message failed",
					zap.Bool("shadow", isShadow), zap.Error(err))
				// 短暂休眠后重试，同时监听 ctx 取消信号以支持快速优雅退出
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// consumeMessage 消费单条消息。msgCtx 在传给 handler 前按 isShadow 标记。
func (c *Consumer) consumeMessage(ctx context.Context, reader *kafka.Reader, isShadow bool) error {
	kafkaMsg, err := reader.FetchMessage(ctx)
	if err != nil {
		return fmt.Errorf("fetch message failed: %w", err)
	}

	// 解析消息
	var msg Message
	if err := json.Unmarshal(kafkaMsg.Value, &msg); err != nil {
		c.logger.Error("unmarshal message failed",
			zap.Error(err),
			zap.Bool("shadow", isShadow),
			zap.String("value", string(kafkaMsg.Value)),
		)
		// 提交 offset，跳过错误消息
		_ = reader.CommitMessages(ctx, kafkaMsg)
		return nil
	}

	// 给 handler 一个带 shadow flag 的 ctx，让下游 SQL / Redis / RPC 自动走影子路径
	msgCtx := ctx
	if isShadow {
		msgCtx = shadow.WithShadow(ctx, true)
	}

	c.logger.Debug("message received",
		zap.String("key", msg.Key),
		zap.String("type", msg.Type),
		zap.Bool("shadow", isShadow),
		zap.Int64("offset", kafkaMsg.Offset),
	)

	// 处理消息
	if err := c.handler(msgCtx, msg); err != nil {
		c.logger.Error("handle message failed",
			zap.Error(err),
			zap.Bool("shadow", isShadow),
			zap.String("key", msg.Key),
			zap.String("type", msg.Type),
		)
		// 处理失败不提交 offset，下次重新消费
		return fmt.Errorf("handle message failed: %w", err)
	}

	// 提交 offset
	if err := reader.CommitMessages(ctx, kafkaMsg); err != nil {
		c.logger.Error("commit message failed",
			zap.Error(err),
			zap.Bool("shadow", isShadow),
			zap.Int64("offset", kafkaMsg.Offset),
		)
		return fmt.Errorf("commit message failed: %w", err)
	}

	return nil
}

// Close 关闭主 + 影子两个 reader。
func (c *Consumer) Close() error {
	var firstErr error
	if err := c.mainReader.Close(); err != nil {
		c.logger.Error("close main reader failed", zap.Error(err))
		firstErr = err
	}
	if err := c.shadowReader.Close(); err != nil {
		c.logger.Error("close shadow reader failed", zap.Error(err))
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
