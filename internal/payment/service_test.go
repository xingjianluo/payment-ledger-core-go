package payment

import (
	"context"
	"testing"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

func newHarness(t *testing.T) (*Service, *ledger.Service) {
	t.Helper()
	db := storage.NewDB()
	l := ledger.New(db)
	mustOpen(t, l, "ACQ_CNY", "CNY", 100) // 收单户余额有限，用于制造余额不足
	mustOpen(t, l, "MCH_CNY", "CNY", 0)
	mustOpen(t, l, "MCH_USD", "USD", 0)
	return New(db, l, "ACQ_CNY"), l
}

func mustOpen(t *testing.T, l *ledger.Service, id, ccy string, bal int64) {
	t.Helper()
	if _, err := l.OpenAccount(id, ccy, bal); err != nil {
		t.Fatalf("open account %s: %v", id, err)
	}
}

func codeOf(err error) apperr.Code {
	if err == nil {
		return ""
	}
	if ae, ok := err.(*apperr.Error); ok {
		return ae.Code
	}
	return ""
}

// TestAccept_IdempotentReplay 同一幂等键 + 相同内容重复提交不得重复扣款。
func TestAccept_IdempotentReplay(t *testing.T) {
	svc, l := newHarness(t)
	ctx := context.Background()
	req := Request{IdemKey: "IDEM-REPLAY-1", TxnID: "P-1", MerchantAcct: "MCH_CNY", Amount: 50, Currency: "CNY"}

	r1, err := svc.Accept(ctx, req)
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	balAfter1 := mustBalance(t, l, "MCH_CNY")
	t.Logf("idem=%s first  accept txn=%s replayed=%v merchantBalance=%d 判定依据=首次受理应入账",
		req.IdemKey, r1.TxnID, r1.Replayed, balAfter1)

	// 再提交 9 次，同一幂等键同一内容。
	for i := 0; i < 9; i++ {
		r, err := svc.Accept(ctx, req)
		if err != nil {
			t.Fatalf("replay #%d: %v", i+2, err)
		}
		if !r.Replayed || r.TxnID != "P-1" {
			t.Fatalf("replay #%d should replay txn P-1, got replayed=%v txn=%s", i+2, r.Replayed, r.TxnID)
		}
	}
	balAfterN := mustBalance(t, l, "MCH_CNY")
	t.Logf("idem=%s replay x10 txn=%s merchantBalance=%d 判定依据=余额与首次入账后一致(=%d)即未重复扣款",
		req.IdemKey, "P-1", balAfterN, balAfter1)
	if balAfterN != balAfter1 {
		t.Fatalf("idempotent replay changed balance: before=%d after=%d", balAfter1, balAfterN)
	}
	if balAfterN != 50 {
		t.Fatalf("expected merchant balance 50, got %d", balAfterN)
	}
}

// TestAccept_SameKeyDifferentContent 同一幂等键携带不同内容必须明确拒绝。
func TestAccept_SameKeyDifferentContent(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	key := "IDEM-CONFLICT-1"

	r, err := svc.Accept(ctx, Request{IdemKey: key, TxnID: "P-2", MerchantAcct: "MCH_CNY", Amount: 10, Currency: "CNY"})
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}

	mutated := Request{IdemKey: key, TxnID: "P-2", MerchantAcct: "MCH_CNY", Amount: 11, Currency: "CNY"}
	_, err = svc.Accept(ctx, mutated)
	if codeOf(err) != apperr.CodeIdemConflict {
		t.Fatalf("idem=%s 判定依据=金额从10变为11, 期望 IDEM_CONFLICT, got %v", key, err)
	}
	t.Logf("idem=%s different amount(10->11) rejected code=IDEM_CONFLICT 判定依据=请求内容hash不一致", key)

	// 换交易号也是不同内容。
	_, err = svc.Accept(ctx, Request{IdemKey: key, TxnID: "P-2-X", MerchantAcct: "MCH_CNY", Amount: 10, Currency: "CNY"})
	if codeOf(err) != apperr.CodeIdemConflict {
		t.Fatalf("idem=%s 判定依据=交易号变化, 期望 IDEM_CONFLICT, got %v", key, err)
	}
	t.Logf("idem=%s different txnId rejected code=IDEM_CONFLICT txn=%s 判定依据=首次txn=%s", key, "P-2-X", r.TxnID)

	// 原内容仍可正常重放，证明冲突请求没有污染首次结果。
	rp, err := svc.Accept(ctx, Request{IdemKey: key, TxnID: "P-2", MerchantAcct: "MCH_CNY", Amount: 10, Currency: "CNY"})
	if err != nil || !rp.Replayed {
		t.Fatalf("original request should still replay: %+v %v", rp, err)
	}
}

