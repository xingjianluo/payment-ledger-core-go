// Package reconciliation 实现本地账与外部账单的对账与差异调整。
package reconciliation

import (
	"fmt"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

// Report 对账报告。
type Report struct {
	Key      string
	Diffs    []domain.ReconciliationDiff
	Adjusted bool // 本次是否实际生成了调整交易
}

// Service 对账服务。
type Service struct {
	db     *storage.DB
	ledger *ledger.Service
}

// New 创建对账服务。
func New(db *storage.DB, l *ledger.Service) *Service {
	return &Service{db: db, ledger: l}
}

// Reconcile 对一批已结算交易与外部记录进行核对。
//
// 差异类型：
//   - MISSING_LOCAL ：外部有、本地无；
//   - MISSING_REMOTE：本地有、外部无（单边缺失）；
//   - DUPLICATE     ：同一交易号在外部侧出现多次，或本地侧传入多次；
//   - AMOUNT_MISMATCH：金额或币种不一致。
//
// 对金额不符生成一笔即时 SETTLED 的 ADJUSTMENT 复式交易（商户账户 vs
// 挂账账户 suspenseAcct），全部调整在单个 Atomic 临界区内完成，余额不足则
// 记为差异但不强行入账。只对"本地存在且金额不符"的差异做自动调整。
//
// 幂等：同一 key 重复执行直接重放首次报告；任一本地交易已在历史对账中
// 覆盖则拒绝，杜绝重复调整。
func (s *Service) Reconcile(key string, localTxnIDs []string, remote []domain.ExternalRecord, suspenseAcct string) (*Report, error) {
	if key == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "reconciliation key required")
	}
	if suspenseAcct == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "suspense account required")
	}

	report := &Report{Key: key}

	err := s.ledger.Atomic(func(tx *ledger.Tx) error {
		// 同一对账键：重放首次结果，不重复调整。
		if run, ok := s.db.GetReconRun(key); ok {
			report.Diffs = append([]domain.ReconciliationDiff(nil), run.Diffs...)
			report.Adjusted = false
			return nil
		}

		// 本地侧建索引，同时检测本地重复传入。
		localByID := make(map[string]*domain.Transaction, len(localTxnIDs))
		diffs := []domain.ReconciliationDiff{}
		matched := map[string]bool{}

		for _, id := range localTxnIDs {
			if _, dup := localByID[id]; dup {
				diffs = append(diffs, domain.ReconciliationDiff{
					Type:  domain.DiffDuplicate,
					TxnID: id,
					Note:  "duplicate local txn in reconciliation request",
				})
				continue
			}
			if priorKey, done := s.db.TxnReconciled(id); done {
				return apperr.New(apperr.CodeBatchSettled,
					"txn %s already reconciled in run %s", id, priorKey)
			}
			t, err := tx.GetTxn(id)
			if err != nil {
				return err
			}
			if t.Status != domain.StatusSettled {
				return apperr.New(apperr.CodeSettlementPending,
					"txn %s not settled (status=%s)", id, t.Status)
			}
			localByID[id] = t
		}

		// 外部侧建索引并检测重复。
		remoteByID := make(map[string]*domain.ExternalRecord, len(remote))
		remoteSeen := map[string]int{}
		for i := range remote {
			r := remote[i]
			remoteSeen[r.TxnID]++
			if remoteSeen[r.TxnID] == 2 {
				diffs = append(diffs, domain.ReconciliationDiff{
					Type:   domain.DiffDuplicate,
					TxnID:  r.TxnID,
					Remote: extPtr(r),
					Note:   "duplicate remote record",
				})
				continue
			}
			if remoteSeen[r.TxnID] > 2 {
				continue
			}
			rcp := r
			remoteByID[r.TxnID] = &rcp
		}

		adjustmentNo := 0
		adjusted := false

		// 本地有 vs 外部。
		for id, lt := range localByID {
			localAmt := txnAmount(lt)
			rec := extFromTxn(lt, localAmt)
			rt, ok := remoteByID[id]
			if !ok {
				diffs = append(diffs, domain.ReconciliationDiff{
					Type:  domain.DiffMissingRemote,
					TxnID: id,
					Local: rec,
					Note:  "present locally, absent on remote statement",
				})
				continue
			}
			matched[id] = true
			if rt.Currency != lt.Currency || rt.Amount != localAmt {
				diff := domain.ReconciliationDiff{
					Type:   domain.DiffAmount,
					TxnID:  id,
					Local:  rec,
					Remote: extPtr(*rt),
					Note: fmt.Sprintf("local=%d %s remote=%d %s",
						localAmt, lt.Currency, rt.Amount, rt.Currency),
				}
				// 仅币种一致时自动调整金额差；币种不一致需人工处理。
				if rt.Currency == lt.Currency {
					if ok, note := s.postAmountAdjustment(tx, key, &adjustmentNo, lt, localAmt, rt.Amount, suspenseAcct); ok {
						adjusted = true
						diff.Note += "; " + note
					} else {
						diff.Note += "; adjustment skipped: " + note
					}
				} else {
					diff.Note += "; adjustment skipped: currency mismatch"
				}
				diffs = append(diffs, diff)
			}
		}

		// 外部有、本地无。
		for id, rt := range remoteByID {
			if _, present := localByID[id]; present {
				continue
			}
			diffs = append(diffs, domain.ReconciliationDiff{
				Type:   domain.DiffMissingLocal,
				TxnID:  id,
				Remote: extPtr(*rt),
				Note:   "present on remote statement, absent locally",
			})
		}

		// 固定差异顺序，保证报告稳定（按 TxnID + Type）。
		sortDiffs(diffs)

		covered := append([]string(nil), localTxnIDs...)
		s.db.PutReconRun(&storage.ReconRunRecord{
			Key:      key,
			Diffs:    diffs,
			Adjusted: adjusted,
			TxnIDs:   covered,
		})
		report.Diffs = diffs
		report.Adjusted = adjusted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// postAmountAdjustment 按"以外部为准"把本地金额补齐/冲减到外部金额。
// 返回 (是否入账, 说明)。余额不足导致不能入账时返回 false。
func (s *Service) postAmountAdjustment(tx *ledger.Tx, key string, no *int, lt *domain.Transaction, localAmt, remoteAmt int64, suspenseAcct string) (bool, string) {
	delta := remoteAmt - localAmt
	if delta == 0 {
		return false, "no delta"
	}
	merchant := creditAccount(lt)
	if merchant == "" {
		return false, "no credit account"
	}
	*no++
	adjID := fmt.Sprintf("ADJ-%s-%s", key, lt.ID)
	if *no > 1 {
		adjID = fmt.Sprintf("ADJ-%s-%s-%d", key, lt.ID, *no)
	}

	var entries []domain.Entry
	if delta > 0 {
		// 本地少记 -> 补给商户：借挂账、贷商户。
		entries = []domain.Entry{
			{AccountID: suspenseAcct, Side: domain.Debit, Amount: delta},
			{AccountID: merchant, Side: domain.Credit, Amount: delta},
		}
	} else {
		// 本地多记 -> 冲减商户：借商户、贷挂账。
		entries = []domain.Entry{
			{AccountID: merchant, Side: domain.Debit, Amount: -delta},
			{AccountID: suspenseAcct, Side: domain.Credit, Amount: -delta},
		}
	}
	if _, err := tx.PostStatus(adjID, domain.TxnAdjustment, domain.StatusSettled, lt.Currency, lt.ID, entries); err != nil {
		// 余额不足等：保留差异、不调整。
		return false, err.Error()
	}
	return true, fmt.Sprintf("adjustment %s posted delta=%d", adjID, delta)
}

// txnAmount 取交易贷记总额。
func txnAmount(t *domain.Transaction) int64 {
	var sum int64
	for _, e := range t.Entries {
		if e.Side == domain.Credit {
			sum += e.Amount
		}
	}
	return sum
}

// creditAccount 取交易收款（贷记）账户。
func creditAccount(t *domain.Transaction) string {
	for _, e := range t.Entries {
		if e.Side == domain.Credit {
			return e.AccountID
		}
	}
	return ""
}

func extFromTxn(t *domain.Transaction, amt int64) *domain.ExternalRecord {
	return &domain.ExternalRecord{TxnID: t.ID, Currency: t.Currency, Amount: amt}
}

func extPtr(r domain.ExternalRecord) *domain.ExternalRecord {
	return &r
}
