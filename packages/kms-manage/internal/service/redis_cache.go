// Package service implements Redis cache layer for KMS decrypt operations.
package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/xiongwp/kms-manage/internal/metrics"
)

// RedisDecryptCache wraps Redis client with encrypted plaintext storage.
// Plaintext is encrypted at rest using ephemeral local key before writing to Redis.
// Cache key: kms:dec:<sha256(ciphertext)>
// Cache value: encrypted plaintext (GCM) with embedded nonce
type RedisDecryptCache struct {
	client    *redis.Client
	logger    *zap.Logger
	ttl       time.Duration
	ephemeralKey []byte // 32 bytes for AES-256
}

// NewRedisDecryptCache creates a new Redis-backed decrypt cache.
// ephemeralKey must be 32 bytes (AES-256).
func NewRedisDecryptCache(client *redis.Client, ttl time.Duration, ephemeralKey []byte, logger *zap.Logger) (*RedisDecryptCache, error) {
	if len(ephemeralKey) != 32 {
		return nil, fmt.Errorf("ephemeral_key must be 32 bytes, got %d", len(ephemeralKey))
	}
	return &RedisDecryptCache{
		client:       client,
		logger:       logger,
		ttl:          ttl,
		ephemeralKey: ephemeralKey,
	}, nil
}

// encryptPlaintext encrypts plaintext using GCM with embedded nonce.
// Format: [nonce (12 bytes)][ciphertext][tag (16 bytes)]
func (r *RedisDecryptCache) encryptPlaintext(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(r.ephemeralKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := aead.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// decryptPlaintext decrypts plaintext using GCM with embedded nonce.
func (r *RedisDecryptCache) decryptPlaintext(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(r.ephemeralKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

// cacheKey generates Redis key: kms:dec:<sha256(kmstext)>
// kmstext = ciphertext + "\x00" + context
func (r *RedisDecryptCache) cacheKey(kmstext string) string {
	hash := sha256.Sum256([]byte(kmstext))
	return "kms:dec:" + hex.EncodeToString(hash[:])
}

// Get retrieves plaintext from Redis cache if present and valid.
// Returns (plaintext, keyID, true) on hit; (nil, "", false) on miss/error.
func (r *RedisDecryptCache) Get(ctx context.Context, ciphertext, context string) ([]byte, string, bool) {
	key := r.cacheKey(ciphertext + "\x00" + context)

	val, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			metrics.KMSCacheTotal.WithLabelValues("redis", "miss").Inc()
			return nil, "", false
		}
		// Error (Redis unavailable): fail open
		r.logger.Debug("redis cache get error (failing open)", zap.String("key", key), zap.Error(err))
		metrics.KMSCacheTotal.WithLabelValues("redis", "error").Inc()
		return nil, "", false
	}

	// Parse: [keyID_len (4 bytes)][keyID][plaintext_encrypted]
	if len(val) < 4 {
		metrics.KMSCacheTotal.WithLabelValues("redis", "corrupted").Inc()
		return nil, "", false
	}

	keyIDLen := int(val[0])<<24 | int(val[1])<<16 | int(val[2])<<8 | int(val[3])
	if keyIDLen < 0 || keyIDLen > 256 || len(val) < 4+keyIDLen {
		metrics.KMSCacheTotal.WithLabelValues("redis", "corrupted").Inc()
		return nil, "", false
	}

	keyID := string(val[4 : 4+keyIDLen])
	plaintextEnc := val[4+keyIDLen:]

	plaintext, err := r.decryptPlaintext(plaintextEnc)
	if err != nil {
		r.logger.Debug("redis cache decrypt error", zap.Error(err))
		metrics.KMSCacheTotal.WithLabelValues("redis", "decrypt_fail").Inc()
		return nil, "", false
	}

	metrics.KMSCacheTotal.WithLabelValues("redis", "hit").Inc()
	return plaintext, keyID, true
}

// Set stores plaintext in Redis with TTL.
// Plaintext is encrypted at rest before writing.
func (r *RedisDecryptCache) Set(ctx context.Context, ciphertext, context, keyID string, plaintext []byte) {
	key := r.cacheKey(ciphertext + "\x00" + context)

	plaintextEnc, err := r.encryptPlaintext(plaintext)
	if err != nil {
		r.logger.Debug("redis cache encrypt error", zap.Error(err))
		return
	}

	// Format: [keyID_len (4 bytes)][keyID][plaintext_encrypted]
	keyIDBytes := []byte(keyID)
	if len(keyIDBytes) > 256 {
		r.logger.Debug("keyID too long", zap.Int("len", len(keyIDBytes)))
		return
	}

	value := make([]byte, 4+len(keyIDBytes)+len(plaintextEnc))
	value[0] = byte((len(keyIDBytes) >> 24) & 0xff)
	value[1] = byte((len(keyIDBytes) >> 16) & 0xff)
	value[2] = byte((len(keyIDBytes) >> 8) & 0xff)
	value[3] = byte(len(keyIDBytes) & 0xff)
	copy(value[4:], keyIDBytes)
	copy(value[4+len(keyIDBytes):], plaintextEnc)

	if err := r.client.Set(ctx, key, value, r.ttl).Err(); err != nil {
		r.logger.Debug("redis cache set error", zap.Error(err))
	}
}

// Clear removes all cache entries. Called on active key change.
func (r *RedisDecryptCache) Clear(ctx context.Context) {
	// Pattern-based deletion: match kms:dec:* keys
	// Use SCAN to avoid blocking
	iter := r.client.Scan(ctx, 0, "kms:dec:*", 1000).Iterator()
	for iter.Next(ctx) {
		_ = r.client.Del(ctx, iter.Val()).Err()
	}
}
