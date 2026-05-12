package scaffold

import (
	"os"
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", c.Addr)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
	if c.DB.MaxOpenConns != 30 {
		t.Errorf("MaxOpenConns = %d", c.DB.MaxOpenConns)
	}
}

func TestLoadEnv_ServicePrefix(t *testing.T) {
	os.Setenv("MY_SERVICE_ADDR", ":9999")
	os.Setenv("MY_SERVICE_DB_DSN", "user:pwd@tcp(host)/db")
	os.Setenv("AUDITLOG_URL", "http://audit:8087")
	defer os.Unsetenv("MY_SERVICE_ADDR")
	defer os.Unsetenv("MY_SERVICE_DB_DSN")
	defer os.Unsetenv("AUDITLOG_URL")

	c := &Config{}
	c.loadEnv("my-service")

	if c.Addr != ":9999" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.DB.DSN != "user:pwd@tcp(host)/db" {
		t.Errorf("DB.DSN = %q", c.DB.DSN)
	}
	if c.Audit.BaseURL != "http://audit:8087" {
		t.Errorf("Audit.BaseURL = %q", c.Audit.BaseURL)
	}
}

func TestLoadEnv_FallsBackToGeneric(t *testing.T) {
	// 没设 prefix 时退到通用名 (兼容老服务)
	os.Setenv("LOG_LEVEL", "debug")
	defer os.Unsetenv("LOG_LEVEL")

	c := &Config{}
	c.loadEnv("my-service")
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
}
