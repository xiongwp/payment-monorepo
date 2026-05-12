// gorm_models.go — GORM-mapped models. 跟 types.go 的纯 domain types 分离.
//
// 设计原则:
//   - GORM tags 只在这里; types.go 纯 JSON-friendly types
//   - Repository 层做转换 (gorm model ↔ domain types)
//   - 业务侧 (service / handler) 只见 domain types

package domain

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// RequestGormModel GORM 映射. 跟 Request type 同字段, 加 GORM tags.
type RequestGormModel struct {
	RequestID       string       `gorm:"primaryKey;column:request_id;type:varchar(48)"`
	Type            string       `gorm:"column:type;type:varchar(32);index"`
	SubjectType     string       `gorm:"column:subject_type;type:varchar(16);index:idx_subject"`
	SubjectID       string       `gorm:"column:subject_id;type:varchar(128);index:idx_subject"`
	SubjectEmail    string       `gorm:"column:subject_email;type:varchar(255)"`
	SubjectCountry  string       `gorm:"column:subject_country;type:char(2)"`
	Jurisdiction    string       `gorm:"column:jurisdiction;type:varchar(8);index:idx_juris_year"`
	State           string       `gorm:"column:state;type:varchar(16);index:idx_state_deadline,priority:1"`
	SubmittedAt     time.Time    `gorm:"column:submitted_at;index:idx_juris_year,priority:2"`
	DeadlineAt      time.Time    `gorm:"column:deadline_at;index:idx_state_deadline,priority:2"`
	ApprovedBy      string       `gorm:"column:approved_by;type:varchar(128)"`
	ApprovedAt      *time.Time   `gorm:"column:approved_at"`
	FulfilledAt     *time.Time   `gorm:"column:fulfilled_at"`
	RejectReason    string       `gorm:"column:reject_reason;type:varchar(512)"`
	ExportURL       string       `gorm:"column:export_url;type:varchar(512)"`
	ExportSHA256    string       `gorm:"column:export_sha256;type:char(64)"`
	VerificationRaw JSONRawValue `gorm:"column:verification_json;type:json"`
}

func (RequestGormModel) TableName() string { return "requests" }

// ServiceStatusGormModel ── per-service fan-out 进度
type ServiceStatusGormModel struct {
	RequestID     string    `gorm:"primaryKey;column:request_id;type:varchar(48)"`
	Service       string    `gorm:"primaryKey;column:service;type:varchar(64)"`
	Endpoint      string    `gorm:"column:endpoint;type:varchar(255)"`
	State         string    `gorm:"column:state;type:varchar(32);index"`
	Attempts      int       `gorm:"column:attempts"`
	LastAttemptAt time.Time `gorm:"column:last_attempt_at"`
	Error         string    `gorm:"column:error;type:varchar(512)"`
	ExportSize    int64     `gorm:"column:export_size"`
	ExportSHA     string    `gorm:"column:export_sha;type:char(64)"`
	Held          bool      `gorm:"column:held"`
	HoldReason    string    `gorm:"column:hold_reason;type:varchar(512)"`
}

func (ServiceStatusGormModel) TableName() string { return "service_statuses" }

// JSONRawValue — GORM 自定义 JSON 列类型, 解 / 拼 Verification struct.
type JSONRawValue []byte

func (j *JSONRawValue) Scan(value interface{}) error {
	if value == nil {
		*j = nil
		return nil
	}
	switch v := value.(type) {
	case []byte:
		*j = append((*j)[:0], v...)
		return nil
	case string:
		*j = []byte(v)
		return nil
	}
	return errors.New("JSONRawValue: unsupported type")
}

func (j JSONRawValue) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return string(j), nil
}

// FromDomain Request → GORM model.
func FromDomainRequest(r Request) RequestGormModel {
	m := RequestGormModel{
		RequestID:      r.RequestID,
		Type:           string(r.Type),
		SubjectType:    string(r.Subject.Type),
		SubjectID:      r.Subject.ID,
		SubjectEmail:   r.Subject.Email,
		SubjectCountry: r.Subject.Country,
		Jurisdiction:   string(r.Jurisdiction),
		State:          string(r.State),
		SubmittedAt:    r.SubmittedAt,
		DeadlineAt:     r.DeadlineAt,
		ApprovedBy:     r.ApprovedBy,
		RejectReason:   r.RejectReason,
		ExportURL:      r.ExportURL,
		ExportSHA256:   r.ExportSHA256,
	}
	if !r.ApprovedAt.IsZero() {
		t := r.ApprovedAt
		m.ApprovedAt = &t
	}
	if !r.FulfilledAt.IsZero() {
		t := r.FulfilledAt
		m.FulfilledAt = &t
	}
	if b, err := json.Marshal(r.Verification); err == nil {
		m.VerificationRaw = b
	}
	return m
}

// ToDomain GORM → Request.
func (m RequestGormModel) ToDomain() Request {
	r := Request{
		RequestID:    m.RequestID,
		Type:         RequestType(m.Type),
		Subject:      Subject{Type: SubjectType(m.SubjectType), ID: m.SubjectID, Email: m.SubjectEmail, Country: m.SubjectCountry},
		Jurisdiction: Jurisdiction(m.Jurisdiction),
		State:        State(m.State),
		SubmittedAt:  m.SubmittedAt,
		DeadlineAt:   m.DeadlineAt,
		ApprovedBy:   m.ApprovedBy,
		RejectReason: m.RejectReason,
		ExportURL:    m.ExportURL,
		ExportSHA256: m.ExportSHA256,
	}
	if m.ApprovedAt != nil {
		r.ApprovedAt = *m.ApprovedAt
	}
	if m.FulfilledAt != nil {
		r.FulfilledAt = *m.FulfilledAt
	}
	if len(m.VerificationRaw) > 0 {
		_ = json.Unmarshal(m.VerificationRaw, &r.Verification)
	}
	return r
}
