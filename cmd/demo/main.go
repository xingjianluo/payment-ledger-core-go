// Command demo 在本地内存存储中跑通受理 -> 划转 -> 结算 -> 对账全流程。
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/reconciliation"
	"github.com/xingjianluo/payment-ledger-core-go/internal/settlement"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func bal(l *ledger.Service, id string) int64 {
	a, err := l.Account(id)
	must(err)
	return a.Balance
}

func main() {
	db := storage.NewDB()
	led := ledger.New(db)

	// 账户：收单过渡户、商户、用户、手续费户、挂账户。
	_, err := led.OpenAccount("ACQ_CNY", "CNY", 1_000_000)
	must(err)
	_, err = led.OpenAccount("MCH_CNY", "CNY", 0)
	must(err)
	_, err = led.OpenAccount("USER_CNY", "CNY", 5_000)
	must(err)
	_, err = led.OpenAccount("FEE_CNY", "CNY", 0)
	must(err)
	_, err = led.OpenAccount("SUSP_CNY", "CNY", 1_000_000)
	must(err)
	db.SetFeeAccount("CNY", "FEE_CNY")

	pay := payment.New(db, led, "ACQ_CNY")
	sett := settlement.New(db, led)
	rec := reconciliation.New(db, led)

	ctx := context.Background()

	// 1) 受理两笔支付。
	r1, err := pay.Accept(ctx, payment.Request{IdemKey: "K-1001", TxnID: "P-1001", MerchantAcct: "MCH_CNY", Amount: 1000, Currency: "CNY"})
	must(err)
	fmt.Printf("pay  P-1001 accepted replayed=%v status=%s\n", r1.Replayed, r1.Status)

	r1b, err := pay.Accept(ctx, payment.Request{IdemKey: "K-1001", TxnID: "P-1001", MerchantAcct: "MCH_CNY", Amount: 1000, Currency: "CNY"})
	must(err)
	fmt.Printf("pay  P-1001 replay   replayed=%v (余额未重复增加)\n", r1b.Replayed)

	r2, err := pay.Accept(ctx, payment.Request{IdemKey: "K-1002", TxnID: "P-1002", MerchantAcct: "MCH_CNY", Amount: 500, Currency: "CNY"})
	must(err)
	_ = r2
	fmt.Printf("pay  P-1002 accepted; MCH_CNY=%d ACQ_CNY=%d\n", bal(led, "MCH_CNY"), bal(led, "ACQ_CNY"))

	// 2) 商户向用户划转 700。
	tf, err := led.Post("T-2001", domain.TxnTransfer, "CNY", "", "", []domain.Entry{
		{AccountID: "MCH_CNY", Side: domain.Debit, Amount: 700},
		{AccountID: "USER_CNY", Side: domain.Credit, Amount: 700},
	})
	must(err)
	fmt.Printf("xfer T-2001 posted status=%s; MCH_CNY=%d USER_CNY=%d\n", tf.Status, bal(led, "MCH_CNY"), bal(led, "USER_CNY"))

	// 3) 结算两笔支付（手续费 10bp = 0.1%）。
	br, err := sett.SettleBatch("BATCH-1", []string{"P-1001", "P-1002"},
		map[string]domain.FeeRule{"CNY": {FeeBps: 10}}, time.Now())
	must(err)
	fmt.Printf("sett BATCH-1 txns=%v feeTotals=%v; MCH_CNY=%d FEE_CNY=%d\n",
		br.TxnIDs, br.FeeTotals, bal(led, "MCH_CNY"), bal(led, "FEE_CNY"))

	// 4) 对账：外部账单 P-1001 金额不符（外部 1100），P-1002 相符，
	//    另含一条本地不存在的 P-9999。
	remote := []domain.ExternalRecord{
		{TxnID: "P-1001", Currency: "CNY", Amount: 1100},
		{TxnID: "P-1002", Currency: "CNY", Amount: 500},
		{TxnID: "P-9999", Currency: "CNY", Amount: 300},
	}
	rep, err := rec.Reconcile("RECON-1", []string{"P-1001", "P-1002"}, remote, "SUSP_CNY")
	must(err)
	fmt.Printf("recon RECON-1 adjusted=%v diffs=%d\n", rep.Adjusted, len(rep.Diffs))
	for _, d := range rep.Diffs {
		fmt.Printf("  diff txn=%s type=%s note=%s\n", d.TxnID, d.Type, d.Note)
	}

	// 5) 同批对账重跑：不产生重复调整。
	rep2, err := rec.Reconcile("RECON-1", []string{"P-1001", "P-1002"}, remote, "SUSP_CNY")
	must(err)
	fmt.Printf("recon RECON-1 replay adjusted=%v diffs=%d; MCH_CNY=%d SUSP_CNY=%d\n",
		rep2.Adjusted, len(rep2.Diffs), bal(led, "MCH_CNY"), bal(led, "SUSP_CNY"))
}
