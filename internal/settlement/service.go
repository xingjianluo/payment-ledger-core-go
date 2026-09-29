// Package settlement 实现按批次结算与手续费扣收。
package settlement

import (
	"fmt"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

// BatchResult 批次结算结果。
type BatchResult struct {
	BatchID   string
	TxnIDs    []string
	FeeTotals map[string]int64 // 按币种汇总的手续费
}

// Service 结算服务。
type Service struct {
	db     *storage.DB
	ledger *ledger.Service
}

// New 创建结算服务。
func New(db *storage.DB, l *ledger.Service) *Service {
	return &Service{db: db, ledger: l}
}

// calcFee 按 FeeRule 计算单笔手续费：固定金额 + floor(金额*bps/10000)，1bp=0.01%。
func calcFee(amount int64, rule domain.FeeRule) int64 {
	return rule.FeeFixed + (amount*rule.FeeBps)/10000
}

// SettleBatch 将指定交易批次结算：
//   - 逐笔校验 PENDING -> SETTLED；
//   - 按每笔币种适用的 FeeRule 扣收手续费（借商户、贷手续费账户）；
//   - 在单个 Atomic 临界区内"先完整校验、后统一提交"，
//     任一笔失败则整批不变，绝不出现只结算部分交易；
//   - 同一批次重复执行返回 CodeBatchSettled，不产生重复手续费；
//   - 同一交易已在别的批次结算也返回 CodeBatchSettled。
func (s *Service) SettleBatch(batchID string, txnIDs []string, rules map[string]domain.FeeRule, at time.Time) (*BatchResult, error) {
	if batchID == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "batch id required")
	}
	if len(txnIDs) == 0 {
		return nil, apperr.New(apperr.CodeInvalidArgument, "empty batch: %s", batchID)
	}

	var result *BatchResult
	err := s.ledger.Atomic(func(tx *ledger.Tx) error {
		if _, ok := s.db.GetBatch(batchID); ok {
			return apperr.New(apperr.CodeBatchSettled, "batch already settled: id=%s", batchID)
		}

		type plan struct {
			txn      *domain.Transaction
			merchant string
			feeAcct  string
			fee      int64
		}
		plans := make([]plan, 0, len(txnIDs))
		feeTotals := map[string]int64{}
		// 同一商户在一批内可能被收多笔费，按账户汇总后统一做余额预演。
		merchantDebit := map[string]int64{}
		seen := map[string]bool{}

		// ---------- 阶段一：只读校验 + 预演，不做任何变更 ----------
		for _, id := range txnIDs {
			if seen[id] {
				return apperr.New(apperr.CodeInvalidArgument, "duplicate txn within batch: %s", id)
			}
			seen[id] = true

			if priorBatch, settled := s.db.LookupSettled(id); settled {
				return apperr.New(apperr.CodeBatchSettled,
					"txn %s already settled in batch %s", id, priorBatch)
			}
			t, err := tx.GetTxn(id)
			if err != nil {
				return err
			}
			if t.Status != domain.StatusPending {
				return apperr.New(apperr.CodeBatchSettled,
					"txn %s not pending (status=%s)", id, t.Status)
			}
			feeAcct, ok := s.db.FeeAccount(t.Currency)
			if !ok {
				return apperr.New(apperr.CodeAccountNotFound,
					"fee account not configured for currency %s (txn=%s)", t.Currency, id)
			}
			merchant := creditAccount(t)
			if merchant == "" {
				return apperr.New(apperr.CodeInvalidArgument, "no credit account on txn %s", id)
			}

			fee := calcFee(txnAmount(t), rules[t.Currency])
			if fee < 0 {
				return apperr.New(apperr.CodeInvalidArgument, "negative fee for txn %s", id)
			}
			merchantDebit[merchant] += fee
			feeTotals[t.Currency] += fee
			plans = append(plans, plan{txn: t, merchant: merchant, feeAcct: feeAcct, fee: fee})
		}

		// 对每个被收费的商户一次性预演余额（同币种才会同账户）。
		for acctID, debit := range merchantDebit {
			if debit == 0 {
				continue
			}
			acc, err := tx.Account(acctID)
			if err != nil {
				return err
			}
			if acc.Balance-debit < 0 {
				return apperr.New(apperr.CodeInsufficientFunds,
					"merchant balance cannot cover fees: acct=%s balance=%d feeDebit=%d batch=%s",
					acctID, acc.Balance, debit, batchID)
			}
		}

		// ---------- 阶段二：全部校验通过，统一提交 ----------
		for i, p := range plans {
			if p.fee > 0 {
				feeTxnID := fmt.Sprintf("FEE-%s-%d", batchID, i)
				entries := []domain.Entry{
					{AccountID: p.merchant, Side: domain.Debit, Amount: p.fee},
					{AccountID: p.feeAcct, Side: domain.Credit, Amount: p.fee},
				}
				if _, err := tx.PostStatus(feeTxnID, domain.TxnFee, domain.StatusSettled, p.txn.Currency, batchID, entries); err != nil {
					return err
				}
			}
			if err := tx.Settle(p.txn.ID, at); err != nil {
				return err
			}
			s.db.MarkSettled(p.txn.ID, batchID)
		}

		s.db.PutBatch(&storage.BatchRecord{
			ID:        batchID,
			TxnIDs:    append([]string(nil), txnIDs...),
			Status:    string(domain.StatusSettled),
			FeeTotals: feeTotals,
		})

		result = &BatchResult{
			BatchID:   batchID,
			TxnIDs:    append([]string(nil), txnIDs...),
			FeeTotals: feeTotals,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// txnAmount 取一笔交易的贷记总额（入账金额）。
func txnAmount(t *domain.Transaction) int64 {
	var sum int64
	for _, e := range t.Entries {
		if e.Side == domain.Credit {
			sum += e.Amount
		}
	}
	return sum
}

// creditAccount 取一笔交易的收款（贷记）账户。
func creditAccount(t *domain.Transaction) string {
	for _, e := range t.Entries {
		if e.Side == domain.Credit {
			return e.AccountID
		}
	}
	return ""
}
