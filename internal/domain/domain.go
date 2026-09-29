// Package domain 定义账本核心领域模型与状态枚举。
package domain

import "time"

// Money 以币种 + 最小币种单位整数表示金额，避免浮点误差。
type Money struct {
	Currency string
	Amount   int64
}

// TxnType 交易类型。
type TxnType string

const (
	TxnPayment    TxnType = "PAYMENT"    // 支付受理（入账）
	TxnTransfer   TxnType = "TRANSFER"   // 账户间划转
	TxnSettle     TxnType = "SETTLEMENT" // 结算（待结算 -> 已结算）
	TxnFee        TxnType = "FEE"        // 手续费
	TxnAdjustment TxnType = "ADJUSTMENT" // 对账调整
)

// TxnStatus 交易状态。
type TxnStatus string

const (
	StatusPending   TxnStatus = "PENDING"  // 已受理、待结算
	StatusSettled   TxnStatus = "SETTLED"  // 已结算
	StatusAdjusted  TxnStatus = "ADJUSTED" // 已对账调整
	StatusFailed    TxnStatus = "FAILED"
	StatusCancelled TxnStatus = "CANCELLED"
)

// Account 账户。
type Account struct {
	ID       string
	Currency string
	Balance  int64 // 最小币种单位，恒 >= 0
	Version  int64 // 乐观版本号
}

// Entry 单式分录方向。
type EntrySide string

const (
	Debit  EntrySide = "DEBIT"
	Credit EntrySide = "CREDIT"
)

// Entry 一笔交易的一条分录。
type Entry struct {
	AccountID string
	Side      EntrySide
	Amount    int64
}

// Transaction 一笔复式交易。
type Transaction struct {
	ID        string
	Type      TxnType
	Currency  string
	Status    TxnStatus
	Entries   []Entry
	RefID     string // 关联交易/批次号
	IdemKey   string // 幂等键（如有）
	CreatedAt time.Time
	SettledAt time.Time
}

// IdemState 幂等记录状态。
type IdemState string

const (
	IdemInFlight IdemState = "IN_FLIGHT"
	IdemDone     IdemState = "DONE"
	IdemFailed   IdemState = "FAILED"
)

// IdempotencyRecord 幂等键登记记录。
type IdempotencyRecord struct {
	Key         string
	RequestHash string
	State       IdemState
	TxnID       string
	ErrCode     string
	CreatedAt   time.Time
}

// FeeRule 手续费规则：按固定金额或按基点（1bp=0.01%）收取。
type FeeRule struct {
	FeeFixed int64
	FeeBps   int64
}

// ReconDiffType 对账差异类型。
type ReconDiffType string

const (
	DiffMissingLocal  ReconDiffType = "MISSING_LOCAL"  // 外部有、本地无
	DiffMissingRemote ReconDiffType = "MISSING_REMOTE" // 本地有、外部无（单边缺失）
	DiffAmount        ReconDiffType = "AMOUNT_MISMATCH"
	DiffDuplicate     ReconDiffType = "DUPLICATE" // 重复记录
)

// ExternalRecord 模拟外部渠道/对账单中的一条记录。
type ExternalRecord struct {
	TxnID    string
	Currency string
	Amount   int64
}

// ReconciliationDiff 一条对账差异。
type ReconciliationDiff struct {
	Type   ReconDiffType
	TxnID  string
	Local  *ExternalRecord
	Remote *ExternalRecord
	Note   string
}
