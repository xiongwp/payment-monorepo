package mockserver

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// Logger is a tiny logging interface so the mock can run with either a real
// *zap.Logger (via an adapter) or a quiet stdlib logger in tests.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

// StdLogger is a zero-dep Logger backed by the stdlib log package.
type StdLogger struct{ l *log.Logger }

func NewStdLogger() *StdLogger { return &StdLogger{l: log.New(os.Stdout, "mockserver ", log.LstdFlags)} }

func (s *StdLogger) Info(msg string, kv ...any) { s.l.Printf("INFO %s %s", msg, formatKV(kv)) }
func (s *StdLogger) Warn(msg string, kv ...any) { s.l.Printf("WARN %s %s", msg, formatKV(kv)) }

func formatKV(kv []any) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "%v=%v ", kv[i], kv[i+1])
	}
	return strings.TrimSpace(b.String())
}

// DiscardLogger drops every log call; handy in tests.
type DiscardLogger struct{}

func (DiscardLogger) Info(string, ...any) {}
func (DiscardLogger) Warn(string, ...any) {}
