// Package routing — retry_queue.go：重试队列的内存实现（开发/测试用）
//
// 生产环境应使用数据库 outbox pattern 实现，保证不丢失。
// 这里提供的内存实现用于快速集成测试和演示。
package routing

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryRetryQueue 基于内存 map 的重试队列实现
type MemoryRetryQueue struct {
	mu    sync.RWMutex
	tasks map[string]*RetryTask
}

// NewMemoryRetryQueue 创建内存队列
func NewMemoryRetryQueue() *MemoryRetryQueue {
	return &MemoryRetryQueue{
		tasks: make(map[string]*RetryTask),
	}
}

// Enqueue 入队
func (q *MemoryRetryQueue) Enqueue(ctx context.Context, task *RetryTask) error {
	if task == nil || task.ID == "" {
		return fmt.Errorf("invalid task")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tasks[task.ID] = task
	return nil
}

// Dequeue 取出即将重试的任务（next_retry_at <= now）
func (q *MemoryRetryQueue) Dequeue(ctx context.Context, limit int) ([]*RetryTask, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	now := time.Now()
	var result []*RetryTask
	count := 0

	for _, task := range q.tasks {
		if count >= limit {
			break
		}
		if task.NextRetryAt.Before(now) || task.NextRetryAt.Equal(now) {
			result = append(result, task)
			count++
		}
	}
	return result, nil
}

// MarkRetry 标记一次重试
func (q *MemoryRetryQueue) MarkRetry(ctx context.Context, taskID string, attempt int, nextRetryAt time.Time, errorMsg string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	task, ok := q.tasks[taskID]
	if !ok {
		return fmt.Errorf("task not found: %s", taskID)
	}
	task.Attempt = attempt
	task.NextRetryAt = nextRetryAt
	task.LastErrorMsg = errorMsg
	task.UpdatedAt = time.Now()
	return nil
}

// MarkSuccess 标记成功，删除任务
func (q *MemoryRetryQueue) MarkSuccess(ctx context.Context, taskID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, ok := q.tasks[taskID]; !ok {
		return fmt.Errorf("task not found: %s", taskID)
	}
	delete(q.tasks, taskID)
	return nil
}

// Stats 返回队列统计信息（用于监控）
func (q *MemoryRetryQueue) Stats() map[string]interface{} {
	q.mu.RLock()
	defer q.mu.RUnlock()

	now := time.Now()
	var pending int
	var overdue int

	for _, task := range q.tasks {
		if task.NextRetryAt.Before(now) {
			overdue++
		} else {
			pending++
		}
	}

	return map[string]interface{}{
		"total":   len(q.tasks),
		"pending": pending,
		"overdue": overdue,
	}
}

// DBRetryQueue 真实实现见 db_retry_queue.go (DBRetryQueueImpl).
//
// 这里保留构造函数别名,方便老调用方迁移 (PaymentService 注入时:
//
//	rq := routing.NewDBRetryQueue(db, logger)
//	svc.SetRetryQueue(rq)
//
// 内存版仅供单测/dev: NewMemoryRetryQueue()).
