// Package payment 实现支付请求受理与幂等处理。
package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

// Request 支付受理请求。
type Request struct {
	IdemKey      string // 幂等键
	TxnID        string // 商户/渠道交易号
	MerchantAcct string // 收款商户账户
	Amount       int64
	Currency     string
}

// Result 受理结果。
type Result struct {
	TxnID    string
	Status   domain.TxnStatus
	Replayed bool // 是否命中幂等重放
}

// Service 支付受理服务。
type Service struct {
	db           *storage.DB
	ledger       *ledger.Service
	acquirerAcct string // 收单/待清算资金账户（资金来源）
}

// New 创建支付受理服务。acquirerAcct 为各币种收单过渡账户。
func New(db *storage.DB, l *ledger.Service, acquirerAcct string) *Service {
	return &Service{db: db, ledger: l, acquirerAcct: acquirerAcct}
}

// requestHash 对幂等键所绑定的请求内容做摘要，用于识别"同键不同内容"。
func requestHash(req Request) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", req.TxnID, req.MerchantAcct, req.Amount, req.Currency)))
	return hex.EncodeToString(h[:])
}

// Accept 受理一笔支付。
//   - 同一幂等键 + 相同内容：重放首次结果（Replayed=true），不重复扣款；
//   - 同一幂等键 + 不同内容：返回 CodeIdemConflict；
//   - 首次业务失败：按具体错误码登记为 FAILED，重试同样重放该失败，不产生新交易。
func (s *Service) Accept(ctx context.Context, req Request) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.IdemKey == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "idempotency key required")
	}
	if req.TxnID == "" || req.MerchantAcct == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "txn id and merchant account required")
	}
	if req.Amount <= 0 {
		return nil, apperr.New(apperr.CodeInvalidAmount, "amount must be > 0: txn=%s", req.TxnID)
	}
	if req.Currency == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "currency required: txn=%s", req.TxnID)
	}

	rh := requestHash(req)

	s.db.Lock()
	rec := s.db.GetIdem(req.IdemKey)
	if rec != nil {
		// 命中已有幂等记录：在锁内判定后立即释放，再返回。
		s.db.Unlock()
		return s.replay(req.IdemKey, rec, rh)
	}
	// 先登记 IN_FLIGHT，占位该幂等键。
	inflight := &domain.IdempotencyRecord{
		Key:         req.IdemKey,
		RequestHash: rh,
		State:       domain.IdemInFlight,
	}
	s.db.PutIdem(inflight)
	s.db.Unlock()

	// 执行复式记账（Post 内部自行加锁）：
	// 借：收单过渡账户  贷：商户账户。
	entries := []domain.Entry{
		{AccountID: s.acquirerAcct, Side: domain.Debit, Amount: req.Amount},
		{AccountID: req.MerchantAcct, Side: domain.Credit, Amount: req.Amount},
	}
	txn, postErr := s.ledger.Post(req.TxnID, domain.TxnPayment, req.Currency, req.IdemKey, "", entries)

	// 终态回写（独立临界区）。
	s.db.Lock()
	defer s.db.Unlock()
	final := &domain.IdempotencyRecord{Key: req.IdemKey, RequestHash: rh}
	if postErr != nil {
		code := errorCode(postErr)
		final.State = domain.IdemFailed
		final.ErrCode = string(code)
		s.db.PutIdem(final)
		return nil, apperr.New(code, "idem=%s original failure replayed: %s", req.IdemKey, postErr.Error())
	}
	final.State = domain.IdemDone
	final.TxnID = txn.ID
	s.db.PutIdem(final)

	return &Result{TxnID: txn.ID, Status: txn.Status, Replayed: false}, nil
}

// replay 处理命中幂等记录的请求。
func (s *Service) replay(key string, rec *domain.IdempotencyRecord, hash string) (*Result, error) {
	// 同一幂等键携带不同内容 -> 明确拒绝。
	if rec.RequestHash != hash {
		return nil, apperr.New(apperr.CodeIdemConflict,
			"idempotency key %s reused with different request content", key)
	}
	switch rec.State {
	case domain.IdemDone:
		return &Result{TxnID: rec.TxnID, Status: domain.StatusPending, Replayed: true}, nil
	case domain.IdemFailed:
		code := apperr.Code(rec.ErrCode)
		if code == "" {
			code = apperr.CodeInvalidArgument
		}
		return nil, apperr.New(code, "idem=%s replayed previous failure code=%s", key, code)
	default:
		// IN_FLIGHT：当前为单机同步模型，出现该状态说明此前处理异常中断。
		return nil, apperr.New(apperr.CodeIdemMissing, "idem=%s is in-flight; retry later", key)
	}
}

// errorCode 提取业务错误码。
func errorCode(err error) apperr.Code {
	if ae, ok := err.(*apperr.Error); ok {
		return ae.Code
	}
	return apperr.CodeInvalidArgument
}
