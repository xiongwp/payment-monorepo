package service

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func TestRedisDecryptCache_EncryptDecrypt(t *testing.T) {
	ephemeralKey := make([]byte, 32)
	for i := 0; i < 32; i++ {
		ephemeralKey[i] = byte(i)
	}
	logger := zap.NewNop()

	// Create in-memory Redis for testing
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379", // Requires local Redis running
	})
	defer rdb.Close()

	rc, err := NewRedisDecryptCache(rdb, 5*time.Minute, ephemeralKey, logger)
	if err != nil {
		t.Fatalf("failed to create redis cache: %v", err)
	}

	plaintext := []byte("sensitive-data-12345")
	encrypted, err := rc.encryptPlaintext(plaintext)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}

	// Verify encrypted != plaintext
	if string(encrypted) == string(plaintext) {
		t.Fatal("encrypted data same as plaintext")
	}

	decrypted, err := rc.decryptPlaintext(encrypted)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}

	if string(decrypted) != string(plaintext) {
		t.Fatalf("decrypted mismatch: got %s, want %s", decrypted, plaintext)
	}
}

func TestRedisDecryptCache_CacheKeyFormat(t *testing.T) {
	ephemeralKey := make([]byte, 32)
	logger := zap.NewNop()

	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
	})
	defer rdb.Close()

	rc, err := NewRedisDecryptCache(rdb, 5*time.Minute, ephemeralKey, logger)
	if err != nil {
		t.Fatalf("failed to create redis cache: %v", err)
	}

	key := rc.cacheKey("test-ciphertext\x00test-context")
	// Should be kms:dec:<sha256_hex>
	if len(key) < len("kms:dec:") || key[:8] != "kms:dec:" {
		t.Fatalf("invalid cache key format: %s", key)
	}
	if len(key) != 8+64 { // "kms:dec:" + 64 hex chars
		t.Fatalf("cache key length incorrect: %d", len(key))
	}
}

func TestRedisDecryptCache_CorruptedDataHandling(t *testing.T) {
	ephemeralKey := make([]byte, 32)
	logger := zap.NewNop()

	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
	})
	defer rdb.Close()

	rc, err := NewRedisDecryptCache(rdb, 5*time.Minute, ephemeralKey, logger)
	if err != nil {
		t.Fatalf("failed to create redis cache: %v", err)
	}

	// Try to decrypt invalid data
	_, err = rc.decryptPlaintext([]byte("invalid"))
	if err == nil {
		t.Fatal("expected error on invalid ciphertext")
	}
}
