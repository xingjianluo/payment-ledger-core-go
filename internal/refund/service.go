// Package refund 提供本地退款能力：对已受理成功的支付发起全额或部分退款。
// 不依赖任何外部支付服务，退款通过账本复式记账落账。
package refund

import (
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type Clock interface{ Now() time.Time }

// RefundResult 为退款结果；Replayed=true 表示命中已完成的幂等记录，直接返回原退款单。
type RefundResult struct {
	Refund   *domain.Refund
	Replayed bool
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

// Refund 对一笔已受理支付发起退款（全额或部分）。
//
// 幂等语义（与支付受理一致）：
//   - 必须携带非空幂等键；
//   - 相同幂等键 + 相同内容（支付号/金额/币种/原因）：返回首次退款单，不重复退，Replayed=true；
//   - 相同幂等键 + 不同内容：返回 ErrIdempotencyConflict，明确拒绝；
//   - 相同幂等键并发到达：后到者看到 IN_FLIGHT 记录，返回 ErrIdempotencyConflict。
//
// 失败语义：
//   - 失败原因可区分：原支付不存在 ErrPaymentNotFound、金额非正 ErrInvalid、
//     累计超过可退余额 ErrRefundExceeds、币种不一致 ErrCurrencyMismatch、
//     账户不存在 ErrAccountNotFound、商户钱包余额不足 ErrInsufficientFunds；
//   - 任何失败都在同一原子临界区内整体回滚：幂等键不被失败占用，
//     之后用同键同内容重试得到与首次相同的失败结论，换新键可正常发起。
//
// 记账规则（按原支付当前状态选择退款资金来源，与账上状态对齐）：
//   - 支付未结算（PENDING_SETTLEMENT）：钱还在商户应付款科目，
//     借 商户应付款 / 贷 付款人钱包（REFUND 凭证）；
//   - 支付已结算（SETTLED）：钱已到商户钱包，
//     借 商户钱包 / 贷 付款人钱包（REFUND 凭证）。
//
// 整笔退款要么全部落账（凭证 + 退款单 + 支付累计退款额 + 幂等记录），
// 要么一分钱都不动，不存在退一半的部分状态。
func (s *Service) Refund(req *domain.RefundRequest) (*RefundResult, error) {
	if req == nil {
		return nil, domain.NewError(domain.ErrInvalid, "退款请求不能为空")
	}
	if req.IdempotencyKey == "" {
		return nil, domain.NewError(domain.ErrInvalid, "幂等键不能为空")
	}
	if req.PaymentID == "" {
		return nil, domain.NewError(domain.ErrInvalid, "原支付号不能为空")
	}
	if req.Amount <= 0 {
		return nil, domain.NewError(domain.ErrInvalid, "退款金额必须为正数")
	}
	fp := domain.Fingerprint{Kind: domain.FingerprintKindRefund, Refund: domain.RefundFingerprintOf(req)}

	var result *RefundResult
	err := s.store.Update(func(tx *storage.Tx) error {
		rec, _ := tx.GetIdempotency(req.IdempotencyKey)
		if rec != nil {
			if rec.Fingerprint != fp {
				return domain.NewError(domain.ErrIdempotencyConflict,
					"幂等键 %s 已用于不同内容的请求: 既有{%s 支付=%s 金额=%d} 本次{支付=%s 金额=%d}",
					req.IdempotencyKey, rec.Fingerprint.Kind,
					rec.Fingerprint.Refund.PaymentID, rec.Fingerprint.Refund.Amount,
					fp.Refund.PaymentID, fp.Refund.Amount)
			}
			if rec.Status == domain.IdempotencyDone {
				// 幂等命中：直接取首次结果，绝不重复退款。
				existing, err := tx.GetRefund(rec.ResultID)
				if err != nil {
					return err
				}
				result = &RefundResult{Refund: existing, Replayed: true}
				return nil
			}
			return domain.NewError(domain.ErrIdempotencyConflict,
				"幂等键 %s 正在处理中（并发重复提交），请稍后以相同请求重试", req.IdempotencyKey)
		}

		now := s.clock.Now()
		if err := tx.SaveIdempotency(&domain.IdempotencyRecord{
			Key: req.IdempotencyKey, Fingerprint: fp,
			Status: domain.IdempotencyInFlight, CreatedAt: now,
		}); err != nil {
			return err
		}

		p, err := tx.GetPayment(req.PaymentID)
		if err != nil {
			return err // ErrPaymentNotFound
		}
		if req.Currency != "" && req.Currency != p.Currency {
			return domain.NewError(domain.ErrCurrencyMismatch,
				"退款币种 %s 与原支付 %s 币种 %s 不一致", req.Currency, p.ID, p.Currency)
		}
		remaining := p.Amount - p.RefundedAmount
		if req.Amount > remaining {
			return domain.NewError(domain.ErrRefundExceeds,
				"退款金额 %d 超过支付 %s 可退余额 %d（原额 %d，已退 %d）",
				req.Amount, p.ID, remaining, p.Amount, p.RefundedAmount)
		}
		// 付款人钱包必须存在（退款入账方）。
		if _, err := tx.GetAccount(p.PayerAccountID); err != nil {
			return err
		}

		// 按支付当前状态选择退款资金来源。
		var lines []domain.JournalLine
		switch p.Status {
		case domain.PaymentStatusPendingSettlement:
			// 钱还在商户应付款科目：借 应付款 / 贷 付款人钱包。
			payableID, err := s.ledger.EnsurePayableInTx(tx, p.PayeeAccountID, p.Currency)
			if err != nil {
				return err
			}
			lines = []domain.JournalLine{
				{AccountID: payableID, Debit: req.Amount, Note: "退款冲减待结算款 " + req.Reason},
				{AccountID: p.PayerAccountID, Credit: req.Amount, Note: "退款入账"},
			}
		case domain.PaymentStatusSettled:
			// 钱已到商户钱包：借 商户钱包 / 贷 付款人钱包（商户余额不足则整体失败）。
			if _, err := tx.GetAccount(p.PayeeAccountID); err != nil {
				return err
			}
			lines = []domain.JournalLine{
				{AccountID: p.PayeeAccountID, Debit: req.Amount, Note: "退款出账 " + req.Reason},
				{AccountID: p.PayerAccountID, Credit: req.Amount, Note: "退款入账"},
			}
		default:
			return domain.NewError(domain.ErrInvalid, "支付 %s 状态 %s 不支持退款", p.ID, p.Status)
		}
		j, err := s.ledger.PostInTx(tx, ledger.BizRefund, "REFUND:"+req.IdempotencyKey,
			p.Currency, lines, "退款 "+req.Reason)
		if err != nil {
			return err
		}

		r := &domain.Refund{
			ID:             tx.NextID("rfnd"),
			IdempotencyKey: req.IdempotencyKey,
			PaymentID:      p.ID,
			PayerAccountID: p.PayerAccountID,
			PayeeAccountID: p.PayeeAccountID,
			Amount:         req.Amount,
			Currency:       p.Currency,
			Status:         domain.RefundStatusRefunded,
			Reason:         req.Reason,
			JournalID:      j.ID,
			CreatedAt:      now,
		}
		if err := tx.SaveRefund(r); err != nil {
			return err
		}
		// 同一临界区内累计已退金额：并发退款串行化，不超退、不丢更新。
		p.RefundedAmount += req.Amount
		if err := tx.SavePayment(p); err != nil {
			return err
		}
		if err := tx.SaveIdempotency(&domain.IdempotencyRecord{
			Key: req.IdempotencyKey, Fingerprint: fp,
			Status: domain.IdempotencyDone, ResultID: r.ID,
			CreatedAt: now, ResolvedAt: &now,
		}); err != nil {
			return err
		}
		result = &RefundResult{Refund: r}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) Get(id string) (*domain.Refund, error) {
	var out *domain.Refund
	err := s.store.View(func(tx *storage.Tx) error {
		r, err := tx.GetRefund(id)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

// ListByPayment 列出一笔支付的全部退款（按退款号排序）。
func (s *Service) ListByPayment(paymentID string) ([]*domain.Refund, error) {
	var out []*domain.Refund
	err := s.store.View(func(tx *storage.Tx) error {
		rs, err := tx.ListRefundsByPayment(paymentID)
		if err != nil {
			return err
		}
		out = rs
		return nil
	})
	return out, err
}
