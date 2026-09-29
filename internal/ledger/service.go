// Package ledger 实现账户体系与复式记账：每笔交易借贷相等、原子入账。
package ledger

import (
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

// 记账方向约定（资金账户视角）：
//   Debit(借)  -> 账户余额减少（资金流出）
//   Credit(贷) -> 账户余额增加（资金流入）

// Service 账本服务。
type Service struct {
	db *storage.DB
}

// New 创建账本服务。
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// DB 暴露底层存储（上层包在 Atomic 回调中使用）。
func (s *Service) DB() *storage.DB { return s.db }

// Atomic 在单一临界区内执行 fn。fn 返回错误则本次所有存储变更由
// 调用方自行保证不产生半截状态（实现上 fn 内只做"先校验后提交"）。
func (s *Service) Atomic(fn func(tx *Tx) error) error {
	s.db.Lock()
	defer s.db.Unlock()
	t := &Tx{s: s}
	return fn(t)
}

// Tx 是临界区内的账本操作句柄。
type Tx struct {
	s *Service
}

// Post 在锁内原子提交一笔复式交易（待结算状态）。
func (tx *Tx) Post(txnID string, t domain.TxnType, currency, idemKey, refID string, entries []domain.Entry) (*domain.Transaction, error) {
	return tx.s.postLocked(txnID, t, domain.StatusPending, currency, idemKey, refID, entries)
}

// PostStatus 以指定状态原子提交一笔复式交易（用于手续费/调整）。
func (tx *Tx) PostStatus(txnID string, t domain.TxnType, status domain.TxnStatus, currency, refID string, entries []domain.Entry) (*domain.Transaction, error) {
	return tx.s.postLocked(txnID, t, status, currency, "", refID, entries)
}

// GetTxn 在锁内读取交易。
func (tx *Tx) GetTxn(id string) (*domain.Transaction, error) {
	t, ok := tx.s.db.GetTxn(id)
	if !ok {
		return nil, apperr.New(apperr.CodeTxnNotFound, "transaction not found: %s", id)
	}
	return cloneTxn(t), nil
}

// Account 在锁内读取账户快照。
func (tx *Tx) Account(id string) (*domain.Account, error) {
	a, ok := tx.s.db.GetAccount(id)
	if !ok {
		return nil, apperr.New(apperr.CodeAccountNotFound, "account not found: %s", id)
	}
	return cloneAccount(a), nil
}

// Settle 在锁内把待结算交易置为已结算。
func (tx *Tx) Settle(txnID string, at time.Time) error {
	t, ok := tx.s.db.GetTxn(txnID)
	if !ok {
		return apperr.New(apperr.CodeTxnNotFound, "transaction not found: %s", txnID)
	}
	if t.Status != domain.StatusPending {
		return apperr.New(apperr.CodeBatchSettled, "transaction not pending: id=%s status=%s", txnID, t.Status)
	}
	t.Status = domain.StatusSettled
	t.SettledAt = at
	return nil
}

// OpenAccount 开户。
func (s *Service) OpenAccount(id, currency string, openingBalance int64) (*domain.Account, error) {
	if id == "" || currency == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "account id/currency required")
	}
	if openingBalance < 0 {
		return nil, apperr.New(apperr.CodeInvalidArgument, "opening balance must be >= 0")
	}
	s.db.Lock()
	defer s.db.Unlock()
	a := &domain.Account{ID: id, Currency: currency, Balance: openingBalance, Version: 0}
	if !s.db.CreateAccount(a) {
		return nil, apperr.New(apperr.CodeInvalidArgument, "account already exists: %s", id)
	}
	return cloneAccount(a), nil
}

// Account 查询账户（返回快照副本）。
func (s *Service) Account(id string) (*domain.Account, error) {
	s.db.Lock()
	defer s.db.Unlock()
	a, ok := s.db.GetAccount(id)
	if !ok {
		return nil, apperr.New(apperr.CodeAccountNotFound, "account not found: %s", id)
	}
	return cloneAccount(a), nil
}

// Post 在单一临界区内原子提交一笔复式交易（便捷入口）。
func (s *Service) Post(txnID string, t domain.TxnType, currency, idemKey, refID string, entries []domain.Entry) (*domain.Transaction, error) {
	s.db.Lock()
	defer s.db.Unlock()
	return s.postLocked(txnID, t, domain.StatusPending, currency, idemKey, refID, entries)
}

