package domain

import "time"

type DifferenceType string

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
	BatchID        string
	Currency       Currency
	InternalCount  int
	StatementCount int
	MatchedCount   int
	// RefundCount 本批次支付关联的退款记录数（对账单中的退款记录按退款号识别）。
	RefundCount int
	Differences []ReconciliationDifference
	RanAt       time.Time
}
