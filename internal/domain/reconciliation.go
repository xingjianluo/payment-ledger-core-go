package domain

import "time"

type DifferenceType string

// SubjectType 标识差异/记录针对的业务对象：支付或退款。
type SubjectType string

const (
	SubjectPayment SubjectType = "PAYMENT"
	SubjectRefund  SubjectType = "REFUND"
)

const (
	DiffMissingStatement DifferenceType = "MISSING_IN_STATEMENT" // 内部有、对账单没有
	DiffMissingInternal  DifferenceType = "MISSING_IN_INTERNAL"  // 对账单有、内部没有
	DiffAmountMismatch   DifferenceType = "AMOUNT_MISMATCH"
	DiffDuplicate        DifferenceType = "DUPLICATE_RECORD"
)

type StatementRecord struct {
	ExternalTxnID string
	TxnID         string
	Amount        Money
	Currency      Currency
	OccurredAt    time.Time
}

type ReconciliationDifference struct {
	Type            DifferenceType
	Subject         SubjectType // PAYMENT / REFUND
	TxnID           string
	ExternalTxnID   string
	InternalAmount  Money
	StatementAmount Money
	Currency        Currency
	AutoAdjusted    bool
	JournalID       string
	Reason          string
}

type ReconciliationRun struct {
	BatchID             string
	Currency            Currency
	InternalCount       int // 内部支付笔数
	InternalRefundCount int // 内部退款笔数
	StatementCount      int
	MatchedCount        int
	Differences         []ReconciliationDifference
	RanAt               time.Time
}
