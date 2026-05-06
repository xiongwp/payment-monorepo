// Package repo: config-center 数据访问层。GORM over MySQL config_center_meta。
//
// 设计：
//   - 单 meta 库：写量小（admin 操作）+ 读量靠 SDK 本地 cache 兜，DB 不在热路径
//   - 5 个核心接口对应 service.Repo（PutVersion/Rollback/GetActive/
//     ListNamespaceForSnapshot/SinceVersion/ListVersions/Delete）
//   - PutVersion / Rollback 走事务：config_version + config_item + audit 三表齐全
//   - 历史完整保留在 config_version（append-only）；config_item 只是 active 指针
package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/config-center/internal/service"
)

// ─── ORM models ───────────────────────────────────────────────────────────

// ConfigNamespace meta 表。
type ConfigNamespace struct {
	ID          int64     `gorm:"primaryKey;column:id"`
	Name        string    `gorm:"column:name;uniqueIndex"`
	Description string    `gorm:"column:description"`
	Owner       string    `gorm:"column:owner"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (ConfigNamespace) TableName() string { return "config_namespace" }

// ConfigItem (namespace, key) 当前 active 指针。
type ConfigItem struct {
	ID             int64     `gorm:"primaryKey;column:id"`
	Namespace      string    `gorm:"column:namespace;uniqueIndex:uk_ns_key,priority:1"`
	KeyName        string    `gorm:"column:key_name;uniqueIndex:uk_ns_key,priority:2"`
	ActiveVersion  int64     `gorm:"column:active_version"`
	LatestVersion  int64     `gorm:"column:latest_version"`
	Deleted        int8      `gorm:"column:deleted"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (ConfigItem) TableName() string { return "config_item" }

// ConfigVersion append-only 历史。
type ConfigVersion struct {
	ID            int64      `gorm:"primaryKey;column:id"`
	Namespace     string     `gorm:"column:namespace"`
	KeyName       string     `gorm:"column:key_name"`
	Version       int64      `gorm:"column:version"`
	Value         string     `gorm:"column:value;type:mediumtext"`
	Format        string     `gorm:"column:format"`
	EffectiveAt   *time.Time `gorm:"column:effective_at"`
	ExpireAt      *time.Time `gorm:"column:expire_at"`
	Strategy      string     `gorm:"column:strategy"`
	StrategySpec  string     `gorm:"column:strategy_spec;type:json"`
	CreatedBy     string     `gorm:"column:created_by"`
	ChangeReason  string     `gorm:"column:change_reason"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
}

func (ConfigVersion) TableName() string { return "config_version" }

// ConfigSubscription 配置 ↔ 订阅服务多对多。
type ConfigSubscription struct {
	ID         int64     `gorm:"primaryKey;column:id"`
	ItemID     int64     `gorm:"column:item_id;uniqueIndex:uk_item_sub,priority:1"`
	Subscriber string    `gorm:"column:subscriber;uniqueIndex:uk_item_sub,priority:2"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (ConfigSubscription) TableName() string { return "config_subscription" }

// ConfigAuditLog 仅 metadata；before/after 走 config_version join。
type ConfigAuditLog struct {
	ID            int64     `gorm:"primaryKey;column:id"`
	Namespace     string    `gorm:"column:namespace"`
	KeyName       string    `gorm:"column:key_name"`
	Op            string    `gorm:"column:op"`
	VersionBefore *int64    `gorm:"column:version_before"`
	VersionAfter  *int64    `gorm:"column:version_after"`
	Actor         string    `gorm:"column:actor"`
	ActorIP       string    `gorm:"column:actor_ip"`
	UserAgent     string    `gorm:"column:user_agent"`
	ChangeReason  string    `gorm:"column:change_reason"`
	TraceID       string    `gorm:"column:trace_id"`
	CreatedAt     time.Time `gorm:"column:created_at"`
}

func (ConfigAuditLog) TableName() string { return "config_audit_log" }

// ─── Repo impl ────────────────────────────────────────────────────────────

// Repo GORM 实现。
type Repo struct {
	db *gorm.DB
}

// NewRepo 构造。
func NewRepo(db *gorm.DB) *Repo {
	return &Repo{db: db}
}

// 编译时断言：满足 service.Repo（核心 5 接口）+ service.AdminRepo（admin 用）
var (
	_ service.Repo      = (*Repo)(nil)
	_ service.AdminRepo = (*Repo)(nil)
)

// rowToConfigRow ORM → service 跨包形态。
func rowToConfigRow(v *ConfigVersion) *service.ConfigRow {
	return &service.ConfigRow{
		ID:           v.ID,
		Namespace:    v.Namespace,
		KeyName:      v.KeyName,
		Version:      v.Version,
		Value:        v.Value,
		Format:       v.Format,
		EffectiveAt:  v.EffectiveAt,
		ExpireAt:     v.ExpireAt,
		Strategy:     v.Strategy,
		StrategySpec: v.StrategySpec,
		CreatedBy:    v.CreatedBy,
		ChangeReason: v.ChangeReason,
		CreatedAt:    v.CreatedAt,
	}
}

// PutVersion 事务：append config_version + 更新 config_item + 写 audit。
//
// 单调递增逻辑：在事务内 SELECT MAX(version) FOR UPDATE，避免并发同 key 写冲突。
// (namespace, key) 上有 unique key 兜底（uk_ns_key_version）。
func (r *Repo) PutVersion(ctx context.Context, in service.PutVersionInput) (int64, error) {
	if in.Format == "" {
		in.Format = "json"
	}
	var newVersion int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 找 / 建 config_item 行；锁住对应 row
		item, latestVer, err := r.upsertItemForUpdate(tx, in.Namespace, in.Key)
		if err != nil {
			return err
		}
		newVersion = latestVer + 1

		// 2) 写新 version
		// strategy_spec 是 MySQL 8 JSON 列，空字符串 "" 会被 MySQL 拒绝
		// (Error 3140 'The document is empty')。FULL 策略不需要 spec → 空串 ""
		// 时写 JSON null 字面量（合法 JSON、长度 4 字节）。
		strategySpec := in.StrategySpec
		if strategySpec == "" {
			strategySpec = "null"
		}
		ver := &ConfigVersion{
			Namespace:    in.Namespace,
			KeyName:      in.Key,
			Version:      newVersion,
			Value:        in.Value,
			Format:       in.Format,
			EffectiveAt:  in.EffectiveAt,
			ExpireAt:     in.ExpireAt,
			Strategy:     in.Strategy,
			StrategySpec: strategySpec,
			CreatedBy:    in.Actor,
			ChangeReason: in.ChangeReason,
		}
		if err := tx.Create(ver).Error; err != nil {
			return fmt.Errorf("insert config_version: %w", err)
		}

		// 3) 更新 config_item.active + latest 指针
		//    立即生效（EffectiveAt nil / 已过去）→ active=新；
		//    未来才生效 → active 不动，仅推 latest（SDK 收到后存 pending）
		now := time.Now()
		newActive := item.ActiveVersion
		if in.EffectiveAt == nil || !in.EffectiveAt.After(now) {
			newActive = ver.ID
		}
		updates := map[string]interface{}{
			"latest_version": ver.ID,
			"active_version": newActive,
			"deleted":        0,
		}
		if err := tx.Model(&ConfigItem{}).
			Where("namespace = ? AND key_name = ?", in.Namespace, in.Key).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update config_item: %w", err)
		}

		// 4) audit log
		audit := &ConfigAuditLog{
			Namespace:     in.Namespace,
			KeyName:       in.Key,
			Op:            opForPut(item.ActiveVersion),
			VersionBefore: ptrInt64(item.ActiveVersion),
			VersionAfter:  ptrInt64(ver.ID),
			Actor:         in.Actor,
			ChangeReason:  in.ChangeReason,
		}
		if err := tx.Create(audit).Error; err != nil {
			return fmt.Errorf("insert audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newVersion, nil
}

// upsertItemForUpdate 锁住 (namespace, key) 行；不存在就建。返当前 latest。
func (r *Repo) upsertItemForUpdate(tx *gorm.DB, namespace, key string) (*ConfigItem, int64, error) {
	// 先 select for update
	var item ConfigItem
	err := tx.Set("gorm:query_option", "FOR UPDATE").
		Where("namespace = ? AND key_name = ?", namespace, key).
		First(&item).Error
	if err == nil {
		return &item, item.LatestVersion, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, 0, err
	}
	// 不存在：插一条 active=0
	item = ConfigItem{
		Namespace:     namespace,
		KeyName:       key,
		ActiveVersion: 0,
		LatestVersion: 0,
	}
	if err := tx.Create(&item).Error; err != nil {
		return nil, 0, err
	}
	return &item, 0, nil
}

func opForPut(prevActive int64) string {
	if prevActive == 0 {
		return "CREATE"
	}
	return "PUT"
}

func ptrInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// Rollback 复制旧 version 的 value/format/strategy 写新 version 号；active 指过去。
func (r *Repo) Rollback(ctx context.Context, namespace, key string, toVersion int64, actor, reason string) (int64, error) {
	if toVersion <= 0 {
		return 0, errors.New("to_version must be > 0")
	}
	var newVersion int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		item, latestVer, err := r.upsertItemForUpdate(tx, namespace, key)
		if err != nil {
			return err
		}
		// 找回滚目标
		var src ConfigVersion
		if err := tx.Where("namespace = ? AND key_name = ? AND version = ?", namespace, key, toVersion).
			First(&src).Error; err != nil {
			return fmt.Errorf("rollback target version=%d not found: %w", toVersion, err)
		}
		newVersion = latestVer + 1

		// 复制成新 version；effective_at 设 nil（立即生效）
		// 同 PutVersion：strategy_spec 为 JSON 列，保证写入合法 JSON。
		rollbackSpec := src.StrategySpec
		if rollbackSpec == "" {
			rollbackSpec = "null"
		}
		ver := &ConfigVersion{
			Namespace:    namespace,
			KeyName:      key,
			Version:      newVersion,
			Value:        src.Value,
			Format:       src.Format,
			EffectiveAt:  nil,
			ExpireAt:     src.ExpireAt,
			Strategy:     src.Strategy,
			StrategySpec: rollbackSpec,
			CreatedBy:    actor,
			ChangeReason: fmt.Sprintf("rollback to v%d: %s", toVersion, reason),
		}
		if err := tx.Create(ver).Error; err != nil {
			return fmt.Errorf("insert rollback version: %w", err)
		}
		updates := map[string]interface{}{
			"latest_version": ver.ID,
			"active_version": ver.ID,
		}
		if err := tx.Model(&ConfigItem{}).
			Where("namespace = ? AND key_name = ?", namespace, key).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update item on rollback: %w", err)
		}
		audit := &ConfigAuditLog{
			Namespace:     namespace,
			KeyName:       key,
			Op:            "ROLLBACK",
			VersionBefore: ptrInt64(item.ActiveVersion),
			VersionAfter:  ptrInt64(ver.ID),
			Actor:         actor,
			ChangeReason:  fmt.Sprintf("rollback to v%d: %s", toVersion, reason),
		}
		if err := tx.Create(audit).Error; err != nil {
			return fmt.Errorf("insert audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newVersion, nil
}

// GetActive (namespace, key) 当前生效行（active_version 指的那条）。
func (r *Repo) GetActive(ctx context.Context, namespace, key string) (*service.ConfigRow, error) {
	var item ConfigItem
	err := r.db.WithContext(ctx).
		Where("namespace = ? AND key_name = ? AND deleted = 0", namespace, key).
		First(&item).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if item.ActiveVersion == 0 {
		return nil, nil
	}
	var v ConfigVersion
	if err := r.db.WithContext(ctx).First(&v, item.ActiveVersion).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return rowToConfigRow(&v), nil
}

// ListNamespaceForSnapshot 给 watch 初始 snapshot 用。
//
// 每个 key 都返：
//   - 当前生效版本（item.active_version 指的）
//   - 未来即将生效的所有 version（version > active 且 effective_at > now）
//
// 客户端 SDK 收后按 IsEffective(now) 自动归位 active vs pending 槽。
func (r *Repo) ListNamespaceForSnapshot(ctx context.Context, namespace string) ([]*service.ConfigRow, error) {
	// 1) 拿所有 active item
	var items []ConfigItem
	if err := r.db.WithContext(ctx).
		Where("namespace = ? AND deleted = 0 AND active_version > 0", namespace).
		Find(&items).Error; err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	activeIDs := make([]int64, 0, len(items))
	for _, it := range items {
		activeIDs = append(activeIDs, it.ActiveVersion)
	}
	var activeRows []ConfigVersion
	if err := r.db.WithContext(ctx).Where("id IN ?", activeIDs).Find(&activeRows).Error; err != nil {
		return nil, err
	}
	// 2) 拿所有未来版本（同 namespace 下 effective_at > now 的所有 version）
	now := time.Now()
	var futureRows []ConfigVersion
	if err := r.db.WithContext(ctx).
		Where("namespace = ? AND effective_at IS NOT NULL AND effective_at > ?", namespace, now).
		Order("effective_at ASC").
		Find(&futureRows).Error; err != nil {
		return nil, err
	}
	out := make([]*service.ConfigRow, 0, len(activeRows)+len(futureRows))
	for i := range activeRows {
		out = append(out, rowToConfigRow(&activeRows[i]))
	}
	for i := range futureRows {
		out = append(out, rowToConfigRow(&futureRows[i]))
	}
	return out, nil
}

// SinceVersion resume 用：取 config_version.id > since 的所有行（namespace 下）。
func (r *Repo) SinceVersion(ctx context.Context, namespace string, since int64) ([]*service.ConfigRow, error) {
	var rows []ConfigVersion
	if err := r.db.WithContext(ctx).
		Where("namespace = ? AND id > ?", namespace, since).
		Order("id ASC").
		Limit(1000). // 单次重连最多补 1000 条；超过的下次再来
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*service.ConfigRow, 0, len(rows))
	for i := range rows {
		out = append(out, rowToConfigRow(&rows[i]))
	}
	return out, nil
}

// ListVersions 单 (namespace, key) 历史 version 倒序，admin 详情页用。
func (r *Repo) ListVersions(ctx context.Context, namespace, key string, limit int) ([]*service.ConfigRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var rows []ConfigVersion
	if err := r.db.WithContext(ctx).
		Where("namespace = ? AND key_name = ?", namespace, key).
		Order("version DESC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*service.ConfigRow, 0, len(rows))
	for i := range rows {
		out = append(out, rowToConfigRow(&rows[i]))
	}
	return out, nil
}

// Delete 软删 + audit。active 清 0；config_version 历史保留（合规）。
func (r *Repo) Delete(ctx context.Context, namespace, key, actor, reason string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item ConfigItem
		err := tx.Where("namespace = ? AND key_name = ?", namespace, key).First(&item).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := tx.Model(&ConfigItem{}).
			Where("id = ?", item.ID).
			Updates(map[string]interface{}{"deleted": 1, "active_version": 0}).
			Error; err != nil {
			return err
		}
		audit := &ConfigAuditLog{
			Namespace:     namespace,
			KeyName:       key,
			Op:            "DELETE",
			VersionBefore: ptrInt64(item.ActiveVersion),
			Actor:         actor,
			ChangeReason:  reason,
		}
		return tx.Create(audit).Error
	})
}

// ─── Admin / search 辅助 ──────────────────────────────────────────────────
// 这些不在 service.Repo 接口里；admin handler 直接拿 *Repo 用。

// itemToView ORM → service DTO。
func itemToView(it *ConfigItem) *service.ConfigItemView {
	return &service.ConfigItemView{
		ID:            it.ID,
		Namespace:     it.Namespace,
		KeyName:       it.KeyName,
		ActiveVersion: it.ActiveVersion,
		LatestVersion: it.LatestVersion,
		UpdatedAt:     it.UpdatedAt,
	}
}

// SearchItems 全平台 item 检索：按 key 模糊 + 按订阅服务过滤。
func (r *Repo) SearchItems(ctx context.Context, q, subscriber string, limit int) ([]*service.ConfigItemView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	tx := r.db.WithContext(ctx).Model(&ConfigItem{}).Where("deleted = 0")
	if q != "" {
		tx = tx.Where("key_name LIKE ? OR namespace LIKE ?", "%"+q+"%", "%"+q+"%")
	}
	if subscriber != "" {
		// JOIN config_subscription 过滤
		tx = tx.Joins("JOIN config_subscription cs ON cs.item_id = config_item.id").
			Where("cs.subscriber = ?", subscriber)
	}
	var items []*ConfigItem
	if err := tx.Order("updated_at DESC").Limit(limit).Find(&items).Error; err != nil {
		return nil, err
	}
	out := make([]*service.ConfigItemView, 0, len(items))
	for _, it := range items {
		out = append(out, itemToView(it))
	}
	if err := r.fillVersionNums(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListItemsByNamespace 单 namespace 全部 item（按 key_name 排序）。
//
// 同时一次性查 config_version.version（per-key 序号）填到 view 的
// ActiveVersionNum / LatestVersionNum，admin UI 列表展示用 — 不要直接
// 展示 config_item.active_version（那是全局自增 PK，跨 key 累加值）。
func (r *Repo) ListItemsByNamespace(ctx context.Context, namespace string) ([]*service.ConfigItemView, error) {
	var items []*ConfigItem
	if err := r.db.WithContext(ctx).
		Where("namespace = ? AND deleted = 0", namespace).
		Order("key_name ASC").
		Find(&items).Error; err != nil {
		return nil, err
	}
	out := make([]*service.ConfigItemView, 0, len(items))
	for _, it := range items {
		out = append(out, itemToView(it))
	}
	if err := r.fillVersionNums(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillVersionNums 批量填 view 里的 ActiveVersionNum / LatestVersionNum。
//
// items.{Active,Latest}Version 是 config_version.id（全局 PK），需 join
// 拿到 config_version.version（per-key 序号 v1/v2/v3）。一次 IN 查询。
func (r *Repo) fillVersionNums(ctx context.Context, views []*service.ConfigItemView) error {
	if len(views) == 0 {
		return nil
	}
	idSet := make(map[int64]struct{}, len(views)*2)
	for _, v := range views {
		if v.ActiveVersion > 0 {
			idSet[v.ActiveVersion] = struct{}{}
		}
		if v.LatestVersion > 0 {
			idSet[v.LatestVersion] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	type idVer struct {
		ID      int64
		Version int64
	}
	var rows []idVer
	if err := r.db.WithContext(ctx).
		Table("config_version").
		Select("id, version").
		Where("id IN ?", ids).
		Find(&rows).Error; err != nil {
		return err
	}
	idToVer := make(map[int64]int64, len(rows))
	for _, row := range rows {
		idToVer[row.ID] = row.Version
	}
	for _, v := range views {
		if n, ok := idToVer[v.ActiveVersion]; ok {
			v.ActiveVersionNum = n
		}
		if n, ok := idToVer[v.LatestVersion]; ok {
			v.LatestVersionNum = n
		}
	}
	return nil
}

// GetVersion 单条 version 行 by id。diff 页用。
func (r *Repo) GetVersion(ctx context.Context, id int64) (*service.ConfigRow, error) {
	var v ConfigVersion
	if err := r.db.WithContext(ctx).First(&v, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return rowToConfigRow(&v), nil
}

// GetSubscribers 一条 config_item 当前订阅服务名列表。
func (r *Repo) GetSubscribers(ctx context.Context, itemID int64) ([]string, error) {
	var subs []ConfigSubscription
	if err := r.db.WithContext(ctx).
		Where("item_id = ?", itemID).
		Find(&subs).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.Subscriber)
	}
	return out, nil
}

// SetSubscribers 重置订阅；diff (旧, 新) 后增删，audit。
func (r *Repo) SetSubscribers(ctx context.Context, itemID int64, subscribers []string, actor string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 拿旧的
		var oldSubs []ConfigSubscription
		if err := tx.Where("item_id = ?", itemID).Find(&oldSubs).Error; err != nil {
			return err
		}
		oldSet := make(map[string]int64, len(oldSubs))
		for _, s := range oldSubs {
			oldSet[s.Subscriber] = s.ID
		}
		newSet := make(map[string]struct{}, len(subscribers))
		for _, s := range subscribers {
			newSet[s] = struct{}{}
		}
		// 2) 新增
		for sub := range newSet {
			if _, ok := oldSet[sub]; ok {
				continue
			}
			if err := tx.Create(&ConfigSubscription{ItemID: itemID, Subscriber: sub}).Error; err != nil {
				return err
			}
		}
		// 3) 删除
		for sub, id := range oldSet {
			if _, ok := newSet[sub]; ok {
				continue
			}
			if err := tx.Delete(&ConfigSubscription{}, id).Error; err != nil {
				return err
			}
		}
		// 4) 拿 namespace / key 写一条 audit
		var item ConfigItem
		if err := tx.First(&item, itemID).Error; err == nil {
			tx.Create(&ConfigAuditLog{
				Namespace:    item.Namespace,
				KeyName:      item.KeyName,
				Op:           "SUBSCRIBE_SET",
				Actor:        actor,
				ChangeReason: fmt.Sprintf("subs=%v", subscribers),
			})
		}
		return nil
	})
}

// RecentAudit 最近的 audit log（admin /audit 页）。
func (r *Repo) RecentAudit(ctx context.Context, limit int) ([]*service.ConfigAuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var logs []*ConfigAuditLog
	if err := r.db.WithContext(ctx).
		Order("id DESC").
		Limit(limit).
		Find(&logs).Error; err != nil {
		return nil, err
	}
	out := make([]*service.ConfigAuditEntry, 0, len(logs))
	for _, l := range logs {
		out = append(out, &service.ConfigAuditEntry{
			ID:            l.ID,
			Namespace:     l.Namespace,
			KeyName:       l.KeyName,
			Op:            l.Op,
			VersionBefore: l.VersionBefore,
			VersionAfter:  l.VersionAfter,
			Actor:         l.Actor,
			ActorIP:       l.ActorIP,
			ChangeReason:  l.ChangeReason,
			TraceID:       l.TraceID,
			CreatedAt:     l.CreatedAt,
		})
	}
	return out, nil
}

// ListNamespaces admin 首页用。
func (r *Repo) ListNamespaces(ctx context.Context) ([]*ConfigNamespace, error) {
	var nss []*ConfigNamespace
	if err := r.db.WithContext(ctx).Order("name ASC").Find(&nss).Error; err != nil {
		return nil, err
	}
	return nss, nil
}

// EnsureNamespace 启动期 / put 时按需自动建。
func (r *Repo) EnsureNamespace(ctx context.Context, name, owner string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ns ConfigNamespace
		err := tx.Where("name = ?", name).First(&ns).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&ConfigNamespace{Name: name, Owner: owner}).Error
	})
}

// FindItemID 查 (namespace, key) 的 item id；admin 改订阅 / 详情页用。
func (r *Repo) FindItemID(ctx context.Context, namespace, key string) (int64, error) {
	var item ConfigItem
	if err := r.db.WithContext(ctx).
		Select("id").
		Where("namespace = ? AND key_name = ?", namespace, key).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return item.ID, nil
}
