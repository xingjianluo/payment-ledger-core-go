// Package e2e_test 验证 受理 -> 划转 -> 结算 -> 对账 正常路径不回归。
package e2etest

import (
	"context"
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/reconciliation"
	"github.com/xingjianluo/payment-ledger-core-go/internal/settlement"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

func TestEndToEnd_HappyPath(t *testing.T) {
	db := storage.NewDB()
	l := ledger.New(db)

	mustOpen := func(id, ccy string, bal int64) {
		if _, err := l.OpenAccount(id, ccy, bal); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
	}
	mustOpen("ACQ", "CNY", 1_000_000)
	mustOpen("MCH", "CNY", 0)
	mustOpen("USER", "CNY", 500)
	mustOpen("FEE", "CNY", 0)
	mustOpen("SUSP", "CNY", 1_000_000)
	db.SetFeeAccount("CNY", "FEE")

	pay := payment.New(db, l, "ACQ")
	sett := settlement.New(db, l)
	rec := reconciliation.New(db, l)
	ctx := context.Background()

	// 1) 受理 2 笔（含幂等重放）。
	if _, err := pay.Accept(ctx, payment.Request{IdemKey: "K1", TxnID: "P-1", MerchantAcct: "MCH", Amount: 1000, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	rp, err := pay.Accept(ctx, payment.Request{IdemKey: "K1", TxnID: "P-1", MerchantAcct: "MCH", Amount: 1000, Currency: "CNY"})
	if err != nil || !rp.Replayed {
		t.Fatalf("幂等重放异常: %+v %v", rp, err)
	}
	if _, err := pay.Accept(ctx, payment.Request{IdemKey: "K2", TxnID: "P-2", MerchantAcct: "MCH", Amount: 400, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	if got := bal(t, l, "MCH"); got != 1400 {
		t.Fatalf("受理后商户余额期望1400, got %d", got)
	}
	t.Logf("txn=P-1/P-2 受理完成，含幂等重放，MCH=%d 判定依据=1000+400", bal(t, l, "MCH"))

	// 2) 划转 600 给用户。
	if _, err := l.Post("T-1", domain.TxnTransfer, "CNY", "", "", []domain.Entry{
		{AccountID: "MCH", Side: domain.Debit, Amount: 600},
		{AccountID: "USER", Side: domain.Credit, Amount: 600},
	}); err != nil {
		t.Fatal(err)
	}
	if got := bal(t, l, "USER"); got != 1100 {
		t.Fatalf("划转后USER期望1100, got %d", got)
	}

	// 3) 结算（固定手续费每笔 2）。
	res, err := sett.SettleBatch("B-1", []string{"P-1", "P-2"},
		map[string]domain.FeeRule{"CNY": {FeeFixed: 2}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.FeeTotals["CNY"] != 4 {
		t.Fatalf("手续费合计期望4, got %d", res.FeeTotals["CNY"])
	}
	if got := bal(t, l, "MCH"); got != 1400-600-4 {
		t.Fatalf("结算后MCH期望796, got %d", got)
	}
	t.Logf("batch=B-1 结算完成 feeTotal=%d MCH=%d USER=%d FEE=%d 判定依据=1400-600划转-4费",
		res.FeeTotals["CNY"], bal(t, l, "MCH"), bal(t, l, "USER"), bal(t, l, "FEE"))

	// 4) 对账完全相符：无差异、无调整。
	remote := []domain.ExternalRecord{
		{TxnID: "P-1", Currency: "CNY", Amount: 1000},
		{TxnID: "P-2", Currency: "CNY", Amount: 400},
	}
	rep, err := rec.Reconcile("R-1", []string{"P-1", "P-2"}, remote, "SUSP")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Diffs) != 0 || rep.Adjusted {
		t.Fatalf("完全相符应无差异: %+v", rep.Diffs)
	}
	t.Logf("recon=R-1 无差异无调整，MCH=%d SUSP=%d 判定依据=外部与本地金额完全一致", bal(t, l, "MCH"), bal(t, l, "SUSP"))

	// 5) 全账借贷守恒（资金账户总额不变，钱只在账户间移动）。
	total := bal(t, l, "ACQ") + bal(t, l, "MCH") + bal(t, l, "USER") + bal(t, l, "FEE") + bal(t, l, "SUSP")
	if total != 2_000_500 {
		t.Fatalf("全账总额期望2000500(初始1000000+500+1000000), got %d", total)
	}
	t.Logf("全账户总额=%d 判定依据=与初始注入总额相等，复式记账守恒", total)
}

func bal(t *testing.T, l *ledger.Service, id string) int64 {
	a, err := l.Account(id)
	if err != nil {
		t.Fatalf("balance %s: %v", id, err)
	}
	return a.Balance
}
