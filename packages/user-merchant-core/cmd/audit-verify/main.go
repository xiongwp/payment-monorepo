// Command audit-verify 遍历 admin_audit_log，按写入顺序重算 row_hash 链，
// 找到第一个"与存储不一致"的行。对于 append-only 审计表，这意味着有人直接
// 改了表内容。
//
// 典型用法（k8s CronJob / 手工复核）：
//   audit-verify --dsn "$USERMERCHANTCORE_DATABASE_META_DSN"
// 退出码：
//   0  链完整
//   1  CLI 用法错
//   2  发现断裂（mismatch_row_id 会打印）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/pkg/dbx"
	"github.com/xiongwp/user-merchant-core/pkg/grpcutil"
)

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN of the meta DB holding admin_audit_log")
	batch := flag.Int("batch", 1000, "rows per SELECT batch")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "usage: audit-verify --dsn <mysql-dsn>")
		os.Exit(1)
	}

	mgr, err := dbx.NewManager(dbx.DBConfig{Name: "meta", DSN: *dsn})
	if err != nil {
		die(err)
	}
	defer mgr.Close()
	db := mgr.GetMeta()

	var (
		lastID   int64
		prevHash string
		checked  int
	)
	ctx := context.Background()
	for {
		var rows []domain.AdminAuditLog
		if err := db.WithContext(ctx).
			Where("id > ?", lastID).
			Order("id ASC").
			Limit(*batch).
			Find(&rows).Error; err != nil {
			die(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			expected := computeRowHash(&r, prevHash)
			if expected != r.RowHash {
				fmt.Fprintf(os.Stderr,
					"CHAIN BREAK at id=%d: stored row_hash=%s but computed=%s\n",
					r.ID, r.RowHash, expected)
				os.Exit(2)
			}
			if r.PrevHash != prevHash {
				fmt.Fprintf(os.Stderr,
					"PREV_HASH MISMATCH at id=%d: stored prev_hash=%s but chain says %s\n",
					r.ID, r.PrevHash, prevHash)
				os.Exit(2)
			}
			prevHash = r.RowHash
			lastID = r.ID
			checked++
		}
	}
	fmt.Printf("OK: %d rows verified, last_row_hash=%s\n", checked, prevHash)
}

// computeRowHash 必须与 pkg/grpcutil.computeRowHash 字节级一致。
// （pkg 层不暴露该函数，这里重实现，任何公式变动要两边同步。）
func computeRowHash(r *domain.AdminAuditLog, prevHash string) string {
	e := &grpcutil.AuditEntry{
		Actor:       r.Actor,
		ActorIP:     r.ActorIP,
		Method:      r.Method,
		TargetID:    r.TargetID,
		RequestBody: r.RequestBody,
		StatusCode:  r.StatusCode,
		ResponseErr: r.ResponseErr,
		TraceID:     r.TraceID,
		PrevHash:    prevHash,
	}
	return grpcutil.ComputeRowHash(e)
}

func die(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(2) }
