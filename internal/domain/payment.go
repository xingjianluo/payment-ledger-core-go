package domain

import "time"

type PaymentStatus string

const (
	PaymentStatusPendingSettlement PaymentStatus = "PENDING_SETTLEMENT"
	PaymentStatusSettled           PaymentStatus = "SETTLED"
)

type PaymentRequest struct {
	IdempotencyKey string
	PayerAccountID string
	PayeeAccountID string
	Amount         Money
	Currency       Currency
	Reference      string
}

type Payment struct {
	ID                string
	IdempotencyKey    string
	PayerAccountID    string
	PayeeAccountID    string
	Amount            Money
	Currency          Currency
	Status            PaymentStatus
	// RefundedAmount 已累计退款金额；可退余额 = Amount - RefundedAmount。
	RefundedAmount    Money
	Reference         string
	JournalID         string
	SettlementBatchID string
	CreatedAt         time.Time
	SettledAt         *time.Time
}

// PaymentFingerprint 是幂等键之外请求内容的指纹。
// 同一幂等键再次到达时，指纹必须一致，否则判定为冲突提交。
type PaymentFingerprint struct {
	PayerAccountID string
	PayeeAccountID string
	Amount         Money
	Currency       Currency
	Reference      string
}

func FingerprintOf(req *PaymentRequest) PaymentFingerprint {
	return PaymentFingerprint{
		PayerAccountID: req.PayerAccountID,
		PayeeAccountID: req.PayeeAccountID,
		Amount:         req.Amount,
		Currency:       req.Currency,
		Reference:      req.Reference,
	}
}
