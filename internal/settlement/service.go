package settlement

import (
	"sort"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type Clock interface{ Now() time.Time }

type Service struct {
	store  *storage.Store
	ledger *ledger.Service
	fees   domain.FeePolicy
	clock  Clock
}

func New(st *storage.Store, l *ledger.Service, fees domain.FeePolicy, clock Clock) *Service {
	if clock == nil {
		clock = ledger.SystemClock()
	}
	return &Service{store: st, ledger: l, fees: fees, clock: clock}
}

// CreateBatch 把待结算支付打包为一个结算批次。
// 规则：所有支付必须存在、同币种、处于 PENDING_SETTLEMENT 且尚未归入任何批次。
// 手续费按每笔支付独立计算后汇总（与结算时口径一致），批次仅支持单一币种。
func (s *Service) CreateBatch(currency domain.Currency, paymentIDs []string) (*domain.SettlementBatch, error) {
	if currency == "" {
		return nil, domain.NewError(domain.ErrInvalid, "结算币种不能为空")
	}
	if len(paymentIDs) == 0 {
		return nil, domain.NewError(domain.ErrInvalid, "结算批次不能为空")
	}
	var out *domain.SettlementBatch
	err := s.store.Update(func(tx *storage.Tx) error {
		var gross, feeTotal domain.Money
		ids := append([]string(nil), paymentIDs...)
		sort.Strings(ids)
		for _, id := range ids {
			p, err := tx.GetPayment(id)
			if err != nil {
				return err
			}
			if p.Currency != currency {
				return domain.NewError(domain.ErrCurrencyMismatch,
					"批次币种 %s 与支付 %s 币种 %s 不一致", currency, id, p.Currency)
			}
			if p.Status != domain.PaymentStatusPendingSettlement {
				return domain.NewError(domain.ErrBatchSettled,
					"支付 %s 当前状态 %s，不可加入新批次", id, p.Status)
			}
			if p.SettlementBatchID != "" {
				return domain.NewError(domain.ErrBatchSettled,
					"支付 %s 已归入批次 %s，不得重复打包", id, p.SettlementBatchID)
			}
			gross = gross.Add(p.Amount)
			feeTotal = feeTotal.Add(s.fees.Fee(p.Amount))
		}

		now := s.clock.Now()
		batch := &domain.SettlementBatch{
			ID:          tx.NextID("batch"),
			Currency:    currency,
			Status:      domain.BatchStatusOpen,
			PaymentIDs:  ids,
			Count:       len(ids),
			GrossAmount: gross,
			FeeTotal:    feeTotal,
			NetAmount:   gross.Sub(feeTotal),
			CreatedAt:   now,
		}
		if err := tx.SaveBatch(batch); err != nil {
			return err
		}
		// 将支付挂到批次上（仍为待结算状态，结算时才置 SETTLED）。
		for _, id := range ids {
			p, _ := tx.GetPayment(id)
			p.SettlementBatchID = batch.ID
			if err := tx.SavePayment(p); err != nil {
				return err
			}
		}
		out = batch
		return nil
	})
	return out, err
}

// SettleBatch 结算批次（幂等：已结算批次返回原批次，不重复过账）。
//
// 在同一原子临界区内过两张复式凭证：
//  1. SETTLEMENT：借 商户应付款(按商户汇总的总金额) / 贷 各商户钱包(总金额)；
//  2. SETTLEMENT_FEE：借 各商户钱包(手续费) / 贷 手续费收入(手续费合计)。
//
// 完成后批次置 SETTLED，支付置 SETTLED；任何一步失败整体回滚。
func (s *Service) SettleBatch(batchID string) (*domain.SettlementBatch, error) {
	var out *domain.SettlementBatch
	err := s.store.Update(func(tx *storage.Tx) error {
		batch, err := tx.GetBatch(batchID)
		if err != nil {
			return err
		}
		if batch.Status == domain.BatchStatusSettled {
			// 结算幂等：重复执行直接返回既有结果。
			out = batch
			return nil
		}
		payments, err := tx.ListPaymentsByIDs(batch.PaymentIDs)
		if err != nil {
			return err
		}

		// 按商户汇总。
		grossByPayee := map[string]domain.Money{}
		feeByPayee := map[string]domain.Money{}
		payableByPayee := map[string]string{}
		var order []string
		var feeTotal domain.Money
		for _, p := range payments {
			if p.Status != domain.PaymentStatusPendingSettlement {
				return domain.NewError(domain.ErrBatchSettled,
					"支付 %s 状态为 %s，批次不可结算", p.ID, p.Status)
			}
			if _, ok := grossByPayee[p.PayeeAccountID]; !ok {
				order = append(order, p.PayeeAccountID)
				payableID, err := s.ledger.EnsurePayableInTx(tx, p.PayeeAccountID, batch.Currency)
				if err != nil {
					return err
				}
				payableByPayee[p.PayeeAccountID] = payableID
			}
			grossByPayee[p.PayeeAccountID] = grossByPayee[p.PayeeAccountID].Add(p.Amount)
			fee := s.fees.Fee(p.Amount)
			feeByPayee[p.PayeeAccountID] = feeByPayee[p.PayeeAccountID].Add(fee)
			feeTotal = feeTotal.Add(fee)
		}
		sort.Strings(order)

		// 凭证1：待结算款结到商户钱包（总额过账）。
		var settleLines []domain.JournalLine
		for _, payee := range order {
			settleLines = append(settleLines, domain.JournalLine{
				AccountID: payableByPayee[payee],
				Debit:     grossByPayee[payee],
				Note:      "结算出账",
			})
			settleLines = append(settleLines, domain.JournalLine{
				AccountID: payee,
				Credit:    grossByPayee[payee],
				Note:      "结算入账",
			})
		}
		sj, err := s.ledger.PostInTx(tx, ledger.BizSettlement, "SETTLE:"+batch.ID,
			batch.Currency, settleLines, "批次结算 "+batch.ID)
		if err != nil {
			return err
		}

		// 凭证2：从商户钱包扣手续费，记手续费收入。
		feeIncomeID, err := s.ledger.EnsureFeeIncomeInTx(tx, batch.Currency)
		if err != nil {
			return err
		}
		var feeLines []domain.JournalLine
		for _, payee := range order {
			if feeByPayee[payee] == 0 {
				continue
			}
			feeLines = append(feeLines, domain.JournalLine{
				AccountID: payee, Debit: feeByPayee[payee], Note: "结算手续费",
			})
		}
		var fjID string
		if len(feeLines) > 0 {
			feeLines = append(feeLines, domain.JournalLine{
				AccountID: feeIncomeID, Credit: feeTotal, Note: "手续费收入",
			})
			fj, err := s.ledger.PostInTx(tx, ledger.BizFee, "FEE:"+batch.ID,
				batch.Currency, feeLines, "批次手续费 "+batch.ID)
			if err != nil {
				return err
			}
			fjID = fj.ID
		}

		now := s.clock.Now()
		batch.Status = domain.BatchStatusSettled
		batch.SettlementJournalID = sj.ID
		batch.FeeJournalID = fjID
		batch.SettledAt = &now
		if err := tx.SaveBatch(batch); err != nil {
			return err
		}
		for _, p := range payments {
			p.Status = domain.PaymentStatusSettled
			p.SettledAt = &now
			if err := tx.SavePayment(p); err != nil {
				return err
			}
		}
		out = batch
		return nil
	})
	return out, err
}

func (s *Service) GetBatch(id string) (*domain.SettlementBatch, error) {
	var out *domain.SettlementBatch
	err := s.store.View(func(tx *storage.Tx) error {
		b, err := tx.GetBatch(id)
		if err != nil {
			return err
		}
		out = b
		return nil
	})
	return out, err
}
