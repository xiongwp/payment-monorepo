package service

import (
	"encoding/json"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// 事件 payload 用扁平 JSON：避免外部消费者 (python ML pipeline / SOC dashboard)
// 跟 risk-manage 的 internal/engine 包耦合。字段子集为业务消费够用的。

type screenEventPayload struct {
	DecisionID  string `json:"decision_id"`
	Decision    string `json:"decision"`
	RiskScore   int    `json:"risk_score"`
	RiskLevel   string `json:"risk_level"`
	MerchantID  string `json:"merchant_id"`
	CustomerID  string `json:"customer_id,omitempty"`
	IPAddress   string `json:"ip_address,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	Amount      int64  `json:"amount,omitempty"`
	Currency    string `json:"currency,omitempty"`
	EventType   string `json:"event_type,omitempty"`
	HitRuleIDs  []string `json:"hit_rule_ids,omitempty"`
}

func encodeScreenEvent(txn *engine.TxnContext, res *engine.Result) ([]byte, error) {
	rules := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		rules = append(rules, h.RuleID)
	}
	return json.Marshal(screenEventPayload{
		DecisionID: res.DecisionID,
		Decision:   res.Decision.String(),
		RiskScore:  res.RiskScore,
		RiskLevel:  res.RiskLevel,
		MerchantID: txn.MerchantID,
		CustomerID: txn.CustomerID,
		IPAddress:  txn.IPAddress,
		DeviceID:   txn.DeviceID,
		Amount:     txn.Amount,
		Currency:   txn.Currency,
		EventType:  txn.EventType,
		HitRuleIDs: rules,
	})
}

type hitEventPayload struct {
	DecisionID string `json:"decision_id,omitempty"`
	RuleID     string `json:"rule_id"`
	RuleName   string `json:"rule_name,omitempty"`
	Decision   string `json:"decision"`
	Detail     string `json:"detail,omitempty"`
	MerchantID string `json:"merchant_id"`
	CustomerID string `json:"customer_id,omitempty"`
}

func encodeHitEvent(txn *engine.TxnContext, h *engine.Hit) ([]byte, error) {
	return json.Marshal(hitEventPayload{
		RuleID:     h.RuleID,
		RuleName:   h.RuleName,
		Decision:   h.Decision.String(),
		Detail:     h.Detail,
		MerchantID: txn.MerchantID,
		CustomerID: txn.CustomerID,
	})
}

type reportEventPayload struct {
	EventType  string `json:"event_type"`
	MerchantID string `json:"merchant_id"`
	CustomerID string `json:"customer_id,omitempty"`
	IPAddress  string `json:"ip_address,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	Amount     int64  `json:"amount,omitempty"`
	Currency   string `json:"currency,omitempty"`
	PIID       string `json:"pi_id,omitempty"`
}

func encodeReportEvent(txn *engine.TxnContext, eventType string) ([]byte, error) {
	return json.Marshal(reportEventPayload{
		EventType:  eventType,
		MerchantID: txn.MerchantID,
		CustomerID: txn.CustomerID,
		IPAddress:  txn.IPAddress,
		DeviceID:   txn.DeviceID,
		Amount:     txn.Amount,
		Currency:   txn.Currency,
		PIID:       txn.PaymentIntentID,
	})
}
