package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// merchant 域分片表 base name（与 templates/schema.sql 对齐）
const (
	tblMerchant            = "merchants"
	tblMerchantKYCDocument = "merchant_kyc_document"
	// merchant_kyc_audit 留 meta（append-only 合规）
	tblMerchantKYCAudit = "merchant_kyc_audit"
	// 反查索引（meta，跨 user / merchant 全局）
	tblMerchantLookup = "merchant_lookup"
)

// MerchantRepository merchants / merchant_kyc_document 按 merchant_id 分片到
// user_merchant_db_0..9；merchant_kyc_audit / merchant_lookup 留 meta。
type MerchantRepository interface {
	Create(ctx context.Context, m *domain.Merchant) error
	Get(ctx context.Context, id string) (*domain.Merchant, error)
	BatchGet(ctx context.Context, ids []string) ([]*domain.Merchant, error)
	GetByEmail(ctx context.Context, email string) (*domain.Merchant, error)
	GetByKeyHash(ctx context.Context, hash string) (*domain.Merchant, error)
	List(ctx context.Context, status domain.MerchantStatus, kyc domain.KYCStatus, limit, offset int) ([]*domain.Merchant, int64, error)
	Update(ctx context.Context, m *domain.Merchant) error
	UpdateFields(ctx context.Context, id string, fields map[string]any) (*domain.Merchant, error)
	TransitionKYC(ctx context.Context, id string, from, to domain.KYCStatus, reason, actor string) (*domain.Merchant, error)
	AddDocument(ctx context.Context, d *domain.MerchantKYCDocument) error
	ListDocuments(ctx context.Context, merchantID string) ([]*domain.MerchantKYCDocument, error)
	ReviewDocument(ctx context.Context, docID, status, note string) error
	ListKYCAudits(ctx context.Context, merchantID string, limit int) ([]*domain.MerchantKYCAudit, error)
	ListActive(ctx context.Context, limit int) ([]*domain.Merchant, error)
	SoftDelete(ctx context.Context, id string) error
	PurgeDeletedBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

type merchantRepo struct {
	mgr    *Manager
	router *ShardRouter
}

// NewMerchantRepository 构造。router 为 nil 时退化到单 meta DB（dev / 测试）。
func NewMerchantRepository(mgr *Manager, router *ShardRouter) MerchantRepository {
	return &merchantRepo{mgr: mgr, router: router}
}

func (r *merchantRepo) shardForID(ctx context.Context, base, id string) (*gorm.DB, string) {
	return shardForMerchant(ctx, r.mgr, r.router, base, id)
}

// metaTbl meta 表（merchant_kyc_audit / merchant_lookup）按 ctx 解 shadow 后缀即可，不分片。
func (r *merchantRepo) metaTbl(ctx context.Context, base string) string {
	return shadow.TableName(ctx, base)
}

func (r *merchantRepo) meta() *gorm.DB   { return r.mgr.GetMeta() }
func (r *merchantRepo) metaRO() *gorm.DB { return r.mgr.GetMetaRO() }

// ─── basic CRUD ──────────────────────────────────────────────────────────────

func (r *merchantRepo) Create(ctx context.Context, m *domain.Merchant) error {
	if m.ID == "" || m.ContactEmail == "" {
		return fmt.Errorf("%w: id and contact_email required", domain.ErrValidation)
	}
	if m.Country == "" {
		m.Country = "PH"
	}
	if m.SettleCurrency == "" {
		m.SettleCurrency = "PHP"
	}
	if m.BusinessType == "" {
		m.BusinessType = "individual"
	}
	if m.KYCStatus == "" {
		m.KYCStatus = domain.KYCStatusPending
	}
	if m.Status == "" {
		m.Status = domain.MerchantStatusPending
	}
	if m.RiskTier == "" {
		m.RiskTier = "standard"
	}
	if m.RateLimitRPS == 0 {
		m.RateLimitRPS = 100
	}
	db, tbl := r.shardForID(ctx, tblMerchant, m.ID)
	if err := db.WithContext(ctx).Table(tbl).Create(m).Error; err != nil {
		if isDupKey(err) {
			return fmt.Errorf("%w: duplicate email or id", domain.ErrValidation)
		}
		return err
	}
	// 同步反查索引（meta）。失败不阻断 Create —— 反查可由 admin 重建脚本兜底。
	r.upsertLookup(ctx, "email", m.ContactEmail, m.ID)
	if m.LiveKeyHash != "" {
		r.upsertLookup(ctx, "live_key_hash", m.LiveKeyHash, m.ID)
	}
	if m.TestKeyHash != "" {
		r.upsertLookup(ctx, "test_key_hash", m.TestKeyHash, m.ID)
	}
	return nil
}

// upsertLookup INSERT/UPDATE 一条 merchant_lookup，失败不抛错（best-effort）。
func (r *merchantRepo) upsertLookup(ctx context.Context, lookupType, lookupValue, merchantID string) {
	if lookupValue == "" {
		return
	}
	_ = r.meta().WithContext(ctx).Exec(
		"INSERT INTO "+r.metaTbl(ctx, tblMerchantLookup)+
			" (lookup_type, lookup_value, merchant_id) VALUES (?, ?, ?)"+
			" ON DUPLICATE KEY UPDATE merchant_id = VALUES(merchant_id)",
		lookupType, lookupValue, merchantID,
	).Error
}

func (r *merchantRepo) deleteLookup(ctx context.Context, lookupType, lookupValue string) {
	if lookupValue == "" {
		return
	}
	_ = r.meta().WithContext(ctx).Exec(
		"DELETE FROM "+r.metaTbl(ctx, tblMerchantLookup)+
			" WHERE lookup_type = ? AND lookup_value = ?",
		lookupType, lookupValue,
	).Error
}

// resolveLookup meta 查反查索引 → merchant_id。
func (r *merchantRepo) resolveLookup(ctx context.Context, lookupType, lookupValue string) (string, bool) {
	type row struct {
		MerchantID string `gorm:"column:merchant_id"`
	}
	var out row
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblMerchantLookup)).
		Where("lookup_type = ? AND lookup_value = ?", lookupType, lookupValue).
		Take(&out).Error
	if err != nil {
		return "", false
	}
	return out.MerchantID, true
}

