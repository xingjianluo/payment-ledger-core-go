package domain

import "time"

type RefundStatus string

const (
	// RefundStatusRefunded 退款成功（终态）。退款失败不落任何记录，仅返回错误。
	RefundStatusRefunded RefundStatus = "REFUNDED"
)

// RefundRequest 退款请求。Amount 为本次退款金额（最小单位），
// 可小于原支付金额（部分退款）；同一支付可多次退款，累计不得超过原支付金额。
// Currency 可选：非空时用于与原支付币种核对，不一致明确拒绝。
type RefundRequest struct {
	IdempotencyKey string
	PaymentID      string
	Amount         Money
	Currency       Currency
	Reason         string
}

type Refund struct {
	ID             string
	IdempotencyKey string
	PaymentID      string
	PayerAccountID string // 退款入账方（原支付付款人）
	PayeeAccountID string // 退款出账方（原支付收款商户）
	Amount         Money
	Currency       Currency
	Status         RefundStatus
	Reason         string
	JournalID      string
	CreatedAt      time.Time
}

// RefundFingerprint 是退款幂等键之外请求内容的指纹。
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
