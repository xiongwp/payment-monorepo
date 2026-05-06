// Package kafkago 把 segmentio/kafka-go.Writer 包成各服务 audit emitter 用的
// `Producer` 接口。
//
// 抽象意图：服务侧 audit 包定义自己的最小接口（`Send(topic, key, value) error`）；
// 这里只提供一个能 satisfy 该接口的 thin adapter。
//
// 使用模式：
//
//	w := kafkago.New(kafkago.Config{
//	    Brokers: []string{"kafka-1:9092", "kafka-2:9092"},
//	    Acks:    "all",   // PCI Req 10：审计不能丢
//	    BatchTimeout: 100 * time.Millisecond,
//	})
//	defer w.Close()
//	emitter := audit.New(w, "card-center.audit", metaDB, logger)
//
// 配置取舍：
//   - RequiredAcks=AcksAll：审计行不接受丢失（vs. metrics 类可以 acks=1）
//   - WriteTimeout=5s：远超正常 P99（< 50ms），故障期早断早报警
//   - Async=false：失败必须能反馈到 audit 链路 fallback DB-only
//   - Compression=Lz4：审计 row JSON 压缩比 ~ 70%，吞吐换 broker 存储
package kafkago

import (
	"context"
	"errors"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Config 暴露给上层 service 配置。零值有合理默认。
type Config struct {
	Brokers      []string
	Acks         string        // "all" | "one" | "none"；空 → "all"（PCI 默认最稳）
	WriteTimeout time.Duration // 单条 send timeout；空 → 5s
	BatchTimeout time.Duration // batch flush 间隔；空 → 100ms
	BatchBytes   int64         // batch 上限字节；空 → 1MB
	BatchSize    int           // batch 上限条数；空 → 100
	Compression  string        // "lz4" | "snappy" | "gzip" | ""；空 → "lz4"
}

// Producer 实现各 service 的 KafkaProducer 接口（Send(topic, key, value)）。
// kafka-go 的 Writer 自带连接池、重试、batch flush；这里只挂一层失败日志。
type Producer struct {
	w      *kafka.Writer
	logger *zap.Logger
}

// New 构造。brokers 空 → 返 (nil, error)；调用方按 nil producer 走 DB-only 路径。
func New(cfg Config, logger *zap.Logger) (*Producer, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("kafkago: brokers required")
	}
	acks := kafka.RequireAll
	switch cfg.Acks {
	case "one":
		acks = kafka.RequireOne
	case "none":
		acks = kafka.RequireNone
	}
	wt := cfg.WriteTimeout
	if wt <= 0 {
		wt = 5 * time.Second
	}
	bt := cfg.BatchTimeout
	if bt <= 0 {
		bt = 100 * time.Millisecond
	}
	bb := cfg.BatchBytes
	if bb <= 0 {
		bb = 1 << 20
	}
	bs := cfg.BatchSize
	if bs <= 0 {
		bs = 100
	}
	var comp kafka.Compression
	switch cfg.Compression {
	case "snappy":
		comp = kafka.Snappy
	case "gzip":
		comp = kafka.Gzip
	case "zstd":
		comp = kafka.Zstd
	case "", "lz4":
		comp = kafka.Lz4
	}
	w := &kafka.Writer{
		Addr:         kafka.TCP(cfg.Brokers...),
		Balancer:     &kafka.LeastBytes{},
		RequiredAcks: acks,
		WriteTimeout: wt,
		BatchTimeout: bt,
		BatchBytes:   bb,
		BatchSize:    bs,
		Async:        false, // 审计要同步反馈
		Compression:  comp,
	}
	return &Producer{w: w, logger: logger}, nil
}

// Send 实现 audit.KafkaProducer。topic 显式传入，让 caller 决定每行 topic
// （审计 / 业务 outbox 等可能不同）。
//
// 同步发：失败让 caller 走 DB-only fallback。建议 caller 给一个短 ctx
// (≤ WriteTimeout)，否则 kafka-go 会被 broker 拖住几十秒。
func (p *Producer) Send(topic string, key, value []byte) error {
	if p == nil || p.w == nil {
		return errors.New("kafkago: nil producer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.w.WriteTimeout)
	defer cancel()
	return p.w.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   key,
		Value: value,
	})
}

// Close 释放 kafka.Writer 内部连接 / batch flush。fx OnStop 调一次。
func (p *Producer) Close() error {
	if p == nil || p.w == nil {
		return nil
	}
	if err := p.w.Close(); err != nil {
		if p.logger != nil {
			p.logger.Warn("kafkago close", zap.Error(err))
		}
		return err
	}
	return nil
}
