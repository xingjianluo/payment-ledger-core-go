package reconciliation

import (
	"fmt"
	"sort"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type Clock interface{ Now() time.Time }

// StatementSource 外部对账单来源（本地文件或内存构造），不依赖任何网络接口。
type StatementSource interface {
	Records(batchID string, currency domain.Currency) ([]domain.StatementRecord, error)
}

type Service struct {
	store  *storage.Store
	ledger *ledger.Service
	clock  Clock
}

func New(st *storage.Store, l *ledger.Service, clock Clock) *Service {
	if clock == nil {
		clock = ledger.SystemClock()
	}
	return &Service{store: st, ledger: l, clock: clock}
}

// Reconcile 对指定结算批次执行对账。
//
// 识别四类差异：
//   - MISSING_IN_STATEMENT 内部已结算但对账单缺失（不自动调整，挂人工）；
//   - MISSING_IN_INTERNAL  对账单有但内部无账（自动：借 手续费收入冲减/贷 暂记 或反向，
//     实际平台资金与账上不符时挂暂记科目，不直接改客户账）；
//   - AMOUNT_MISMATCH      双方都有但金额不符（自动按差额挂暂记调整）；
//   - DUPLICATE_RECORD     对账单中同 TxnID 重复出现（不自动调整，挂人工）。
//
// 退款记录：对账单中 TxnID 与内部退款号匹配的记录按退款核对（Subject=REFUND），
// 与支付记录一样参与匹配/差异判定，不会被误判为内部缺失的"幽灵"记录；
// 内部有、对账单没有的退款记 MISSING_IN_STATEMENT(REFUND)，挂人工核查。
//
// 幂等：同一批次重复执行直接返回首次结果，绝不产生重复调整凭证。
func (s *Service) Reconcile(batchID string, source StatementSource) (*domain.ReconciliationRun, error) {
	if batchID == "" || source == nil {
		return nil, domain.NewError(domain.ErrInvalid, "批次ID与对账单来源不能为空")
	}

	var out *domain.ReconciliationRun
	err := s.store.Update(func(tx *storage.Tx) error {
		// 批次级幂等：已跑过则原样返回。
		if existing, _ := tx.GetReconRun(batchID); existing != nil {
			out = existing
			return nil
		}

		batch, err := tx.GetBatch(batchID)
		if err != nil {
			return err
		}
		if batch.Status != domain.BatchStatusSettled {
			return domain.NewError(domain.ErrInvalid, "批次 %s 尚未结算，不能对账", batchID)
		}

		records, err := source.Records(batchID, batch.Currency)
		if err != nil {
			return err
		}
		payments, err := tx.ListPaymentsByIDs(batch.PaymentIDs)
		if err != nil {
			return err
		}
		// 退款记录与支付一起纳入对账口径：按退款号与对账单 TxnID 匹配。
		refunds, err := tx.ListRefundsByPayments(batch.PaymentIDs)
		if err != nil {
			return err
		}

		suspenseID, err := s.ledger.EnsureSuspenseInTx(tx, batch.Currency)
		if err != nil {
			return err
		}
		suspenseRecvID, err := s.ledger.EnsureSuspenseReceivableInTx(tx, batch.Currency)
		if err != nil {
			return err
		}

		run := &domain.ReconciliationRun{
			BatchID:             batchID,
			Currency:            batch.Currency,
			InternalCount:       len(payments),
			InternalRefundCount: len(refunds),
			StatementCount:      len(records),
			RanAt:               s.clock.Now(),
		}

		// 对账单按 TxnID 聚合，识别重复记录。
		stmtByTxn := map[string][]domain.StatementRecord{}
		var stmtTxnOrder []string
		for _, r := range records {
			if r.Currency != batch.Currency {
				return domain.NewError(domain.ErrCurrencyMismatch,
					"对账单记录 %s 币种 %s 与批次币种 %s 不一致", r.TxnID, r.Currency, batch.Currency)
			}
			if _, ok := stmtByTxn[r.TxnID]; !ok {
				stmtTxnOrder = append(stmtTxnOrder, r.TxnID)
			}
			stmtByTxn[r.TxnID] = append(stmtByTxn[r.TxnID], r)
		}
		sort.Strings(stmtTxnOrder)

		internal := map[string]*domain.Payment{}
		for _, p := range payments {
			internal[p.ID] = p
		}
		internalRefund := map[string]*domain.Refund{}
		for _, r := range refunds {
			internalRefund[r.ID] = r
		}

		matched := 0
		var diffs []domain.ReconciliationDifference

		// 先扫对账单：处理内部缺失、重复、金额不符（支付与退款记录分别识别）。
		for _, txnID := range stmtTxnOrder {
			recs := stmtByTxn[txnID]
			// 判定该记录的业务对象：支付号命中支付，退款号命中退款，否则按支付类幽灵记录处理。
			subject := domain.SubjectPayment
			if _, ok := internal[txnID]; !ok {
				if _, ok := internalRefund[txnID]; ok {
					subject = domain.SubjectRefund
				}
			}
			// 第一条用于与内部账匹配；多出的条目记为重复（不自动调整）。
			if len(recs) > 1 {
				sort.SliceStable(recs, func(i, j int) bool { return recs[i].Amount < recs[j].Amount })
				for i := 1; i < len(recs); i++ {
					r := recs[i]
					diffs = append(diffs, domain.ReconciliationDifference{
						Type:            domain.DiffDuplicate,
						Subject:         subject,
						TxnID:           txnID,
						ExternalTxnID:   r.ExternalTxnID,
						InternalAmount:  0,
						StatementAmount: r.Amount,
						Currency:        batch.Currency,
						Reason:          fmt.Sprintf("对账单中交易号 %s 出现第 %d 次，疑似重复入账", txnID, i+1),
					})
				}
			}
			r := recs[0]
			if subject == domain.SubjectRefund {
				rf := internalRefund[txnID]
				if rf.Amount != r.Amount {
					delta := r.Amount.Sub(rf.Amount)
					j, err := s.postMismatchAdjustment(tx, txnID, rf.Amount, r.Amount, suspenseID, suspenseRecvID, batch.Currency)
					if err != nil {
						return err
					}
					diffs = append(diffs, domain.ReconciliationDifference{
						Type:            domain.DiffAmountMismatch,
						Subject:         domain.SubjectRefund,
						TxnID:           txnID,
						ExternalTxnID:   r.ExternalTxnID,
						InternalAmount:  rf.Amount,
						StatementAmount: r.Amount,
						Currency:        batch.Currency,
						AutoAdjusted:    true,
						JournalID:       j.ID,
						Reason: fmt.Sprintf("退款 %s 金额不符：内部 %d，对账单 %d，差额 %d，已挂暂记调整",
							txnID, rf.Amount, r.Amount, delta),
					})
					continue
				}
				matched++
				continue
			}
			p, ok := internal[txnID]
			if !ok {
				// 对账单有、内部无：按对账单金额挂暂记应付调整。
				j, err := s.postMissingInternalAdjustment(tx, txnID, r.Amount, suspenseID, batch.Currency)
				if err != nil {
					return err
				}
				diffs = append(diffs, domain.ReconciliationDifference{
					Type:            domain.DiffMissingInternal,
					Subject:         domain.SubjectPayment,
					TxnID:           txnID,
					ExternalTxnID:   r.ExternalTxnID,
					StatementAmount: r.Amount,
					Currency:        batch.Currency,
					AutoAdjusted:    true,
					JournalID:       j.ID,
					Reason: fmt.Sprintf("对账单存在交易 %s(%s) 金额 %d，内部账簿缺失，已挂暂记调整",
						txnID, r.ExternalTxnID, r.Amount),
				})
				continue
			}
			if p.Amount != r.Amount {
				delta := r.Amount.Sub(p.Amount) // 正：对账单多；负：对账单少
				j, err := s.postMismatchAdjustment(tx, txnID, p.Amount, r.Amount, suspenseID, suspenseRecvID, batch.Currency)
				if err != nil {
					return err
				}
				diffs = append(diffs, domain.ReconciliationDifference{
					Type:            domain.DiffAmountMismatch,
					Subject:         domain.SubjectPayment,
					TxnID:           txnID,
					ExternalTxnID:   r.ExternalTxnID,
					InternalAmount:  p.Amount,
					StatementAmount: r.Amount,
					Currency:        batch.Currency,
					AutoAdjusted:    true,
					JournalID:       j.ID,
					Reason: fmt.Sprintf("交易 %s 金额不符：内部 %d，对账单 %d，差额 %d，已挂暂记调整",
						txnID, p.Amount, r.Amount, delta),
				})
				continue
			}
			matched++
		}

		// 再扫内部：内部有、对账单完全没有的支付与退款。
		var internalIDs []string
		for id := range internal {
			internalIDs = append(internalIDs, id)
		}
		sort.Strings(internalIDs)
		for _, id := range internalIDs {
			if _, ok := stmtByTxn[id]; ok {
				continue
			}
			p := internal[id]
			diffs = append(diffs, domain.ReconciliationDifference{
				Type:           domain.DiffMissingStatement,
				Subject:        domain.SubjectPayment,
				TxnID:          id,
				InternalAmount: p.Amount,
				Currency:       batch.Currency,
				Reason: fmt.Sprintf("内部已结算支付 %s 金额 %d 在对账单中缺失，需人工核查",
					id, p.Amount),
			})
		}
		var refundIDs []string
		for id := range internalRefund {
			refundIDs = append(refundIDs, id)
		}
		sort.Strings(refundIDs)
		for _, id := range refundIDs {
			if _, ok := stmtByTxn[id]; ok {
				continue
			}
			rf := internalRefund[id]
			diffs = append(diffs, domain.ReconciliationDifference{
				Type:           domain.DiffMissingStatement,
				Subject:        domain.SubjectRefund,
				TxnID:          id,
				InternalAmount: rf.Amount,
				Currency:       batch.Currency,
				Reason: fmt.Sprintf("内部已退款 %s（支付 %s）金额 %d 在对账单中缺失，需人工核查",
					id, rf.PaymentID, rf.Amount),
			})
		}

		run.MatchedCount = matched
		sort.SliceStable(diffs, func(i, j int) bool {
			if diffs[i].TxnID != diffs[j].TxnID {
				return diffs[i].TxnID < diffs[j].TxnID
			}
			return diffs[i].Type < diffs[j].Type
		})
		run.Differences = diffs
		if err := tx.SaveReconRun(run); err != nil {
			return err
		}
		out = run
		return nil
	})
	return out, err
}

// postMissingInternalAdjustment 对账单有、内部无的暂记调整。
// 对账单金额代表渠道实际代收，平台欠商户这笔钱：借 备付金资产 / 贷 暂记负债。
func (s *Service) postMissingInternalAdjustment(tx *storage.Tx, txnID string, amount domain.Money, suspenseID string, c domain.Currency) (*domain.Journal, error) {
	floatID, err := s.ledger.EnsureFloatInTx(tx, c)
	if err != nil {
		return nil, err
	}
	lines := []domain.JournalLine{
		{AccountID: floatID, Debit: amount, Reference: txnID, Note: "对账差异：渠道实收挂账"},
		{AccountID: suspenseID, Credit: amount, Reference: txnID, Note: "对账差异：内部漏账暂记"},
	}
	return s.ledger.PostInTx(tx, ledger.BizReconAdj, "RECON:"+txnID+":MISSING_INTERNAL",
		c, lines, "对账自动调整-内部缺失 "+txnID)
}

// postMismatchAdjustment 金额不符的差额调整（delta = 对账单 - 内部）。
func (s *Service) postMismatchAdjustment(tx *storage.Tx, txnID string, internalAmt, stmtAmt domain.Money, suspensePayableID, suspenseReceivableID string, c domain.Currency) (*domain.Journal, error) {
	delta := stmtAmt.Sub(internalAmt)
	floatID, err := s.ledger.EnsureFloatInTx(tx, c)
	if err != nil {
		return nil, err
	}
	var lines []domain.JournalLine
	if delta > 0 {
		// 渠道多收：借 备付金 / 贷 暂记应付
		lines = []domain.JournalLine{
			{AccountID: floatID, Debit: delta, Reference: txnID, Note: "对账差异：渠道多收"},
			{AccountID: suspensePayableID, Credit: delta, Reference: txnID, Note: "对账差异：金额不符暂记应付"},
		}
	} else {
		// 渠道少收：借 暂记应收 / 贷 备付金
		lines = []domain.JournalLine{
			{AccountID: suspenseReceivableID, Debit: delta.Negative(), Reference: txnID, Note: "对账差异：金额不符暂记应收"},
			{AccountID: floatID, Credit: delta.Negative(), Reference: txnID, Note: "对账差异：渠道少收"},
		}
	}
	return s.ledger.PostInTx(tx, ledger.BizReconAdj, "RECON:"+txnID+":MISMATCH",
		c, lines, "对账自动调整-金额不符 "+txnID)
}

func (s *Service) GetRun(batchID string) (*domain.ReconciliationRun, error) {
	var out *domain.ReconciliationRun
	err := s.store.View(func(tx *storage.Tx) error {
		r, _ := tx.GetReconRun(batchID)
		if r == nil {
			return domain.NewError(domain.ErrInvalid, "批次 %s 尚无对账记录", batchID)
		}
		out = r
		return nil
	})
	return out, err
}
