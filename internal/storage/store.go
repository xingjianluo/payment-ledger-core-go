package storage

import (
	"fmt"
	"sync"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
)

// Store 是内存存储；所有变更都应通过 Update 在同一个互斥临界区内完成，
// 以保证"一笔划转要么整体成功要么整体失败"的原子性（相当于数据库事务）。
type Store struct {
	mu  sync.Mutex
	seq uint64

	accounts       map[string]*domain.Account
	journalByID    map[string]*domain.Journal
	journalByBiz   map[string]*domain.Journal
	paymentsByID   map[string]*domain.Payment
	paymentsByIDem map[string]*domain.Payment
	refundsByID    map[string]*domain.Refund
	refundsByIDem  map[string]*domain.Refund
	idempotency    map[string]*domain.IdempotencyRecord
	batches        map[string]*domain.SettlementBatch
	reconByBatch   map[string]*domain.ReconciliationRun
}

func New() *Store {
	return &Store{
		accounts:       map[string]*domain.Account{},
		journalByID:    map[string]*domain.Journal{},
		journalByBiz:   map[string]*domain.Journal{},
		paymentsByID:   map[string]*domain.Payment{},
		paymentsByIDem: map[string]*domain.Payment{},
		refundsByID:    map[string]*domain.Refund{},
		refundsByIDem:  map[string]*domain.Refund{},
		idempotency:    map[string]*domain.IdempotencyRecord{},
		batches:        map[string]*domain.SettlementBatch{},
		reconByBatch:   map[string]*domain.ReconciliationRun{},
	}
}

// Update 在一个原子临界区内执行变更；fn 返回错误则本次所有改动被回滚丢弃。
// 回滚通过对各 map 做浅快照实现：存储层保存的都是"先复制再写入"的不可变对象，
// 没有任何代码原地修改已存对象，因此浅快照足以恢复到事务前状态。
func (s *Store) Update(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snapshot()
	tx := &Tx{s: s}
	if err := fn(tx); err != nil {
		s.restore(snap)
		return err
	}
	return nil
}

type snapshot struct {
	seq            uint64
	accounts       map[string]*domain.Account
	journalByID    map[string]*domain.Journal
	journalByBiz   map[string]*domain.Journal
	paymentsByID   map[string]*domain.Payment
	paymentsByIDem map[string]*domain.Payment
	refundsByID    map[string]*domain.Refund
	refundsByIDem  map[string]*domain.Refund
	idempotency    map[string]*domain.IdempotencyRecord
	batches        map[string]*domain.SettlementBatch
	reconByBatch   map[string]*domain.ReconciliationRun
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Store) snapshot() snapshot {
	return snapshot{
		seq:            s.seq,
		accounts:       cloneMap(s.accounts),
		journalByID:    cloneMap(s.journalByID),
		journalByBiz:   cloneMap(s.journalByBiz),
		paymentsByID:   cloneMap(s.paymentsByID),
		paymentsByIDem: cloneMap(s.paymentsByIDem),
		refundsByID:    cloneMap(s.refundsByID),
		refundsByIDem:  cloneMap(s.refundsByIDem),
		idempotency:    cloneMap(s.idempotency),
		batches:        cloneMap(s.batches),
		reconByBatch:   cloneMap(s.reconByBatch),
	}
}

func (s *Store) restore(snap snapshot) {
	s.seq = snap.seq
	s.accounts = snap.accounts
	s.journalByID = snap.journalByID
	s.journalByBiz = snap.journalByBiz
	s.paymentsByID = snap.paymentsByID
	s.paymentsByIDem = snap.paymentsByIDem
	s.refundsByID = snap.refundsByID
	s.refundsByIDem = snap.refundsByIDem
	s.idempotency = snap.idempotency
	s.batches = snap.batches
	s.reconByBatch = snap.reconByBatch
}

// View 在一个读临界区内执行查询。
func (s *Store) View(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&Tx{s: s})
}

// Tx 是事务句柄：直接操作底层 map，无自带锁，必须在 Update/View 中使用。
type Tx struct{ s *Store }

// NextID 返回单调递增的带前缀 ID；必须在 Update 临界区内调用。
func (tx *Tx) NextID(prefix string) string {
	tx.s.seq++
	return fmt.Sprintf("%s-%010d", prefix, tx.s.seq)
}

// ---- accounts ----

func (tx *Tx) SaveAccount(a *domain.Account) error {
	cp := *a
	tx.s.accounts[a.ID] = &cp
	return nil
}

func (tx *Tx) GetAccount(id string) (*domain.Account, error) {
	a, ok := tx.s.accounts[id]
	if !ok {
		return nil, domain.NewError(domain.ErrAccountNotFound, "账户不存在: %s", id)
	}
	cp := *a
	return &cp, nil
}

