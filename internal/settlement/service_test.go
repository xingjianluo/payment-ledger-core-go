package settlement

import (
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

func codeOf(err error) apperr.Code {
	if err == nil {
		return ""
	}
	if ae, ok := err.(*apperr.Error); ok {
		return ae.Code
	}
	return ""
}

type env struct {
	db  *storage.DB
	l   *ledger.Service
	pay *payment.Service
	svc *Service
}

func setup(t *testing.T) *env {
	t.Helper()
	db := storage.NewDB()
	l := ledger.New(db)
	open := func(id, ccy string, bal int64) {
		if _, err := l.OpenAccount(id, ccy, bal); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
	}
	open("ACQ_CNY", "CNY", 1_000_000)
	open("ACQ_USD", "USD", 1_000_000)
	open("MCH_CNY", "CNY", 0)
	open("MCH_USD", "USD", 0)
	open("FEE_CNY", "CNY", 0)
	open("FEE_USD", "USD", 0)
	db.SetFeeAccount("CNY", "FEE_CNY")
	db.SetFeeAccount("USD", "FEE_USD")
	return &env{db: db, l: l, pay: payment.New(db, l, "ACQ_CNY"), svc: New(db, l)}
}

func (e *env) accept(t *testing.T, key, txn, mch string, amt int64, ccy string) {
	t.Helper()
	acq := "ACQ_CNY"
	if ccy == "USD" {
		acq = "ACQ_USD"
	}
	if _, err := payment.New(e.db, e.l, acq).
		Accept(testCtx(), payment.Request{IdemKey: key, TxnID: txn, MerchantAcct: mch, Amount: amt, Currency: ccy}); err != nil {
		t.Fatalf("accept %s: %v", txn, err)
	}
}

func bal(e *env, id string) int64 {
	a, err := e.l.Account(id)
	if err != nil {
		return -1
	}
	return a.Balance
}

// TestSettleBatch_HappyPath 正常结算并按币种收取手续费。
func TestSettleBatch_HappyPath(t *testing.T) {
	e := setup(t)
	e.accept(t, "K1", "P-1", "MCH_CNY", 1000, "CNY") // fee 10bp => 1
	e.accept(t, "K2", "P-2", "MCH_CNY", 2000, "CNY") // fee => 2
	e.accept(t, "K3", "P-3", "MCH_USD", 100, "USD")  // fee => 0 (100*10/10000)

	rules := map[string]domain.FeeRule{"CNY": {FeeBps: 10}, "USD": {FeeBps: 10}}
	res, err := e.svc.SettleBatch("B1", []string{"P-1", "P-2", "P-3"}, rules, time.Now())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	t.Logf("batch=B1 txns=%v feeTotals=%v 判定依据=CNY手续费应=3(1+2), USD应=0", res.TxnIDs, res.FeeTotals)
	if res.FeeTotals["CNY"] != 3 || res.FeeTotals["USD"] != 0 {
		t.Fatalf("fee totals wrong: %v", res.FeeTotals)
	}
	if bal(e, "MCH_CNY") != 2997 || bal(e, "FEE_CNY") != 3 {
		t.Fatalf("CNY balances wrong: mch=%d fee=%d", bal(e, "MCH_CNY"), bal(e, "FEE_CNY"))
	}
	st1, _ := e.db.GetTxn("P-1")
	if st1.Status != domain.StatusSettled {
		t.Fatalf("P-1 status=%s", st1.Status)
	}
}

// TestSettleBatch_Idempotent 同一批次重复结算不得重复收取手续费。
func TestSettleBatch_Idempotent(t *testing.T) {
	e := setup(t)
	e.accept(t, "K1", "P-1", "MCH_CNY", 1000, "CNY")
	rules := map[string]domain.FeeRule{"CNY": {FeeFixed: 5}}

	if _, err := e.svc.SettleBatch("B1", []string{"P-1"}, rules, time.Now()); err != nil {
		t.Fatal(err)
	}
	feeAfter1 := bal(e, "FEE_CNY")
	mchAfter1 := bal(e, "MCH_CNY")

	_, err := e.svc.SettleBatch("B1", []string{"P-1"}, rules, time.Now())
	if codeOf(err) != apperr.CodeBatchSettled {
		t.Fatalf("batch=B1 重复结算期望 BATCH_SETTLED, got %v", err)
	}
	// 换批次号也不能重复结算同一交易。
	_, err = e.svc.SettleBatch("B2", []string{"P-1"}, rules, time.Now())
	if codeOf(err) != apperr.CodeBatchSettled {
		t.Fatalf("txn=P-1 跨批重复结算期望 BATCH_SETTLED, got %v", err)
	}
	t.Logf("batch=B1 txn=P-1 首次手续费户=%d 商户=%d；重复结算后 手续费户=%d 商户=%d 判定依据=两者不变即未重复收费",
		feeAfter1, mchAfter1, bal(e, "FEE_CNY"), bal(e, "MCH_CNY"))
	if bal(e, "FEE_CNY") != feeAfter1 || bal(e, "MCH_CNY") != mchAfter1 {
		t.Fatal("重复结算产生了额外手续费")
	}
	// 手续费交易也只应有一笔。
	if fees := e.db.ListTxnsByRef("B1"); len(fees) != 1 {
		t.Fatalf("期望批次B1仅1笔手续费交易, 实际 %d", len(fees))
	}
}

// TestSettleBatch_AtomicOnFailure 批次中任一笔失败，整批不变。
func TestSettleBatch_AtomicOnFailure(t *testing.T) {
	e := setup(t)
	e.accept(t, "K1", "P-1", "MCH_CNY", 1000, "CNY")

	// P-X 不存在，整批必须失败，P-1 不得被结算、不得扣任何费用。
	_, err := e.svc.SettleBatch("B1", []string{"P-1", "P-X"},
		map[string]domain.FeeRule{"CNY": {FeeFixed: 5}}, time.Now())
	if codeOf(err) != apperr.CodeTxnNotFound {
		t.Fatalf("期望 TRANSACTION_NOT_FOUND, got %v", err)
	}
	p1, _ := e.db.GetTxn("P-1")
	t.Logf("batch=B1(含不存在的P-X) code=TRANSACTION_NOT_FOUND 判定依据=P-1仍为%s, 商户余额=%d(应1000), 手续费户=%d(应0)",
		p1.Status, bal(e, "MCH_CNY"), bal(e, "FEE_CNY"))
	if p1.Status != domain.StatusPending {
		t.Fatalf("失败批次不得结算P-1, status=%s", p1.Status)
	}
	if bal(e, "MCH_CNY") != 1000 || bal(e, "FEE_CNY") != 0 {
		t.Fatal("失败批次产生了半截变更")
	}
	if _, ok := e.db.GetBatch("B1"); ok {
		t.Fatal("失败批次不得落库")
	}

	// 修正后同批次号可正常结算。
	if _, err := e.svc.SettleBatch("B1", []string{"P-1"},
		map[string]domain.FeeRule{"CNY": {FeeFixed: 5}}, time.Now()); err != nil {
		t.Fatalf("retry settle: %v", err)
	}
	t.Logf("batch=B1 修正后结算成功, 商户=%d(应995), 手续费户=%d(应5)", bal(e, "MCH_CNY"), bal(e, "FEE_CNY"))
}

// TestSettleBatch_FeeInsufficient 商户余额不足以承担手续费时整批失败。
func TestSettleBatch_FeeInsufficient(t *testing.T) {
	e := setup(t)
	// 直接构造一笔收款 100 的交易，手续费固定 1000，必不足。
	e.accept(t, "K1", "P-1", "MCH_CNY", 100, "CNY")
	_, err := e.svc.SettleBatch("B1", []string{"P-1"},
		map[string]domain.FeeRule{"CNY": {FeeFixed: 1000}}, time.Now())
	if codeOf(err) != apperr.CodeInsufficientFunds {
		t.Fatalf("期望 INSUFFICIENT_FUNDS(手续费), got %v", err)
	}
	p1, _ := e.db.GetTxn("P-1")
	t.Logf("batch=B1 txn=P-1 手续费1000>商户余额100 code=INSUFFICIENT_FUNDS 判定依据=P-1仍=%s 余额未变=%d",
		p1.Status, bal(e, "MCH_CNY"))
	if p1.Status != domain.StatusPending || bal(e, "MCH_CNY") != 100 {
		t.Fatal("手续费不足时不应改变状态/余额")
	}
}
