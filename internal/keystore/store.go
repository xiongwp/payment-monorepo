// Package keystore 从目录加载 master key，暴露 (key_id → raw bytes) 快照 +
// 一个 ActiveKeyID 指针，给 service 层取用。
//
// 目录约定：
//   keystore.dir/
//     ├── main.key          (64 hex chars = 32 bytes)
//     ├── rotated_2025.key  (64 hex chars = 32 bytes)
//     └── ACTIVE            (一行：main)
//
// - 每个 *.key 文件内容：纯 hex，strip whitespace 后必须是 32/48/64 chars
// - ACTIVE：一行文本，指向某个 *.key 的文件名（不含 .key 后缀）
//
// 运行时本包只读；生成 / rotate / 删 key 由 cmd/kmsctl 做。
package keystore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiongwp/kms-manage/internal/cryptoenv"
)

var (
	ErrNoActiveKey = errors.New("keystore: ACTIVE file missing or empty")
	ErrEmptyStore  = errors.New("keystore: no *.key files in directory")
)

// KeyMeta 给 admin / audit 侧用。
type KeyMeta struct {
	ID        string
	CreatedAt time.Time
	Algorithm string // 目前恒为 "AES-256-GCM"（或 AES-128/192-GCM 取决长度）
}

// Store 是全量快照；加载后只读。rotate 需要完全 reload 一次（prod 通常进程重启）。
type Store struct {
	keys    map[string][]byte
	metas   map[string]KeyMeta
	active  atomic.Pointer[string]
	baseDir string
}

// Load 从目录加载 keystore。
func Load(dir string) (*Store, error) {
	// 资金安全：keystore 目录权限校验。CLAUDE.md 明确要求 master key 文件 0600，
	// 但之前代码只 os.ReadFile 没 Stat → 误置成 0644 / 0666 启动时不报错，宿主机
	// 任何 shell 用户能读 master key → 全平台 kms:v1:* ciphertext 全裸。
	//
	// 校验规则：
	//   - 目录本身：不允许 group/other 写入（防 attacker 写新 key 让 ACTIVE 切换）
	//   - 每个 .key 文件：mode & 0077 == 0（即 group/other 无任何权限）
	// 启动期硬失败而非 warn，否则就跟没检查一样。
	if di, err := os.Stat(dir); err == nil {
		if mode := di.Mode().Perm(); mode&0o022 != 0 {
			return nil, fmt.Errorf("keystore: dir %s mode %#o is too open (no group/other write); chmod 0700 %s", dir, mode, dir)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("keystore: read %s: %w", dir, err)
	}
	s := &Store{
		keys:    make(map[string][]byte),
		metas:   make(map[string]KeyMeta),
		baseDir: dir,
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".key") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".key")
		path := filepath.Join(dir, e.Name())
		// 文件权限校验：mode & 0o077 != 0 → 任意 group/other 位 → 启动失败。
		// 0600 OK；0400 (read-only owner) 也 OK；0644 / 0660 全部拒。
		if fi, err := os.Stat(path); err == nil {
			if mode := fi.Mode().Perm(); mode&0o077 != 0 {
				return nil, fmt.Errorf("keystore: key file %s mode %#o is too open (must be 0600 or stricter); chmod 0600 %s", path, mode, path)
			}
		}
		raw, err := readKeyFile(path)
		if err != nil {
			return nil, fmt.Errorf("keystore: read %s: %w", id, err)
		}
		if err := cryptoenv.ValidateKey(raw); err != nil {
			return nil, fmt.Errorf("keystore: key %s: %w", id, err)
		}
		info, _ := e.Info()
		var ct time.Time
		if info != nil {
			ct = info.ModTime()
		}
		s.keys[id] = raw
		s.metas[id] = KeyMeta{
			ID:        id,
			CreatedAt: ct,
			Algorithm: algoFor(len(raw)),
		}
	}
	if len(s.keys) == 0 {
		return nil, ErrEmptyStore
	}
	active, err := readActive(dir)
	if err != nil {
		return nil, err
	}
	if _, ok := s.keys[active]; !ok {
		return nil, fmt.Errorf("keystore: ACTIVE points to missing key %q", active)
	}
	s.active.Store(&active)
	return s, nil
}

// ActiveKeyID 返回当前活跃 key 的 id。
func (s *Store) ActiveKeyID() string { return *s.active.Load() }

// KeyByID 根据 id 取 raw bytes；not-found 返回 (nil, false)。不要修改返回的 slice。
func (s *Store) KeyByID(id string) ([]byte, bool) {
	k, ok := s.keys[id]
	return k, ok
}

// Snapshot 返回所有 key 的浅拷贝（map 本身是新的，values 直接引用）。
// 给 cryptoenv.Decrypt 用。
func (s *Store) Snapshot() map[string][]byte {
	out := make(map[string][]byte, len(s.keys))
	for k, v := range s.keys {
		out[k] = v
	}
	return out
}

// List 返回所有 metadata（稳定顺序）。
func (s *Store) List() []KeyMeta {
	out := make([]KeyMeta, 0, len(s.metas))
	for _, m := range s.metas {
		out = append(out, m)
	}
	// 按 id 排序，测试更稳
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].ID > out[j].ID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Meta 按 id 查 metadata。
func (s *Store) Meta(id string) (KeyMeta, bool) {
	m, ok := s.metas[id]
	return m, ok
}

// ─── private helpers ───────────────────────────────────────

func readKeyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.Join(strings.Fields(string(data)), "")
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}
	return raw, nil
}

func readActive(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "ACTIVE"))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoActiveKey, err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", ErrNoActiveKey
	}
	return id, nil
}

func algoFor(n int) string {
	switch n {
	case 16:
		return "AES-128-GCM"
	case 24:
		return "AES-192-GCM"
	case 32:
		return "AES-256-GCM"
	default:
		return "unknown"
	}
}
