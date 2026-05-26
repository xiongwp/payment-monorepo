//go:build !s3

// s3_store_stub.go — 默认 build 走 stub，不引 AWS SDK 依赖。
// 真实现见 s3_store.go (//go:build s3)。
package auditreplay

import (
	"context"
	"errors"
	"time"
)

// ErrS3NotEnabled stub 返这个；真实现在 //go:build s3 时编进去。
var ErrS3NotEnabled = errors.New("auditreplay: s3 store not enabled (build with -tags s3)")

// S3Config S3/OSS 配置。
type S3Config struct {
	Endpoint  string // s3.amazonaws.com / oss-cn-hangzhou.aliyuncs.com
	Region    string
	Bucket    string
	Prefix    string // 默认 "risk"
	AccessKey string
	SecretKey string
}

// NewS3Store stub 实现直接报错；//go:build s3 编进真实现替换。
func NewS3Store(cfg S3Config) (Store, error) {
	return nil, ErrS3NotEnabled
}

// noopStore 占位让 cmd/server/main.go 在 stub build 下能编。
type noopStore struct{}

func (noopStore) Put(ctx context.Context, snap SignalSnapshot) error { return nil }
func (noopStore) Get(ctx context.Context, decisionID string) (SignalSnapshot, error) {
	return SignalSnapshot{}, ErrNotFound
}
func (noopStore) List(ctx context.Context, date time.Time, limit int) ([]SignalSnapshot, error) {
	return nil, nil
}

// NewNoopStore 主流程 fail-safe 占位（auditreplay 关闭时用）。
func NewNoopStore() Store { return noopStore{} }
