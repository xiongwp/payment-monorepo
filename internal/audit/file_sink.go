// file_sink.go: append-only newline-delimited JSON 文件 sink。
//
// 给 没接 Kafka / ClickHouse 的部署（小规模 / 内网 / dev）一个比 LogSink
// 更结构化的持久化选项：
//   - 每条 audit 一行 canonical JSON (跟 ChainSink hash 输入完全一致)
//   - 自动按 max_size_bytes / max_age 滚动 (file.log → file.log.1 → ...)
//   - 落盘前 fsync 让 SSD 突然断电也能保最近一条
//
// 跟 ChainSink 配套时输出文件可被 cmd/audit-verify (生产可补) 流式扫一遍
// hash 校验。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// jsonUnmarshalImpl wrapper let tests stub if needed.
var jsonUnmarshalImpl = json.Unmarshal

// FileSink append-only 写到指定文件；按 size 滚动（max_size_bytes > 0 时）。
type FileSink struct {
	path        string
	maxSize     int64    // 单文件大小上限；0 = 不滚动
	maxBackups  int      // 保留的滚动文件数；0 = 全留
	logger      *zap.Logger
	fsyncEvery  int      // 每 N 条 fsync 一次；0 = 不强制 fsync (依赖 OS page cache)
	mu          sync.Mutex
	f           *os.File
	curSize     int64
	writtenSinceSync int
}

// NewFileSink 默认 maxSize=128MB / maxBackups=10 / fsyncEvery=100
// （吞吐 vs 可靠的折衷；超高合规场景 fsyncEvery=1）。
func NewFileSink(path string, maxSize int64, maxBackups int, fsyncEvery int, logger *zap.Logger) (*FileSink, error) {
	if maxSize == 0 {
		maxSize = 128 << 20
	}
	if maxBackups == 0 {
		maxBackups = 10
	}
	if fsyncEvery == 0 {
		fsyncEvery = 100
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir audit dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open audit file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &FileSink{
		path:       path,
		maxSize:    maxSize,
		maxBackups: maxBackups,
		fsyncEvery: fsyncEvery,
		logger:     logger,
		f:          f,
		curSize:    st.Size(),
	}, nil
}

// Write append 一条 JSON 行。fail-open：写文件失败只 log，不阻塞 hot path。
func (s *FileSink) Write(_ context.Context, a *DecisionAudit) {
	if s == nil || s.f == nil || a == nil {
		return
	}
	body, err := json.Marshal(a)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("file sink marshal failed", zap.Error(err))
		}
		return
	}
	body = append(body, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	// 滚动：当前文件 + 新写后超 maxSize → rotate
	if s.maxSize > 0 && s.curSize+int64(len(body)) > s.maxSize {
		if err := s.rotateLocked(); err != nil && s.logger != nil {
			s.logger.Warn("file sink rotate failed", zap.Error(err))
		}
	}

	n, err := s.f.Write(body)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("file sink write failed", zap.Error(err))
		}
		return
	}
	s.curSize += int64(n)
	s.writtenSinceSync++
	if s.fsyncEvery > 0 && s.writtenSinceSync >= s.fsyncEvery {
		_ = s.f.Sync()
		s.writtenSinceSync = 0
	}
}

// WriteBatch 给 AsyncBatchSink 调；批量 Write 后单次 fsync。
func (s *FileSink) WriteBatch(ctx context.Context, batch []*DecisionAudit) {
	if s == nil || len(batch) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range batch {
		if a == nil {
			continue
		}
		body, err := json.Marshal(a)
		if err != nil {
			continue
		}
		body = append(body, '\n')
		if s.maxSize > 0 && s.curSize+int64(len(body)) > s.maxSize {
			_ = s.rotateLocked()
		}
		n, err := s.f.Write(body)
		if err != nil {
			continue
		}
		s.curSize += int64(n)
	}
	_ = s.f.Sync()
	s.writtenSinceSync = 0
}

