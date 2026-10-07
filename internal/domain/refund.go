package domain

import "time"

// RefundRequest 退款请求：针对一笔已受理成功的支付做全额或部分退款。
type RefundRequest struct {
	IdempotencyKey string
	PaymentID      string
	Amount         Money
	Currency       Currency
	Reason         string
}

// Refund 一笔成功退款的记录。
type Refund struct {
	ID             string
	IdempotencyKey string
	PaymentID      string
	Amount         Money
	Currency       Currency
	Reason         string
	JournalID      string
	CreatedAt      time.Time
}

// RefundFingerprint 退款幂等键之外请求内容的指纹。
type RefundFingerprint struct {
	PaymentID string
	Amount    Money
	Currency  Currency
	Reason    string
}

func RefundFingerprintOf(req *RefundRequest) RefundFingerprint {
	return RefundFingerprint{
		PaymentID: req.PaymentID,
		Amount:    req.Amount,
		Currency:  req.Currency,
		Reason:    req.Reason,
	}
}
