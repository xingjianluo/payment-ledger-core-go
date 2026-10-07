package domain

import "time"

type IdempotencyStatus string

const (
	IdempotencyInFlight IdempotencyStatus = "IN_FLIGHT"
	IdempotencyDone     IdempotencyStatus = "DONE"
)

// Fingerprint 是幂等键之外请求内容的指纹（按业务类型区分）。
// 同一幂等键再次到达时，指纹必须一致，否则判定为冲突提交。
// 全值类型，可直接用 == 比较。
type Fingerprint struct {
	Kind    string // "PAYMENT" / "REFUND"
	Payment PaymentFingerprint
	Refund  RefundFingerprint
}

const (
	FingerprintKindPayment = "PAYMENT"
	FingerprintKindRefund  = "REFUND"
)

type IdempotencyRecord struct {
	Key         string
	Fingerprint Fingerprint
	Status      IdempotencyStatus
	ResultID    string
	CreatedAt   time.Time
	ResolvedAt  *time.Time
}