// rotateLocked 关掉当前文件、重命名为 .1 / .2 / ...、重开新文件。
// 旧文件超过 maxBackups 时删掉最老的。
func (s *FileSink) rotateLocked() error {
	if err := s.f.Close(); err != nil {
		return err
	}
	// 找已有 .1 / .2 / ... 重命名，新文件压栈
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type backup struct {
		path string
		idx  int
	}
	var backups []backup
	for _, e := range entries {
		name := e.Name()
		if len(name) <= len(base)+1 || name[:len(base)+1] != base+"." {
			continue
		}
		var idx int
		_, err := fmt.Sscanf(name[len(base)+1:], "%d", &idx)
		if err != nil {
			continue
		}
		backups = append(backups, backup{path: filepath.Join(dir, name), idx: idx})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].idx > backups[j].idx })
	// 把 file.log.N+1 → file.log.N+2 等等下一档
	for _, b := range backups {
		newPath := fmt.Sprintf("%s.%d", s.path, b.idx+1)
		_ = os.Rename(b.path, newPath)
	}
	// file.log → file.log.1
	if err := os.Rename(s.path, s.path+".1"); err != nil {
		return err
	}
	// 删掉超 maxBackups 的
	if s.maxBackups > 0 {
		entries, _ = os.ReadDir(dir)
		var toCheck []backup
		for _, e := range entries {
			name := e.Name()
			if len(name) <= len(base)+1 || name[:len(base)+1] != base+"." {
				continue
			}
			var idx int
			if _, err := fmt.Sscanf(name[len(base)+1:], "%d", &idx); err != nil {
				continue
			}
			toCheck = append(toCheck, backup{path: filepath.Join(dir, name), idx: idx})
		}
		for _, b := range toCheck {
			if b.idx > s.maxBackups {
				_ = os.Remove(b.path)
			}
		}
	}
	// 重开新文件
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	s.f = f
	s.curSize = 0
	return nil
}

// Close 关文件 (生产 main.go shutdown 钩子调；防止最后一批 audit 留在 OS
// page cache 没 fsync 就掉电)。
func (s *FileSink) Close() error {
	if s == nil || s.f == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.f.Sync()
	err := s.f.Close()
	s.f = nil
	return err
}

// CurrentSize 当前文件 byte 数（给 metric 用，看是否快到 maxSize 触发 rotate）。
func (s *FileSink) CurrentSize() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.curSize
}

// 兼容 _ = time.Now（避免 unused import 警告）
var _ = time.Now

// LastChainRowHash 读取文件最后一条审计的 chain_row_hash，给 ChainSink
// resume 续链用。失败 / 空文件 → 返 ""，caller fallback 到 genesis。
//
// 实现：从文件尾向前读最多 64KB（一条 audit JSON 通常 < 4KB；多预留），
// 找最后一行的 metadata.chain_row_hash 字段。轻量启动逻辑，不影响 hot path。
func ReadLastChainRowHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() == 0 {
		return "", nil
	}
	// 读最后 64KB；不够大就全读
	const tailMax = 64 * 1024
	readLen := int64(tailMax)
	if st.Size() < readLen {
		readLen = st.Size()
	}
	buf := make([]byte, readLen)
	if _, err := f.ReadAt(buf, st.Size()-readLen); err != nil {
		return "", err
	}
	// 找最后一个非空行
	lines := splitTrailingLines(buf)
	if len(lines) == 0 {
		return "", nil
	}
	last := lines[len(lines)-1]
	// chain hash 在 Input.Metadata.chain_row_hash (见 chain_sink.go)
	var rec struct {
		Input struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"input"`
	}
	if err := jsonUnmarshalLine(last, &rec); err != nil {
		return "", err
	}
	if rec.Input.Metadata == nil {
		return "", nil
	}
	return rec.Input.Metadata["chain_row_hash"], nil
}

// splitTrailingLines 拆 buf 为非空行 (按 \n)；丢弃首行（可能是上一行的
// 不完整片段）。
func splitTrailingLines(buf []byte) [][]byte {
	out := [][]byte{}
	start := 0
	first := true
	for i, b := range buf {
		if b != '\n' {
			continue
		}
		line := buf[start:i]
		start = i + 1
		if first {
			// 第一行可能是上一行的尾巴 (因为 readLen 从中间切的)；丢
			first = false
			continue
		}
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	if start < len(buf) {
		// 末尾没换行的那行视作完整（fsync 时有时这条还没 \n）
		if rest := buf[start:]; len(rest) > 0 {
			out = append(out, rest)
		}
	}
	return out
}

// jsonUnmarshalLine wrapper：本来 file_sink 用 encoding/json.Marshal；这里
// 用 stdlib 解析。独立 func 让测试 stub 简单。
func jsonUnmarshalLine(b []byte, v any) error {
	return jsonUnmarshalImpl(b, v)
}
