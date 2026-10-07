package domain

import "time"

type IdempotencyStatus string

const (
	IdempotencyInFlight IdempotencyStatus = "IN_FLIGHT"
	IdempotencyDone     IdempotencyStatus = "DONE"
	// IdempotencyFailed 记录一次"说得清原因"的业务失败：
	// 同键同内容重试必须返回同样的失败结论，而不是含糊的冲突错误。
	IdempotencyFailed IdempotencyStatus = "FAILED"
)

// 幂等记录所属业务种类；不同种类的键不能混用。
const (
	IdempotencyKindPayment = "PAYMENT"
	IdempotencyKindRefund  = "REFUND"
)

type IdempotencyRecord struct {
	Key               string
	Kind              string
	Fingerprint       PaymentFingerprint
	RefundFingerprint RefundFingerprint
	Status            IdempotencyStatus
	ResultID          string
	FailureCode       ErrorCode
	FailureMessage    string
	CreatedAt         time.Time
	ResolvedAt        *time.Time
}