// Settle 将待结算交易置为已结算。
func (s *Service) Settle(txnID string, at time.Time) error {
	s.db.Lock()
	defer s.db.Unlock()
	t, ok := s.db.GetTxn(txnID)
	if !ok {
		return apperr.New(apperr.CodeTxnNotFound, "transaction not found: %s", txnID)
	}
	if t.Status != domain.StatusPending {
		return apperr.New(apperr.CodeBatchSettled, "transaction not pending: id=%s status=%s", txnID, t.Status)
	}
	t.Status = domain.StatusSettled
	t.SettledAt = at
	return nil
}

// postLocked 假定调用方已持有锁。
// 流程：参数校验 -> 交易号去重 -> 账户/币种/金额校验 -> 借贷平衡 ->
// 余额充足性预演 -> 全部通过才提交余额与交易，任一失败则状态完全不变。
func (s *Service) postLocked(txnID string, t domain.TxnType, status domain.TxnStatus, currency, idemKey, refID string, entries []domain.Entry) (*domain.Transaction, error) {
	if txnID == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "txn id required")
	}
	if currency == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "currency required")
	}
	if len(entries) == 0 {
		return nil, apperr.New(apperr.CodeInvalidArgument, "entries required")
	}
	if _, ok := s.db.GetTxn(txnID); ok {
		return nil, apperr.New(apperr.CodeDuplicateTxn, "transaction already posted: %s", txnID)
	}

	// 第一遍：校验并汇总每个账户的净变动。
	deltas := make(map[string]int64)
	var debitSum, creditSum int64
	for _, e := range entries {
		if e.Amount <= 0 {
			return nil, apperr.New(apperr.CodeInvalidAmount, "entry amount must be > 0 (txn=%s acct=%s)", txnID, e.AccountID)
		}
		acc, ok := s.db.GetAccount(e.AccountID)
		if !ok {
			return nil, apperr.New(apperr.CodeAccountNotFound, "account not found: %s (txn=%s)", e.AccountID, txnID)
		}
		if acc.Currency != currency {
			return nil, apperr.New(apperr.CodeCurrencyMismatch,
				"currency mismatch: txn=%s txnCCY=%s acct=%s acctCCY=%s",
				txnID, currency, e.AccountID, acc.Currency)
		}
		switch e.Side {
		case domain.Debit:
			deltas[e.AccountID] -= e.Amount
			debitSum += e.Amount
		case domain.Credit:
			deltas[e.AccountID] += e.Amount
			creditSum += e.Amount
		default:
			return nil, apperr.New(apperr.CodeInvalidArgument, "unknown entry side %q (txn=%s)", e.Side, txnID)
		}
	}
	if debitSum != creditSum {
		return nil, apperr.New(apperr.CodeInvalidArgument,
			"unbalanced entries: debit=%d credit=%d (txn=%s)", debitSum, creditSum, txnID)
	}

	// 第二遍：预演余额，任一账户将为负则整笔拒绝。
	for acctID, delta := range deltas {
		acc, _ := s.db.GetAccount(acctID)
		if acc.Balance+delta < 0 {
			return nil, apperr.New(apperr.CodeInsufficientFunds,
				"insufficient funds: acct=%s balance=%d required=%d (txn=%s)",
				acctID, acc.Balance, -delta, txnID)
		}
	}

	// 提交：更新余额与版本号。
	for acctID, delta := range deltas {
		acc, _ := s.db.GetAccount(acctID)
		acc.Balance += delta
		acc.Version++
	}

	txn := &domain.Transaction{
		ID:        txnID,
		Type:      t,
		Currency:  currency,
		Status:    status,
		Entries:   append([]domain.Entry(nil), entries...),
		RefID:     refID,
		IdemKey:   idemKey,
		CreatedAt: time.Now(),
	}
	if status == domain.StatusSettled {
		txn.SettledAt = time.Now()
	}
	if !s.db.PutTxn(txn) {
		return nil, apperr.New(apperr.CodeDuplicateTxn, "transaction already posted: %s", txnID)
	}
	return cloneTxn(txn), nil
}

func cloneAccount(a *domain.Account) *domain.Account {
	cp := *a
	return &cp
}

func cloneTxn(t *domain.Transaction) *domain.Transaction {
	cp := *t
	cp.Entries = append([]domain.Entry(nil), t.Entries...)
	return &cp
}