func (tx *Tx) ListAccounts() ([]*domain.Account, error) {
	out := make([]*domain.Account, 0, len(tx.s.accounts))
	for _, a := range tx.s.accounts {
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

// ---- journals ----

func bizKey(bizType, bizID string) string { return bizType + "\x00" + bizID }

func (tx *Tx) SaveJournal(j *domain.Journal) error {
	cp := *j
	if len(j.Lines) > 0 {
		cp.Lines = append([]domain.JournalLine(nil), j.Lines...)
	}
	tx.s.journalByID[j.ID] = &cp
	tx.s.journalByBiz[bizKey(j.BizType, j.BizID)] = &cp
	return nil
}

func (tx *Tx) GetJournal(id string) (*domain.Journal, error) {
	j, ok := tx.s.journalByID[id]
	if !ok {
		return nil, domain.NewError(domain.ErrInvalid, "凭证不存在: %s", id)
	}
	cp := *j
	cp.Lines = append([]domain.JournalLine(nil), j.Lines...)
	return &cp, nil
}

// GetJournalByBiz 按业务键查找已过账凭证（用于业务级幂等）。
func (tx *Tx) GetJournalByBiz(bizType, bizID string) (*domain.Journal, bool) {
	j, ok := tx.s.journalByBiz[bizKey(bizType, bizID)]
	if !ok {
		return nil, false
	}
	cp := *j
	cp.Lines = append([]domain.JournalLine(nil), j.Lines...)
	return &cp, true
}

// ---- payments ----

func (tx *Tx) SavePayment(p *domain.Payment) error {
	cp := *p
	tx.s.paymentsByID[p.ID] = &cp
	if p.IdempotencyKey != "" {
		tx.s.paymentsByIDem[p.IdempotencyKey] = &cp
	}
	return nil
}

func (tx *Tx) GetPayment(id string) (*domain.Payment, error) {
	p, ok := tx.s.paymentsByID[id]
	if !ok {
		return nil, domain.NewError(domain.ErrPaymentNotFound, "支付不存在: %s", id)
	}
	cp := *p
	return &cp, nil
}

func (tx *Tx) GetPaymentByIdempotencyKey(key string) (*domain.Payment, bool) {
	p, ok := tx.s.paymentsByIDem[key]
	if !ok {
		return nil, false
	}
	cp := *p
	return &cp, true
}

func (tx *Tx) ListPaymentsByIDs(ids []string) ([]*domain.Payment, error) {
	out := make([]*domain.Payment, 0, len(ids))
	for _, id := range ids {
		p, ok := tx.s.paymentsByID[id]
		if !ok {
			return nil, domain.NewError(domain.ErrPaymentNotFound, "支付不存在: %s", id)
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

// ListPendingPayments 列出指定币种下待结算支付。
func (tx *Tx) ListPendingPayments(c domain.Currency) ([]*domain.Payment, error) {
	var out []*domain.Payment
	for _, p := range tx.s.paymentsByID {
		if p.Status == domain.PaymentStatusPendingSettlement && p.Currency == c {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

// ---- refunds ----

func (tx *Tx) SaveRefund(r *domain.Refund) error {
	cp := *r
	tx.s.refundsByID[r.ID] = &cp
	if r.IdempotencyKey != "" {
		tx.s.refundsByIDem[r.IdempotencyKey] = &cp
	}
	return nil
}

func (tx *Tx) GetRefund(id string) (*domain.Refund, error) {
	r, ok := tx.s.refundsByID[id]
	if !ok {
		return nil, domain.NewError(domain.ErrRefundNotFound, "退款不存在: %s", id)
	}
	cp := *r
	return &cp, nil
}

// ListRefundsByPaymentIDs 列出指定支付集合的全部退款（对账识别退款记录用）。
func (tx *Tx) ListRefundsByPaymentIDs(paymentIDs []string) ([]*domain.Refund, error) {
	set := map[string]bool{}
	for _, id := range paymentIDs {
		set[id] = true
	}
	var out []*domain.Refund
	for _, r := range tx.s.refundsByID {
		if set[r.PaymentID] {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

// ---- idempotency ----
func (tx *Tx) SaveIdempotency(r *domain.IdempotencyRecord) error {
	cp := *r
	tx.s.idempotency[r.Key] = &cp
	return nil
}

func (tx *Tx) GetIdempotency(key string) (*domain.IdempotencyRecord, error) {
	r, ok := tx.s.idempotency[key]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// ---- batches ----

func (tx *Tx) SaveBatch(b *domain.SettlementBatch) error {
	cp := *b
	if len(b.PaymentIDs) > 0 {
		cp.PaymentIDs = append([]string(nil), b.PaymentIDs...)
	}
	tx.s.batches[b.ID] = &cp
	return nil
}

func (tx *Tx) GetBatch(id string) (*domain.SettlementBatch, error) {
	b, ok := tx.s.batches[id]
	if !ok {
		return nil, domain.NewError(domain.ErrBatchNotFound, "结算批次不存在: %s", id)
	}
	cp := *b
	cp.PaymentIDs = append([]string(nil), b.PaymentIDs...)
	return &cp, nil
}

func (tx *Tx) ListBatches() ([]*domain.SettlementBatch, error) {
	out := make([]*domain.SettlementBatch, 0, len(tx.s.batches))
	for _, b := range tx.s.batches {
		cp := *b
		out = append(out, &cp)
	}
	return out, nil
}

// ---- reconciliation ----

func (tx *Tx) SaveReconRun(r *domain.ReconciliationRun) error {
	cp := *r
	if len(r.Differences) > 0 {
		cp.Differences = append([]domain.ReconciliationDifference(nil), r.Differences...)
	}
	tx.s.reconByBatch[r.BatchID] = &cp
	return nil
}

func (tx *Tx) GetReconRun(batchID string) (*domain.ReconciliationRun, error) {
	r, ok := tx.s.reconByBatch[batchID]
	if !ok {
		return nil, nil
	}
	cp := *r
	cp.Differences = append([]domain.ReconciliationDifference(nil), r.Differences...)
	return &cp, nil
}
