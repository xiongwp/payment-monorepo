// gorm_store.go — SQL-backed store. 替换原 in-memory map.
//
// GORM models 跟 payment-util/approval/types.go 字段对齐.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// ActionGormModel — 跟 Action 双向转换.
type ActionGormModel struct {
	ID                string    `gorm:"primaryKey;column:id;type:varchar(48)"`
	Type              string    `gorm:"column:type;type:varchar(64);index:idx_type_state"`
	Resource          string    `gorm:"column:resource;type:varchar(128);index"`
	Requester         string    `gorm:"column:requester;type:varchar(128);index"`
	RequesterAt       time.Time `gorm:"column:requester_at;index:idx_requester_at"`
	RequestNote       string    `gorm:"column:request_note;type:varchar(512)"`
	RequiredApprovals int       `gorm:"column:required_approvals"`
	State             string    `gorm:"column:state;type:varchar(16);index:idx_type_state"`
	PayloadJSON       []byte    `gorm:"column:payload_json;type:json"`
	ApprovalsJSON     []byte    `gorm:"column:approvals_json;type:json"`
	ExpiresAt         time.Time `gorm:"column:expires_at;index"`
}

func (ActionGormModel) TableName() string { return "approval_actions" }

func fromAction(a *Action) ActionGormModel {
	pj, _ := json.Marshal(a.Payload)
	apj, _ := json.Marshal(a.Approvals)
	return ActionGormModel{
		ID:                a.ID,
		Type:              a.Type,
		Resource:          a.Resource,
		Requester:         a.Requester,
		RequesterAt:       a.RequesterAt,
		RequestNote:       a.RequestNote,
		RequiredApprovals: a.RequiredApprovals,
		State:             string(a.State),
		PayloadJSON:       pj,
		ApprovalsJSON:     apj,
		ExpiresAt:         a.ExpiresAt,
	}
}

func (m ActionGormModel) toAction() *Action {
	a := &Action{
		ID:                m.ID,
		Type:              m.Type,
		Resource:          m.Resource,
		Requester:         m.Requester,
		RequesterAt:       m.RequesterAt,
		RequestNote:       m.RequestNote,
		RequiredApprovals: m.RequiredApprovals,
		State:             State(m.State),
		ExpiresAt:         m.ExpiresAt,
	}
	if len(m.PayloadJSON) > 0 {
		_ = json.Unmarshal(m.PayloadJSON, &a.Payload)
	}
	if len(m.ApprovalsJSON) > 0 {
		_ = json.Unmarshal(m.ApprovalsJSON, &a.Approvals)
	}
	return a
}

// GormStore 实现 in-memory Server.mu+map 等价的 GORM 版本.
type GormStore struct {
	db  *gorm.DB
	log *zap.Logger
}

func NewGormStore(db *gorm.DB, log *zap.Logger) *GormStore {
	return &GormStore{db: db, log: log}
}

var ErrActionNotFound = errors.New("action not found")

func (s *GormStore) Create(a *Action) error {
	m := fromAction(a)
	return s.db.WithContext(context.Background()).Create(&m).Error
}

func (s *GormStore) Get(id string) (*Action, error) {
	var m ActionGormModel
	err := s.db.Where("id = ?", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrActionNotFound
	}
	if err != nil {
		return nil, err
	}
	return m.toAction(), nil
}

func (s *GormStore) Update(a *Action) error {
	m := fromAction(a)
	return s.db.Save(&m).Error
}

// List by state/type 过滤.
func (s *GormStore) List(state, typ string, limit int) ([]*Action, error) {
	q := s.db.Model(&ActionGormModel{}).Order("requester_at DESC")
	if state != "" {
		q = q.Where("state = ?", state)
	}
	if typ != "" {
		q = q.Where("type = ?", typ)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	var ms []ActionGormModel
	if err := q.Find(&ms).Error; err != nil {
		return nil, err
	}
	out := make([]*Action, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.toAction())
	}
	return out, nil
}
