package model

type Event struct {
	Type    string `json:"type"`
	OrderID string `json:"order_id"`
	Amount  int64  `json:"amount"`
}

type Result struct {
	OrderID string `json:"order_id"`
	Rule    string `json:"rule"`
	Status  string `json:"status"`
}