func (r *merchantRepo) Get(ctx context.Context, id string) (*domain.Merchant, error) {
	db, tbl := r.shardForID(ctx, tblMerchant, id)
	var m domain.Merchant
	err := db.WithContext(ctx).Table(tbl).
		Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrMerchantNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// BatchGet 按 merchant_id 分组到对应 shard，并发查询。重复 id 在结果里合并。
// 上限 500 防 admin-web 传 10k ids 把 DB 打跪。
func (r *merchantRepo) BatchGet(ctx context.Context, ids []string) ([]*domain.Merchant, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	if r.router == nil || r.mgr.ShardCount() == 0 {
		// dev 单库模式：直接查 meta。
		var out []*domain.Merchant
		err := r.metaRO().WithContext(ctx).Table(shadow.TableName(ctx, tblMerchant)).
			Where("id IN ? AND deleted_at IS NULL", ids).Find(&out).Error
		return out, err
	}
	// 按 shard 分组
	type shardGroup struct {
		ids []string
		db  *gorm.DB
		tbl string
	}
	groups := make(map[int]*shardGroup)
	for _, id := range ids {
		dbIdx, gtbl := r.router.RouteByMerchantID(id)
		if g, ok := groups[gtbl]; ok {
			g.ids = append(g.ids, id)
		} else {
			groups[gtbl] = &shardGroup{
				ids: []string{id},
				db:  r.mgr.GetShard(dbIdx),
				tbl: r.router.TableName(ctx, tblMerchant, gtbl),
			}
		}
	}
	out := make([]*domain.Merchant, 0, len(ids))
	for _, g := range groups {
		var rows []*domain.Merchant
		if err := g.db.WithContext(ctx).Table(g.tbl).
			Where("id IN ? AND deleted_at IS NULL", g.ids).Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

func (r *merchantRepo) SoftDelete(ctx context.Context, id string) error {
	db, tbl := r.shardForID(ctx, tblMerchant, id)
	res := db.WithContext(ctx).Table(tbl).
		Where("id = ? AND deleted_at IS NULL", id).
		Update("deleted_at", time.Now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrMerchantNotFound
	}
	// 同步删反查索引（merchant 已软删，反查应该返 NotFound）
	if m, err := r.Get(ctx, id); err == nil && m != nil {
		r.deleteLookup(ctx, "email", m.ContactEmail)
		r.deleteLookup(ctx, "live_key_hash", m.LiveKeyHash)
		r.deleteLookup(ctx, "test_key_hash", m.TestKeyHash)
	}
	return nil
}

// PurgeDeletedBefore 真删 deleted_at<cutoff 的商户：fanout 全 100 分片，每分片
// 单事务删一批。
func (r *merchantRepo) PurgeDeletedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if r.router == nil || r.mgr.ShardCount() == 0 {
		return r.purgeOne(ctx, r.meta(), shadow.TableName(ctx, tblMerchant),
			shadow.TableName(ctx, tblMerchantKYCDocument), cutoff)
	}
	var total int64
	for _, s := range r.router.AllShards() {
		db := r.mgr.GetShard(s.DBIndex)
		mTbl := r.router.TableName(ctx, tblMerchant, s.TableIndex)
		dTbl := r.router.TableName(ctx, tblMerchantKYCDocument, s.TableIndex)
		n, err := r.purgeOne(ctx, db, mTbl, dTbl, cutoff)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (r *merchantRepo) purgeOne(ctx context.Context, db *gorm.DB, mTbl, dTbl string, cutoff time.Time) (int64, error) {
	var total int64
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ids []string
		if err := tx.Table(mTbl).
			Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
			Limit(5000).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		// kyc_document 跟 merchant 同 shard，可在同事务删。
		if err := tx.Table(dTbl).Where("merchant_id IN ?", ids).
			Delete(&domain.MerchantKYCDocument{}).Error; err != nil {
			return err
		}
		// kyc_audit 在 meta；purge 异步交给 retention 走，不在此事务里。
		res := tx.Table(mTbl).Where("id IN ?", ids).Delete(&domain.Merchant{})
		if res.Error != nil {
			return res.Error
		}
		total = res.RowsAffected
		return nil
	})
	return total, err
}

func (r *merchantRepo) GetByEmail(ctx context.Context, email string) (*domain.Merchant, error) {
	mid, ok := r.resolveLookup(ctx, "email", email)
	if !ok {
		return nil, domain.ErrMerchantNotFound
	}
	return r.Get(ctx, mid)
}

func (r *merchantRepo) GetByKeyHash(ctx context.Context, hash string) (*domain.Merchant, error) {
	// live_key_hash / test_key_hash 都查
	for _, kind := range []string{"live_key_hash", "test_key_hash"} {
		mid, ok := r.resolveLookup(ctx, kind, hash)
		if !ok {
			continue
		}
		return r.Get(ctx, mid)
	}
	return nil, domain.ErrMerchantNotFound
}

// List admin 翻页：跨 100 张分片表 fanout，再 limit/offset。
// 注意：跨 shard 的 ORDER BY created DESC + Limit/Offset 不能下推到 DB；
// 实现：每 shard 取 limit+offset 条，本地合并排序后切片。负载随 limit+offset 线性放大。
// 调用方应避免 deep pagination（offset > 1000）。
func (r *merchantRepo) List(ctx context.Context, status domain.MerchantStatus, kyc domain.KYCStatus, limit, offset int) ([]*domain.Merchant, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if r.router == nil || r.mgr.ShardCount() == 0 {
		return r.listOne(ctx, r.metaRO(), shadow.TableName(ctx, tblMerchant), status, kyc, limit, offset)
	}
	// 每 shard 拿 limit+offset 条 + total，本地合并。
	shards := r.router.AllShards()
	type shardOut struct {
		rows  []*domain.Merchant
		total int64
		err   error
	}
	results := make([]shardOut, len(shards))
	for i, s := range shards {
		db := r.mgr.GetShard(s.DBIndex)
		tbl := r.router.TableName(ctx, tblMerchant, s.TableIndex)
		rows, total, err := r.listOne(ctx, db, tbl, status, kyc, limit+offset, 0)
		results[i] = shardOut{rows: rows, total: total, err: err}
	}
	var allRows []*domain.Merchant
	var grandTotal int64
	for _, r := range results {
		if r.err != nil {
			return nil, 0, r.err
		}
		allRows = append(allRows, r.rows...)
		grandTotal += r.total
	}
	// 全局按 created DESC 排序
	sortByCreatedDesc(allRows)
	if offset >= len(allRows) {
		return nil, grandTotal, nil
	}
	end := offset + limit
	if end > len(allRows) {
		end = len(allRows)
	}
	return allRows[offset:end], grandTotal, nil
}

func (r *merchantRepo) listOne(ctx context.Context, db *gorm.DB, tbl string, status domain.MerchantStatus, kyc domain.KYCStatus, limit, offset int) ([]*domain.Merchant, int64, error) {
	q := db.WithContext(ctx).Table(tbl).Where("deleted_at IS NULL")
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if kyc != "" {
		q = q.Where("kyc_status = ?", kyc)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []*domain.Merchant
	if err := q.Order("created DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *merchantRepo) Update(ctx context.Context, m *domain.Merchant) error {
	db, tbl := r.shardForID(ctx, tblMerchant, m.ID)
	if err := db.WithContext(ctx).Table(tbl).Save(m).Error; err != nil {
		return err
	}
	// Update 可能改了 email / key_hash → 反查索引同步刷新（best-effort）。
	r.upsertLookup(ctx, "email", m.ContactEmail, m.ID)
	if m.LiveKeyHash != "" {
		r.upsertLookup(ctx, "live_key_hash", m.LiveKeyHash, m.ID)
	}
	if m.TestKeyHash != "" {
		r.upsertLookup(ctx, "test_key_hash", m.TestKeyHash, m.ID)
	}
	return nil
}

func (r *merchantRepo) UpdateFields(ctx context.Context, id string, fields map[string]any) (*domain.Merchant, error) {
	if len(fields) == 0 {
		return r.Get(ctx, id)
	}
	db, tbl := r.shardForID(ctx, tblMerchant, id)
	if err := db.WithContext(ctx).Table(tbl).
		Where("id = ?", id).Updates(fields).Error; err != nil {
		return nil, err
	}
	// 反查索引：能从 fields 里直接取到的就同步
	if v, ok := fields["contact_email"].(string); ok {
		r.upsertLookup(ctx, "email", v, id)
	}
	if v, ok := fields["live_key_hash"].(string); ok && v != "" {
		r.upsertLookup(ctx, "live_key_hash", v, id)
	}
	if v, ok := fields["test_key_hash"].(string); ok && v != "" {
		r.upsertLookup(ctx, "test_key_hash", v, id)
	}
	return r.Get(ctx, id)
}

// ─── KYC FSM ────────────────────────────────────────────────────────────────

func (r *merchantRepo) TransitionKYC(ctx context.Context, id string, from, to domain.KYCStatus, reason, actor string) (*domain.Merchant, error) {
	if !from.CanTransition(to) {
		return nil, fmt.Errorf("%w: %s → %s", domain.ErrMerchantKYCInvalidTransition, from, to)
	}
	db, tbl := r.shardForID(ctx, tblMerchant, id)
	auditTbl := r.metaTbl(ctx, tblMerchantKYCAudit)
	var updated *domain.Merchant
	// merchant 在 shard，audit 在 meta —— 跨 DB 不能单事务。
	// 妥协：先在 shard 单事务里 CAS 改 status，再在 meta 写一条 audit。
	// 失败时 audit 缺失，不影响业务（admin UI 看 KYC 历史会少一条）。
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Table(tbl).
			Where("id = ? AND kyc_status = ?", id, from).
			Updates(map[string]any{
				"kyc_status":      to,
				"kyc_reason":      reason,
				"kyc_reviewer":    actor,
				"kyc_reviewed_at": gorm.Expr("CURRENT_TIMESTAMP(3)"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			var cur domain.Merchant
			if err := tx.Table(tbl).Where("id = ?", id).First(&cur).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.ErrMerchantNotFound
			} else if err != nil {
				return err
			}
			return fmt.Errorf("%w: current=%s, tried from=%s", domain.ErrMerchantKYCInvalidTransition, cur.KYCStatus, from)
		}
		switch to {
		case domain.KYCStatusApproved:
			_ = tx.Table(tbl).Where("id = ?", id).Update("status", domain.MerchantStatusActive).Error
		case domain.KYCStatusSuspended:
			_ = tx.Table(tbl).Where("id = ?", id).Update("status", domain.MerchantStatusSuspended).Error
		case domain.KYCStatusRejected, domain.KYCStatusTerminated:
			_ = tx.Table(tbl).Where("id = ?", id).Update("status", domain.MerchantStatusTerminated).Error
		}
		var m domain.Merchant
		if err := tx.Table(tbl).Where("id = ?", id).First(&m).Error; err != nil {
			return err
		}
		updated = &m
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 写 audit 到 meta（跨 DB，不在主事务内）
	audit := &domain.MerchantKYCAudit{MerchantID: id, FromStatus: from, ToStatus: to, Reason: reason, Actor: actor}
	_ = r.meta().WithContext(ctx).Table(auditTbl).Create(audit).Error
	return updated, nil
}

// ─── documents ───────────────────────────────────────────────────────────────

func (r *merchantRepo) AddDocument(ctx context.Context, d *domain.MerchantKYCDocument) error {
	if d.MerchantID == "" || d.FileURL == "" {
		return fmt.Errorf("%w: merchant_id and file_url required", domain.ErrValidation)
	}
	if d.ReviewStatus == "" {
		d.ReviewStatus = "pending"
	}
	db, tbl := r.shardForID(ctx, tblMerchantKYCDocument, d.MerchantID)
	return db.WithContext(ctx).Table(tbl).Create(d).Error
}

func (r *merchantRepo) ListActive(ctx context.Context, limit int) ([]*domain.Merchant, error) {
	if limit <= 0 || limit > 50_000 {
		limit = 1000
	}
	if r.router == nil || r.mgr.ShardCount() == 0 {
		var out []*domain.Merchant
		err := r.meta().WithContext(ctx).Table(shadow.TableName(ctx, tblMerchant)).
			Where("status = ?", domain.MerchantStatusActive).
			Order("updated DESC").Limit(limit).Find(&out).Error
		return out, err
	}
	// fanout 100 分片，每片取 limit/100 + 余量
	perShard := limit/r.router.TotalTableCount() + 1
	if perShard < 10 {
		perShard = 10
	}
	var all []*domain.Merchant
	for _, s := range r.router.AllShards() {
		db := r.mgr.GetShard(s.DBIndex)
		tbl := r.router.TableName(ctx, tblMerchant, s.TableIndex)
		var rows []*domain.Merchant
		if err := db.WithContext(ctx).Table(tbl).
			Where("status = ?", domain.MerchantStatusActive).
			Order("updated DESC").Limit(perShard).Find(&rows).Error; err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	sortByUpdatedDesc(all)
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (r *merchantRepo) ListDocuments(ctx context.Context, merchantID string) ([]*domain.MerchantKYCDocument, error) {
	db, tbl := r.shardForID(ctx, tblMerchantKYCDocument, merchantID)
	var out []*domain.MerchantKYCDocument
	err := db.WithContext(ctx).Table(tbl).Where("merchant_id = ?", merchantID).
		Order("created DESC").Find(&out).Error
	return out, err
}

// ReviewDocument 只有 docID 时拿不到 merchant_id 路由 → fanout 100 分片找对应 row
// 再更新。这条路径只走 admin 后台 KYC 审核，QPS 极低，fanout 可接受。
//
// 实现：先 SELECT 找出哪个 shard 有这个 doc，再对那一片做 UPDATE。整体 100 次
// SELECT，避免对所有 shard 都打 UPDATE。
func (r *merchantRepo) ReviewDocument(ctx context.Context, docID, status, note string) error {
	if r.router == nil || r.mgr.ShardCount() == 0 {
		// dev 单库
		return r.meta().WithContext(ctx).Table(shadow.TableName(ctx, tblMerchantKYCDocument)).
			Where("id = ?", docID).
			Updates(map[string]any{"review_status": status, "review_note": note}).Error
	}
	for _, s := range r.router.AllShards() {
		db := r.mgr.GetShard(s.DBIndex)
		tbl := r.router.TableName(ctx, tblMerchantKYCDocument, s.TableIndex)
		var n int64
		if err := db.WithContext(ctx).Table(tbl).Where("id = ?", docID).
			Limit(1).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		// 命中 —— 单分片 UPDATE
		return db.WithContext(ctx).Table(tbl).
			Where("id = ?", docID).
			Updates(map[string]any{"review_status": status, "review_note": note}).Error
	}
	return domain.ErrMerchantNotFound
}

// ─── audit trail（meta，不分片）──────────────────────────────────────────────

func (r *merchantRepo) ListKYCAudits(ctx context.Context, merchantID string, limit int) ([]*domain.MerchantKYCAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []*domain.MerchantKYCAudit
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblMerchantKYCAudit)).
		Where("merchant_id = ?", merchantID).
		Order("created DESC").Limit(limit).Find(&out).Error
	return out, err
}

// ─── helpers ────────────────────────────────────────────────────────────────

// sortByCreatedDesc 按 Created DESC 排序（fanout 后本地合并用）。
func sortByCreatedDesc(rows []*domain.Merchant) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j-1].Created.Before(rows[j].Created); j-- {
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

func sortByUpdatedDesc(rows []*domain.Merchant) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j-1].Updated.Before(rows[j].Updated); j-- {
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}
