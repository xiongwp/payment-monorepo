package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestFileSink_AppendsJSONLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	s, err := NewFileSink(path, 0, 0, 1, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Write(context.Background(), mkAudit("d1"))
	s.Write(context.Background(), mkAudit("d2"))
	s.Close()

	f, _ := os.Open(path)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines; got %d", len(lines))
	}
	for _, l := range lines {
		var a DecisionAudit
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatalf("invalid JSON line: %s", l)
		}
	}
}

func TestFileSink_RotatesOnSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	// maxSize 200B → 一两条就 rotate
	s, err := NewFileSink(path, 200, 5, 1, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 10; i++ {
		s.Write(context.Background(), mkAudit("decision-"+string(rune('a'+i))))
	}
	s.Close()
	entries, _ := os.ReadDir(dir)
	files := []string{}
	for _, e := range entries {
		files = append(files, e.Name())
	}
	// 至少应该有 audit.log + audit.log.1
	hasOriginal := false
	hasRotated := false
	for _, f := range files {
		if f == "audit.log" {
			hasOriginal = true
		}
		if strings.HasPrefix(f, "audit.log.") {
			hasRotated = true
		}
	}
	if !hasOriginal || !hasRotated {
		t.Fatalf("expected audit.log + .1; got %+v", files)
	}
}

func TestFileSink_MaxBackupsLimitsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	// maxSize 100B + maxBackups 2 → 老 backup 应被删
	s, err := NewFileSink(path, 100, 2, 1, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 30; i++ {
		s.Write(context.Background(), mkAudit(fmt.Sprintf("d%d", i)))
	}
	s.Close()
	entries, _ := os.ReadDir(dir)
	cnt := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "a.log") {
			cnt++
		}
	}
	// 最多 1 当前文件 + 2 backup = 3
	if cnt > 3 {
		t.Fatalf("expected ≤ 3 files (current + 2 backups); got %d", cnt)
	}
}

func TestFileSink_WriteBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.log")
	s, _ := NewFileSink(path, 0, 0, 0, zap.NewNop())
	defer s.Close()
	batch := []*DecisionAudit{mkAudit("x"), mkAudit("y"), mkAudit("z")}
	s.WriteBatch(context.Background(), batch)
	s.Close()
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), "\n") != 3 {
		t.Fatalf("expected 3 lines; got %d", strings.Count(string(data), "\n"))
	}
}

func TestFileSink_NilSafe(t *testing.T) {
	var s *FileSink
	s.Write(context.Background(), mkAudit("x"))
	s.WriteBatch(context.Background(), []*DecisionAudit{mkAudit("y")})
	if got := s.CurrentSize(); got != 0 {
		t.Fatalf("nil should be 0; got %d", got)
	}
	_ = s.Close()
}

func TestReadLastChainRowHash_ResumesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.log")
	// 写两条 audit；第二条带 metadata.chain_row_hash
	s, _ := NewFileSink(path, 0, 0, 1, zap.NewNop())
	a1 := mkAudit("d1")
	a1.Input.Metadata = map[string]string{"chain_row_hash": "AAAA"}
	s.Write(context.Background(), a1)
	a2 := mkAudit("d2")
	a2.Input.Metadata = map[string]string{"chain_row_hash": "BBBB"}
	s.Write(context.Background(), a2)
	s.Close()

	got, err := ReadLastChainRowHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "BBBB" {
		t.Fatalf("expected BBBB; got %q", got)
	}
}

func TestReadLastChainRowHash_EmptyFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	if _, err := os.Create(path); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLastChainRowHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected empty; got %q", got)
	}
}

func TestReadLastChainRowHash_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadLastChainRowHash(filepath.Join(dir, "no.log")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
