// local_store.go — 文件系统 Store 实现（dev / 单机部署用）。
//
// S3 / OSS 实现见 s3_store.go（//go:build s3 tag）。
package auditreplay

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LocalStore 把 SignalSnapshot 落到本地文件系统。
//
// 结构：{baseDir}/risk/{yyyy}/{mm}/{dd}/{decision_id}.json.gz
// 单文件 gzip 后 ~3-8KB（取决于信号数）；月写入 1M 决策 = ~5GB / 月。
type LocalStore struct {
	mu      sync.Mutex
	baseDir string
}

func NewLocalStore(baseDir string) (*LocalStore, error) {
	if baseDir == "" {
		return nil, errors.New("auditreplay: baseDir required")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", baseDir, err)
	}
	return &LocalStore{baseDir: baseDir}, nil
}

func (s *LocalStore) Put(ctx context.Context, snap SignalSnapshot) error {
	redacted := snap.Redact()
	body, err := json.Marshal(redacted)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	path := filepath.Join(s.baseDir, objectKey("risk", snap.OccurredAt, snap.DecisionID)+".gz")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	if _, err := gz.Write(body); err != nil {
		_ = gz.Close()
		return fmt.Errorf("gzip: %w", err)
	}
	return gz.Close()
}

func (s *LocalStore) Get(ctx context.Context, decisionID string) (SignalSnapshot, error) {
	// 不知道日期 → 反向扫最近 30 天
	now := time.Now()
	for d := 0; d < 30; d++ {
		t := now.AddDate(0, 0, -d)
		path := filepath.Join(s.baseDir, objectKey("risk", t, decisionID)+".gz")
		if snap, ok := s.tryRead(path); ok {
			return snap, nil
		}
	}
	return SignalSnapshot{}, ErrNotFound
}

func (s *LocalStore) tryRead(path string) (SignalSnapshot, bool) {
	f, err := os.Open(path)
	if err != nil {
		return SignalSnapshot{}, false
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return SignalSnapshot{}, false
	}
	defer gz.Close()
	body, err := io.ReadAll(gz)
	if err != nil {
		return SignalSnapshot{}, false
	}
	var snap SignalSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return SignalSnapshot{}, false
	}
	return snap, true
}

func (s *LocalStore) List(ctx context.Context, date time.Time, limit int) ([]SignalSnapshot, error) {
	dir := filepath.Join(s.baseDir, fmt.Sprintf("risk/%04d/%02d/%02d", date.Year(), int(date.Month()), date.Day()))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir: %w", err)
	}
	out := make([]SignalSnapshot, 0, min(limit, len(entries)))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if snap, ok := s.tryRead(filepath.Join(dir, e.Name())); ok {
			out = append(out, snap)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// PurgeOlderThan 清理超过 retain 时间的文件（30d retention）。
// 调度：admin endpoint 触发或 cron job。
func (s *LocalStore) PurgeOlderThan(ctx context.Context, retain time.Duration) (int, error) {
	cutoff := time.Now().Add(-retain)
	deleted := 0
	root := filepath.Join(s.baseDir, "risk")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if rmErr := os.Remove(path); rmErr == nil {
				deleted++
			}
		}
		return nil
	})
	return deleted, err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
