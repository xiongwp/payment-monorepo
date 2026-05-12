package scaffold

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseYAML_FlatFields(t *testing.T) {
	src := `
addr: ":9000"
log_level: "warn"
shutdown_timeout: "15s"
log_dev: true
`
	var c Config
	if err := parseYAML(src, &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9000" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
	if c.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v", c.ShutdownTimeout)
	}
	if !c.LogDev {
		t.Error("LogDev should be true")
	}
}

func TestParseYAML_Nested(t *testing.T) {
	src := `
db:
  dsn: "u:p@tcp(h)/d"
  max_open_conns: 50
  auto_migrate: true
oauth2:
  introspect_url: "http://oauth:8087/introspect"
  required: true
`
	var c Config
	if err := parseYAML(src, &c); err != nil {
		t.Fatal(err)
	}
	if c.DB.DSN != "u:p@tcp(h)/d" {
		t.Errorf("DB.DSN = %q", c.DB.DSN)
	}
	if c.DB.MaxOpenConns != 50 {
		t.Errorf("MaxOpenConns = %d", c.DB.MaxOpenConns)
	}
	if !c.DB.AutoMigrate {
		t.Error("AutoMigrate should be true")
	}
	if c.OAuth2.IntrospectURL != "http://oauth:8087/introspect" {
		t.Errorf("OAuth2.IntrospectURL = %q", c.OAuth2.IntrospectURL)
	}
}

func TestParseYAML_IgnoresComments(t *testing.T) {
	src := `
# top comment
addr: ":1"
# inline comment
log_level: "info"
`
	var c Config
	if err := parseYAML(src, &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":1" {
		t.Errorf("Addr = %q", c.Addr)
	}
}

func TestLoadYAMLFile_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.yaml")
	if err := os.WriteFile(path, []byte(`addr: ":7777"`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := loadYAMLFile(path, &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":7777" {
		t.Errorf("Addr = %q", c.Addr)
	}
}
