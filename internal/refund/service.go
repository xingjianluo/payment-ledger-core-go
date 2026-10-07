// Package refund 提供本地退款能力：对已受理成功的支付做全额或部分退款。
package refund

import (
	"errors"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type Clock interface{ Now() time.Time }

// RefundResult 为退款结果；Replayed=true 表示命中已完成的幂等记录，直接返回首次退款单。
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

// Refund 发起退款。
//
// 幂等语义（与支付受理一致，并覆盖失败场景）：
//   - 必须携带非空幂等键；
//   - 相同幂等键 + 相同内容（支付号/金额/币种/原因）：返回首次退款单，不重复退款，Replayed=true；
//   - 相同幂等键 + 不同内容：返回 ErrIdempotencyConflict，明确拒绝；
//   - 首次因"说得清的原因"失败（支付不存在/金额不合法/超可退余额/币种不符/余额不足）：
//     失败结论按幂等键落档；同键同内容重试返回**同样的失败结论**（原因稳定、不含糊），
//     同键不同内容仍按冲突拒绝；换新的幂等键可正常重新发起；
//   - 相同幂等键并发到达：后到者看到 IN_FLIGHT，返回 ErrIdempotencyConflict，稍后以相同请求重试。
//
// 记账（在同一原子临界区内完成，要么整笔落账要么一分钱不动）：
//   - 支付尚未结算（含已进待结算批次）：借 商户待结算款 PAYABLE_MERCHANT:<商户> / 贷 付款方钱包，
//     退款直接从待结算资金池冲减，后续结算只会结"支付额 − 已退额"；
//   - 支付已结算：借 商户钱包 / 贷 付款方钱包，商户钱包余额不足则失败（ErrInsufficientFunds）。
//
// 同一笔支付可多次部分退款，累计退款额不得超过原支付金额（ErrRefundExceeds）；
// 并发退款在同一存储临界区内串行化，不超退、不丢更新。
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
	if req.Currency == "" {
		return nil, domain.NewError(domain.ErrInvalid, "币种不能为空")
	}
	fp := domain.RefundFingerprintOf(req)

	var result *RefundResult
	var bizErr error
	err := s.store.Update(func(tx *storage.Tx) error {
		rec, _ := tx.GetIdempotency(req.IdempotencyKey)
		if rec != nil {
			if rec.Kind != domain.IdempotencyKindRefund || rec.RefundFingerprint != fp {
				return domain.NewError(domain.ErrIdempotencyConflict,
					"幂等键 %s 已用于不同内容的请求（业务类型或退款指纹不一致）", req.IdempotencyKey)
			}
			switch rec.Status {
			case domain.IdempotencyDone:
				existing, err := tx.GetRefund(rec.ResultID)
				if err != nil {
					return err
				}
				result = &RefundResult{Refund: existing, Replayed: true}
				return nil
			case domain.IdempotencyFailed:
				// 失败结论稳定重放：与首次失败完全同因。
				bizErr = &domain.LedgerError{Code: rec.FailureCode, Message: rec.FailureMessage}
				return nil
			default: // IN_FLIGHT：同键并发的第二个请求。
				return domain.NewError(domain.ErrIdempotencyConflict,
					"幂等键 %s 正在处理中（并发重复提交），请稍后以相同请求重试", req.IdempotencyKey)
			}
		}

		now := s.clock.Now()
		if err := tx.SaveIdempotency(&domain.IdempotencyRecord{
			Key: req.IdempotencyKey, Kind: domain.IdempotencyKindRefund,
			RefundFingerprint: fp, Status: domain.IdempotencyInFlight, CreatedAt: now,
		}); err != nil {
			return err
		}

		// fail 把"说得清原因"的业务失败按幂等键落档并提交（不落账、不动余额），
		// 保证同键同内容重试得到同样的失败结论。
		fail := func(err error) error {
			var le *domain.LedgerError
			if !errors.As(err, &le) {
				return err // 非领域错误：整体回滚
			}
			if saveErr := tx.SaveIdempotency(&domain.IdempotencyRecord{
				Key: req.IdempotencyKey, Kind: domain.IdempotencyKindRefund,
				RefundFingerprint: fp, Status: domain.IdempotencyFailed,
				FailureCode: le.Code, FailureMessage: le.Message,
				CreatedAt: now, ResolvedAt: &now,
			}); saveErr != nil {
				return saveErr
			}
			bizErr = le
			return nil
		}

		p, err := tx.GetPayment(req.PaymentID)
		if err != nil {
			return fail(err)
		}
		if req.Currency != p.Currency {
			return fail(domain.NewError(domain.ErrCurrencyMismatch,
				"退款币种 %s 与原支付 %s 币种 %s 不一致", req.Currency, p.ID, p.Currency))
		}
		if remaining := p.RemainingRefundable(); req.Amount > remaining {
			return fail(domain.NewError(domain.ErrRefundExceeds,
				"退款金额 %d 超过支付 %s 剩余可退金额 %d（原额 %d 已退 %d）",
				req.Amount, p.ID, remaining, p.Amount, p.RefundedAmount))
		}

		// 按支付当前账上状态选择退款资金来源。
		var lines []domain.JournalLine
		if p.Status == domain.PaymentStatusSettled {
			// 已结算：钱在商户钱包，从商户钱包退回付款方。
			lines = []domain.JournalLine{
				{AccountID: p.PayeeAccountID, Debit: req.Amount, Note: "退款出账 " + req.Reason},
				{AccountID: p.PayerAccountID, Credit: req.Amount, Note: "退款入账 " + req.Reason},
			}
		} else {
			// 未结算（含已进待结算批次）：钱还在商户待结算款，直接冲减。
			payableID, err := s.ledger.EnsurePayableInTx(tx, p.PayeeAccountID, p.Currency)
			if err != nil {
				return err
			}
			lines = []domain.JournalLine{
				{AccountID: payableID, Debit: req.Amount, Note: "退款冲减待结算款 " + req.Reason},
				{AccountID: p.PayerAccountID, Credit: req.Amount, Note: "退款入账 " + req.Reason},
			}
		}
		j, err := s.ledger.PostInTx(tx, ledger.BizRefund, "REFUND:"+req.IdempotencyKey,
			p.Currency, lines, "退款 "+req.Reason)
		if err != nil {
			return fail(err)
		}

		p.RefundedAmount = p.RefundedAmount.Add(req.Amount)
		if err := tx.SavePayment(p); err != nil {
			return err
		}
		rf := &domain.Refund{
			ID:             tx.NextID("ref"),
			IdempotencyKey: req.IdempotencyKey,
			PaymentID:      p.ID,
			Amount:         req.Amount,
			Currency:       p.Currency,
			Reason:         req.Reason,
			JournalID:      j.ID,
			CreatedAt:      now,
		}
		if err := tx.SaveRefund(rf); err != nil {
			return err
		}
		if err := tx.SaveIdempotency(&domain.IdempotencyRecord{
			Key: req.IdempotencyKey, Kind: domain.IdempotencyKindRefund,
			RefundFingerprint: fp, Status: domain.IdempotencyDone, ResultID: rf.ID,
			CreatedAt: now, ResolvedAt: &now,
		}); err != nil {
			return err
		}
		result = &RefundResult{Refund: rf}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if bizErr != nil {
		return nil, bizErr
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