// TestAccept_FailureCausesDistinguishable 不同失败原因必须可区分，且失败也幂等。
func TestAccept_FailureCausesDistinguishable(t *testing.T) {
	svc, l := newHarness(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		req   Request
		want  apperr.Code
		basis string
	}{
		{
			name:  "insufficient_funds",
			req:   Request{IdemKey: "K-INSUF", TxnID: "P-E1", MerchantAcct: "MCH_CNY", Amount: 1000, Currency: "CNY"},
			want:  apperr.CodeInsufficientFunds,
			basis: "收单户ACQ_CNY余额100 < 借方1000",
		},
		{
			name:  "account_not_found",
			req:   Request{IdemKey: "K-NOACCT", TxnID: "P-E2", MerchantAcct: "MISSING", Amount: 10, Currency: "CNY"},
			want:  apperr.CodeAccountNotFound,
			basis: "收款账户MISSING不存在",
		},
		{
			name:  "currency_mismatch",
			req:   Request{IdemKey: "K-CCY", TxnID: "P-E3", MerchantAcct: "MCH_CNY", Amount: 10, Currency: "USD"},
			want:  apperr.CodeCurrencyMismatch,
			basis: "账户MCH_CNY币种CNY != 请求USD",
		},
		{
			name:  "invalid_amount",
			req:   Request{IdemKey: "K-AMT", TxnID: "P-E4", MerchantAcct: "MCH_CNY", Amount: 0, Currency: "CNY"},
			want:  apperr.CodeInvalidAmount,
			basis: "金额0 <= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Accept(ctx, tc.req)
			if codeOf(err) != tc.want {
				t.Fatalf("idem=%s txn=%s 期望 %s, got %v (%s)", tc.req.IdemKey, tc.req.TxnID, tc.want, err, tc.basis)
			}
			t.Logf("idem=%s txn=%s first failure code=%s 判定依据=%s", tc.req.IdemKey, tc.req.TxnID, tc.want, tc.basis)

			// 失败重试：重放同一失败码，且不得产生交易、不得改变余额。
			obsAcct := tc.req.MerchantAcct
			if obsAcct == "MISSING" {
				obsAcct = "MCH_CNY"
			}
			balBefore := mustBalance(t, l, obsAcct)
			_, err2 := svc.Accept(ctx, tc.req)
			if codeOf(err2) != tc.want {
				t.Fatalf("idem=%s 失败重试期望同样 %s, got %v", tc.req.IdemKey, tc.want, err2)
			}
			balAfter := mustBalance(t, l, obsAcct)
			t.Logf("idem=%s txn=%s replay failure code=%s balanceBefore=%d balanceAfter=%d 判定依据=同一失败码且余额不变",
				tc.req.IdemKey, tc.req.TxnID, tc.want, balBefore, balAfter)
			if balBefore != balAfter {
				t.Fatalf("failed payment retry changed balance %d->%d", balBefore, balAfter)
			}
		})
	}
}

func mustBalance(t *testing.T, l *ledger.Service, id string) int64 {
	t.Helper()
	a, err := l.Account(id)
	if err != nil {
		t.Fatalf("get account %s: %v", id, err)
	}
	return a.Balance
}
