package storage

import (
	"fmt"
	"sort"
	"sync"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
)

// tables 是全部数据表与序号发生器的集合。
// Store 与 Tx 共用同一结构：Update 时先对 tables 做快照，
// fn 在快照上读写，成功才把快照整体换回 Store，失败直接丢弃，
// 从而实现"一笔变更要么整体成功要么整体失败"的原子性（相当于数据库事务）。
type tables struct {
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

// clone 复制全部 map（浅拷贝）。所有读写方法都按"存入即拷贝、取出即拷贝"
// 的约定保证表内对象不被原地修改，因此浅拷贝快照即可隔离事务内变更。
func (t *tables) clone() tables {
	cp := tables{seq: t.seq}
	cp.accounts = cloneMap(t.accounts)
	cp.journalByID = cloneMap(t.journalByID)
	cp.journalByBiz = cloneMap(t.journalByBiz)
	cp.paymentsByID = cloneMap(t.paymentsByID)
	cp.paymentsByIDem = cloneMap(t.paymentsByIDem)
	cp.refundsByID = cloneMap(t.refundsByID)
	cp.refundsByIDem = cloneMap(t.refundsByIDem)
	cp.idempotency = cloneMap(t.idempotency)
	cp.batches = cloneMap(t.batches)
	cp.reconByBatch = cloneMap(t.reconByBatch)
	return cp
}

func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Store 是内存存储；所有变更都应通过 Update 在同一个互斥临界区内完成。
type Store struct {
	mu sync.Mutex
	tables
}

func New() *Store {
	return &Store{tables: tables{
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
	}}
}

// Update 在一个原子临界区内执行变更；fn 返回错误则本次所有改动被丢弃（整体回滚）。
func (s *Store) Update(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &Tx{tables: s.tables.clone()}
	if err := fn(tx); err != nil {
		return err
	}
	s.tables = tx.tables
	return nil
}

// View 在一个读临界区内执行查询。
func (s *Store) View(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&Tx{tables: s.tables})
}

// Tx 是事务句柄：Update 时持有数据快照，View 时持有当前表的只读视图；
// 无自带锁，必须在 Update/View 中使用。
type Tx struct{ tables }

// NextID 返回单调递增的带前缀 ID；必须在 Update 临界区内调用。
func (tx *Tx) NextID(prefix string) string {
	tx.seq++
	return fmt.Sprintf("%s-%010d", prefix, tx.seq)
}

// ---- accounts ----

func (tx *Tx) SaveAccount(a *domain.Account) error {
	cp := *a
	tx.accounts[a.ID] = &cp
	return nil
}

func (tx *Tx) GetAccount(id string) (*domain.Account, error) {
	a, ok := tx.accounts[id]
	if !ok {
		return nil, domain.NewError(domain.ErrAccountNotFound, "账户不存在: %s", id)
	}
	cp := *a
	return &cp, nil
}

func (tx *Tx) ListAccounts() ([]*domain.Account, error) {
	out := make([]*domain.Account, 0, len(tx.accounts))
	for _, a := range tx.accounts {
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
	tx.journalByID[j.ID] = &cp
	tx.journalByBiz[bizKey(j.BizType, j.BizID)] = &cp
	return nil
}

func (tx *Tx) GetJournal(id string) (*domain.Journal, error) {
	j, ok := tx.journalByID[id]
	if !ok {
		return nil, domain.NewError(domain.ErrInvalid, "凭证不存在: %s", id)
	}
	cp := *j
	cp.Lines = append([]domain.JournalLine(nil), j.Lines...)
	return &cp, nil
}

// GetJournalByBiz 按业务键查找已过账凭证（用于业务级幂等）。
func (tx *Tx) GetJournalByBiz(bizType, bizID string) (*domain.Journal, bool) {
	j, ok := tx.journalByBiz[bizKey(bizType, bizID)]
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
	tx.paymentsByID[p.ID] = &cp
	if p.IdempotencyKey != "" {
		tx.paymentsByIDem[p.IdempotencyKey] = &cp
	}
	return nil
}

func (tx *Tx) GetPayment(id string) (*domain.Payment, error) {
	p, ok := tx.paymentsByID[id]
	if !ok {
		return nil, domain.NewError(domain.ErrPaymentNotFound, "支付不存在: %s", id)
	}
	cp := *p
	return &cp, nil
}

func (tx *Tx) GetPaymentByIdempotencyKey(key string) (*domain.Payment, bool) {
	p, ok := tx.paymentsByIDem[key]
	if !ok {
		return nil, false
	}
	cp := *p
	return &cp, true
}

func (tx *Tx) ListPaymentsByIDs(ids []string) ([]*domain.Payment, error) {
	out := make([]*domain.Payment, 0, len(ids))
	for _, id := range ids {
		p, ok := tx.paymentsByID[id]
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
	for _, p := range tx.paymentsByID {
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
	tx.refundsByID[r.ID] = &cp
	if r.IdempotencyKey != "" {
		tx.refundsByIDem[r.IdempotencyKey] = &cp
	}
	return nil
}

func (tx *Tx) GetRefund(id string) (*domain.Refund, error) {
	r, ok := tx.refundsByID[id]
	if !ok {
		return nil, domain.NewError(domain.ErrRefundNotFound, "退款不存在: %s", id)
	}
	cp := *r
	return &cp, nil
}

// ListRefundsByPayment 列出一笔支付的全部退款（按退款号排序，保证口径稳定）。
func (tx *Tx) ListRefundsByPayment(paymentID string) ([]*domain.Refund, error) {
	var out []*domain.Refund
	for _, r := range tx.refundsByID {
		if r.PaymentID == paymentID {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListRefundsByPayments 列出多笔支付的全部退款（按退款号排序）。
func (tx *Tx) ListRefundsByPayments(paymentIDs []string) ([]*domain.Refund, error) {
	set := make(map[string]bool, len(paymentIDs))
	for _, id := range paymentIDs {
		set[id] = true
	}
	var out []*domain.Refund
	for _, r := range tx.refundsByID {
		if set[r.PaymentID] {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- idempotency ----

func (tx *Tx) SaveIdempotency(r *domain.IdempotencyRecord) error {
	cp := *r
	tx.idempotency[r.Key] = &cp
	return nil
}

func (tx *Tx) GetIdempotency(key string) (*domain.IdempotencyRecord, error) {
	r, ok := tx.idempotency[key]
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
	tx.batches[b.ID] = &cp
	return nil
}

func (tx *Tx) GetBatch(id string) (*domain.SettlementBatch, error) {
	b, ok := tx.batches[id]
	if !ok {
		return nil, domain.NewError(domain.ErrBatchNotFound, "结算批次不存在: %s", id)
	}
	cp := *b
	cp.PaymentIDs = append([]string(nil), b.PaymentIDs...)
	return &cp, nil
}

func (tx *Tx) ListBatches() ([]*domain.SettlementBatch, error) {
	out := make([]*domain.SettlementBatch, 0, len(tx.batches))
	for _, b := range tx.batches {
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
	tx.reconByBatch[r.BatchID] = &cp
	return nil
}

func (tx *Tx) GetReconRun(batchID string) (*domain.ReconciliationRun, error) {
	r, ok := tx.reconByBatch[batchID]
	if !ok {
		return nil, nil
	}
	cp := *r
	cp.Differences = append([]domain.ReconciliationDifference(nil), r.Differences...)
	return &cp, nil
}
