// Package storage 提供基于内存的本地存储，单锁保证事务原子性。
package storage

import (
	"sync"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
)

// DB 内存存储。所有可变状态都在同一把互斥锁下访问，
// 从而让"扣款 + 入账 + 交易落库"等复合操作保持原子。
type DB struct {
	mu sync.Mutex

	accounts    map[string]*domain.Account
	txns        map[string]*domain.Transaction
	txnsByRef   map[string][]string
	idem        map[string]*domain.IdempotencyRecord
	batches     map[string]*BatchRecord
	settledTxns map[string]string // txnID -> batchID
	reconRuns   map[string]*ReconRunRecord
	feeAccounts map[string]string // currency -> fee account id
}

// BatchRecord 结算批次记录。
type BatchRecord struct {
	ID        string
	TxnIDs    []string
	Status    string
	FeeTotals map[string]int64 // 按币种汇总的手续费
}

// ReconRunRecord 对账执行记录（用于对账幂等）。
type ReconRunRecord struct {
	Key      string
	Diffs    []domain.ReconciliationDiff
	Adjusted bool
	TxnIDs   []string
}

// NewDB 创建空存储。
func NewDB() *DB {
	return &DB{
		accounts:    map[string]*domain.Account{},
		txns:        map[string]*domain.Transaction{},
		txnsByRef:   map[string][]string{},
		idem:        map[string]*domain.IdempotencyRecord{},
		batches:     map[string]*BatchRecord{},
		settledTxns: map[string]string{},
		reconRuns:   map[string]*ReconRunRecord{},
		feeAccounts: map[string]string{},
	}
}

// Lock / Unlock 供上层复合操作在同一临界区内完成。
func (db *DB) Lock()   { db.mu.Lock() }
func (db *DB) Unlock() { db.mu.Unlock() }

// ---- accounts ----

// CreateAccount 存入新账户，账户 ID 重复返回 false。
func (db *DB) CreateAccount(a *domain.Account) bool {
	if _, ok := db.accounts[a.ID]; ok {
		return false
	}
	cp := *a
	db.accounts[a.ID] = &cp
	return true
}

// GetAccount 返回账户指针（调用方须持锁）。
func (db *DB) GetAccount(id string) (*domain.Account, bool) {
	a, ok := db.accounts[id]
	return a, ok
}

// ---- transactions ----

// PutTxn 落库一笔交易，交易 ID 重复返回 false。
func (db *DB) PutTxn(t *domain.Transaction) bool {
	if _, ok := db.txns[t.ID]; ok {
		return false
	}
	cp := *t
	if cp.Entries != nil {
		cp.Entries = append([]domain.Entry(nil), t.Entries...)
	}
	db.txns[t.ID] = &cp
	if t.RefID != "" {
		db.txnsByRef[t.RefID] = append(db.txnsByRef[t.RefID], t.ID)
	}
	return true
}

// GetTxn 读取交易（返回存储指针的副本，调用方须持锁）。
func (db *DB) GetTxn(id string) (*domain.Transaction, bool) {
	t, ok := db.txns[id]
	return t, ok
}

// ListTxnsByRef 按关联 ID 列出交易。
func (db *DB) ListTxnsByRef(ref string) []*domain.Transaction {
	ids := db.txnsByRef[ref]
	out := make([]*domain.Transaction, 0, len(ids))
	for _, id := range ids {
		out = append(out, db.txns[id])
	}
	return out
}

// ListPendingTxns 列出全部待结算交易（稳定顺序）。
func (db *DB) ListPendingTxns() []*domain.Transaction {
	out := make([]*domain.Transaction, 0)
	for _, t := range db.txns {
		if t.Status == domain.StatusPending {
			out = append(out, t)
		}
	}
	return out
}

// ---- idempotency ----

func (db *DB) GetIdem(key string) *domain.IdempotencyRecord {
	return db.idem[key]
}

func (db *DB) PutIdem(r *domain.IdempotencyRecord) {
	cp := *r
	db.idem[r.Key] = &cp
}

// ---- settlement ----

func (db *DB) PutBatch(b *BatchRecord) {
	cp := *b
	cp.TxnIDs = append([]string(nil), b.TxnIDs...)
	if b.FeeTotals != nil {
		cp.FeeTotals = make(map[string]int64, len(b.FeeTotals))
		for k, v := range b.FeeTotals {
			cp.FeeTotals[k] = v
		}
	}
	db.batches[b.ID] = &cp
}

func (db *DB) GetBatch(id string) (*BatchRecord, bool) {
	b, ok := db.batches[id]
	return b, ok
}

func (db *DB) MarkSettled(txnID, batchID string) {
	db.settledTxns[txnID] = batchID
}

func (db *DB) LookupSettled(txnID string) (string, bool) {
	b, ok := db.settledTxns[txnID]
	return b, ok
}

func (db *DB) SetFeeAccount(currency, accountID string) {
	db.feeAccounts[currency] = accountID
}

func (db *DB) FeeAccount(currency string) (string, bool) {
	id, ok := db.feeAccounts[currency]
	return id, ok
}

// ---- reconciliation ----

func (db *DB) PutReconRun(r *ReconRunRecord) {
	cp := *r
	cp.Diffs = append([]domain.ReconciliationDiff(nil), r.Diffs...)
	cp.TxnIDs = append([]string(nil), r.TxnIDs...)
	db.reconRuns[r.Key] = &cp
}

func (db *DB) GetReconRun(key string) (*ReconRunRecord, bool) {
	r, ok := db.reconRuns[key]
	return r, ok
}

// TxnReconciled 返回该交易是否已在某次对账中被覆盖（避免跨批次重复调整）。
func (db *DB) TxnReconciled(txnID string) (string, bool) {
	for _, r := range db.reconRuns {
		for _, id := range r.TxnIDs {
			if id == txnID {
				return r.Key, true
			}
		}
	}
	return "", false
}
