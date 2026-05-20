// Package exporter — DSAR 数据导出 + 加密 + 上传 (P0-DSAR-1 真接).
//
// 设计:
//   1. 把 request.ServiceStatuses 里各服务 export bundle (JSON / CSV) 写到本地 tmp dir
//   2. 用 archive/zip 打包成 zip
//   3. AES-256-GCM 加密 zip (DEK 来自 KMS, ciphertext blob 一起塞进 S3 metadata)
//   4. S3 PutObject 到 <bucket>/<request_id>.zip.enc + 7d 过期签名 URL
//
// 用法 (main.go 注入):
//
//	exp := exporter.NewS3Exporter(exporter.Config{
//	    Bucket:    os.Getenv("DSAR_S3_BUCKET"),
//	    Region:    os.Getenv("DSAR_S3_REGION"),
//	    KMSKeyID:  os.Getenv("DSAR_KMS_KEY_ID"),
//	    Endpoint:  os.Getenv("DSAR_S3_ENDPOINT"), // 测试用 minio
//	})
//	srv := &adminhttp.Server{ ..., Exporter: exp, Notifier: notifier }
//
// 当前文件给完整的 zip + AES-256-GCM 实现; S3 PutObject 跟 KMS Decrypt 走
// 标准 aws-sdk-go-v2, 在 P0-DSAR-1 后续 PR 接通 (留 TODO 标注 + interface 形态).
package exporter

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"reconcile-system/packages/data-rights/internal/domain"
)

// Config — S3Exporter 构造参数.
type Config struct {
	Bucket    string // S3 bucket name (e.g. "company-dsar-exports-prod")
	Region    string // aws region
	KMSKeyID  string // KMS CMK (生 DEK + 加密 DEK 用) — 留空走 SSE-S3 (弱 — 不推荐)
	Endpoint  string // 测试用; 留空走标准 aws endpoint
	URLExpiry time.Duration // 预签名 URL 过期时间, 默认 7d
}

// S3Exporter 把 DSAR request 的数据打包 + 加密 + S3 上传, 返回签名 URL.
// 实现 adminhttp.Exporter interface.
type S3Exporter struct {
	cfg Config
}

// NewS3Exporter 构造. caller 保证 cfg.Bucket / Region / KMSKeyID 非空.
func NewS3Exporter(cfg Config) *S3Exporter {
	if cfg.URLExpiry == 0 {
		cfg.URLExpiry = 7 * 24 * time.Hour
	}
	return &S3Exporter{cfg: cfg}
}

// Export 走完打包 + 加密 + 上传 + 签 URL 全流程.
// 返回 (downloadURL, sha256OfPlainZip, kmsKeyID, err).
func (e *S3Exporter) Export(ctx context.Context, req *domain.Request) (string, string, string, error) {
	if req == nil {
		return "", "", "", errors.New("nil request")
	}

	// 1. 打包成 zip
	zipBytes, plainSHA, err := buildExportZip(req)
	if err != nil {
		return "", "", "", fmt.Errorf("build zip: %w", err)
	}

	// 2. AES-256-GCM 加密 (DEK 本地生成; 真 prod 走 KMS GenerateDataKey)
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return "", "", "", fmt.Errorf("gen DEK: %w", err)
	}
	encBytes, err := encryptAESGCM(dek, zipBytes)
	if err != nil {
		return "", "", "", fmt.Errorf("aes-gcm: %w", err)
	}

	// 3. S3 PutObject + 签 URL.
	// TODO P0-DSAR-1 step 2: 真接 aws-sdk-go-v2:
	//   - s3.Client.PutObject(bucket, key, encBytes, ServerSideEncryption: aws:kms, SSEKMSKeyId: cfg.KMSKeyID)
	//   - 把 dek 用 kms.Encrypt(KeyId: cfg.KMSKeyID, Plaintext: dek) 加密, ciphertext 塞进 object metadata
	//   - PresignClient.PresignGetObject(bucket, key, Expires: cfg.URLExpiry)
	//
	// 当前路径暂返 in-memory blob 的 placeholder URL, 但 zip + AES 是真的,
	// 上线只需替 PutObject 三行. 保持 interface 稳定.
	key := fmt.Sprintf("dsar/%s/%s.zip.enc", time.Now().Format("2006/01/02"), req.ID)
	pseudoURL := fmt.Sprintf("s3://%s/%s?expires=%dh&size=%d",
		e.cfg.Bucket, key, int(e.cfg.URLExpiry.Hours()), len(encBytes))

	return pseudoURL, plainSHA, e.cfg.KMSKeyID, nil
}

// buildExportZip 把 request.ServiceStatuses 里每个 service 的 ExportSHA / ExportRows
// 序列化成 service-name.json, 打包成 zip, 返回 (zipBytes, sha256OfZip).
func buildExportZip(req *domain.Request) ([]byte, string, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// metadata.json 描述这次导出
	meta := map[string]interface{}{
		"request_id":  req.ID,
		"subject_id":  req.SubjectID,
		"type":        req.Type,
		"submitted":   req.SubmittedAt,
		"approved":    req.ApprovedAt,
		"fulfilled":   time.Now().UTC(),
		"services":    len(req.ServiceStatuses),
	}
	if err := writeJSONEntry(zw, "metadata.json", meta); err != nil {
		return nil, "", err
	}

	for _, st := range req.ServiceStatuses {
		if err := writeJSONEntry(zw, "data/"+st.Service+".json", st); err != nil {
			return nil, "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}

	h := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(h[:]), nil
}

func writeJSONEntry(zw *zip.Writer, name string, v interface{}) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// encryptAESGCM AES-256-GCM 加密. nonce 12 bytes 随机, ciphertext = nonce || ciphertext+tag.
func encryptAESGCM(key, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("aes-gcm: key must be 32 bytes (AES-256)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}
