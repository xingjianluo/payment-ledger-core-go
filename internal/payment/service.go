package payment

import (
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type Clock interface{ Now() time.Time }

// AcceptResult 为受理结果；Replayed=true 表示命中已完成的幂等记录，直接返回原支付。
type AcceptResult struct {
	Payment  *domain.Payment
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

// Accept 受理支付请求。
//
// 幂等语义：
//   - 必须携带非空幂等键；
//   - 相同幂等键 + 相同内容（付款方/收款方/金额/币种/附言）：返回首次受理的支付单，
//     不重复扣款，Replayed=true；
//   - 相同幂等键 + 不同内容：返回 ErrIdempotencyConflict，明确拒绝；
//   - 相同幂等键并发到达：临界区内后到者看到 IN_FLIGHT 记录，返回 ErrIdempotencyConflict，
//     调用方应稍后用相同请求重试，最终拿到幂等结果。
//
// 受理成功即时过账：借 付款方账户 / 贷 收款方账户（PAYMENT 凭证）。
func (s *Service) Accept(req *domain.PaymentRequest) (*AcceptResult, error) {
	if req == nil {
		return nil, domain.NewError(domain.ErrInvalid, "支付请求不能为空")
	}
	if req.IdempotencyKey == "" {
		return nil, domain.NewError(domain.ErrInvalid, "幂等键不能为空")
	}
	if req.PayerAccountID == "" || req.PayeeAccountID == "" {
		return nil, domain.NewError(domain.ErrInvalid, "付款方与收款方账户不能为空")
	}
	if req.PayerAccountID == req.PayeeAccountID {
		return nil, domain.NewError(domain.ErrInvalid, "付款方与收款方不能相同")
	}
	if req.Amount <= 0 {
		return nil, domain.NewError(domain.ErrInvalid, "支付金额必须为正数")
	}
	if req.Currency == "" {
		return nil, domain.NewError(domain.ErrInvalid, "币种不能为空")
	}
	fp := domain.FingerprintOf(req)

	var result *AcceptResult
	err := s.store.Update(func(tx *storage.Tx) error {
		rec, _ := tx.GetIdempotency(req.IdempotencyKey)
		if rec != nil {
			if rec.Fingerprint != fp {
				return domain.NewError(domain.ErrIdempotencyConflict,
					"幂等键 %s 已用于不同内容的请求: 既有{付款=%s 收款=%s 金额=%d %s} 本次{付款=%s 收款=%s 金额=%d %s}",
					req.IdempotencyKey,
					rec.Fingerprint.PayerAccountID, rec.Fingerprint.PayeeAccountID, rec.Fingerprint.Amount, rec.Fingerprint.Currency,
					fp.PayerAccountID, fp.PayeeAccountID, fp.Amount, fp.Currency)
			}
			if rec.Status == domain.IdempotencyDone {
				// 幂等命中：直接取首次结果，绝不重复过账。
				existing, err := tx.GetPayment(rec.ResultID)
				if err != nil {
					return err
				}
				result = &AcceptResult{Payment: existing, Replayed: true}
				return nil
			}
			// IN_FLIGHT：同键并发的第二个请求。
			return domain.NewError(domain.ErrIdempotencyConflict,
				"幂等键 %s 正在处理中（并发重复提交），请稍后以相同请求重试", req.IdempotencyKey)
		}

		now := s.clock.Now()
		inflight := &domain.IdempotencyRecord{
			Key: req.IdempotencyKey, Fingerprint: fp,
			Status: domain.IdempotencyInFlight, CreatedAt: now,
		}
		if err := tx.SaveIdempotency(inflight); err != nil {
			return err
		}

		// 同一临界区内过账：借 付款方钱包(负债减少) / 贷 商户应付款(负债增加，待结算)。
		payer, err := tx.GetAccount(req.PayerAccountID)
		if err != nil {
			return err
		}
		payee, err := tx.GetAccount(req.PayeeAccountID)
		if err != nil {
			return err
		}
		if payer.Currency != req.Currency || payee.Currency != req.Currency {
			return domain.NewError(domain.ErrCurrencyMismatch,
				"币种不一致: 请求=%s 付款账户=%s 收款账户=%s", req.Currency, payer.Currency, payee.Currency)
		}
		payableID, err := s.ledger.EnsurePayableInTx(tx, req.PayeeAccountID, req.Currency)
		if err != nil {
			return err
		}
		lines := []domain.JournalLine{
			{AccountID: req.PayerAccountID, Debit: req.Amount, Note: "支付扣款 " + req.Reference},
			{AccountID: payableID, Credit: req.Amount, Note: "商户待结算款"},
		}
		j, err := s.ledger.PostInTx(tx, ledger.BizPayment, "PAY:"+req.IdempotencyKey,
			req.Currency, lines, "支付受理 "+req.Reference)
		if err != nil {
			return err
		}

		p := &domain.Payment{
			ID:             tx.NextID("pay"),
			IdempotencyKey: req.IdempotencyKey,
			PayerAccountID: req.PayerAccountID,
			PayeeAccountID: req.PayeeAccountID,
			Amount:         req.Amount,
			Currency:       req.Currency,
			Status:         domain.PaymentStatusPendingSettlement,
			Reference:      req.Reference,
			JournalID:      j.ID,
			CreatedAt:      now,
		}
		if err := tx.SavePayment(p); err != nil {
			return err
		}
		done := &domain.IdempotencyRecord{
			Key: req.IdempotencyKey, Fingerprint: fp,
			Status: domain.IdempotencyDone, ResultID: p.ID,
			CreatedAt: now, ResolvedAt: &now,
		}
		if err := tx.SaveIdempotency(done); err != nil {
			return err
		}
		result = &AcceptResult{Payment: p}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) Get(id string) (*domain.Payment, error) {
	var out *domain.Payment
	err := s.store.View(func(tx *storage.Tx) error {
		p, err := tx.GetPayment(id)
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}
