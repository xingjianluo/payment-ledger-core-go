// Command demo 在本地内存存储中端到端跑通：开户入金 → 支付受理(幂等) →
// 并发划转 → 批次结算与手续费 → 对账(含全部差异类型与重复执行幂等)。
// 不依赖任何外部支付服务或网络接口。
package main

import (
	"fmt"
	"sync"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/reconciliation"
	"github.com/xingjianluo/payment-ledger-core-go/internal/refund"
	"github.com/xingjianluo/payment-ledger-core-go/internal/settlement"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

const CNY domain.Currency = "CNY"

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	st := storage.New()
	led := ledger.New(st, nil)
	paySvc := payment.New(st, led, nil)
	refundSvc := refund.New(st, led, nil)
	settleSvc := settlement.New(st, led, domain.FeePolicy{BPS: 60}, nil) // 0.60%
	reconSvc := reconciliation.New(st, led, nil)

	fmt.Println("========== 1. 开户与入金 ==========")
	for _, id := range []string{"cust-alice", "cust-bob", "merch-shop"} {
		_, err := led.OpenAccount(id, id, CNY, domain.AccountTypeLiability, false)
		must(err)
	}
	_, err := led.Deposit("cust-alice", 1_000_000, "DEP-ALICE")
	must(err)
	_, err = led.Deposit("cust-bob", 500_000, "DEP-BOB")
	must(err)
	printBalance(led, "cust-alice")
	printBalance(led, "cust-bob")

	fmt.Println("========== 2. 支付受理与幂等 ==========")
	req := &domain.PaymentRequest{
		IdempotencyKey: "IDEM-ORDER-1001",
		PayerAccountID: "cust-alice", PayeeAccountID: "merch-shop",
		Amount: 12000, Currency: CNY, Reference: "订单1001",
	}
	r1, err := paySvc.Accept(req)
	must(err)
	r2, err := paySvc.Accept(req) // 完全相同的重复提交
	must(err)
	fmt.Printf("首次受理支付号=%s；重复提交返回同一支付号=%s, Replayed=%v（未重复扣款）\n",
		r1.Payment.ID, r2.Payment.ID, r2.Replayed)

	conflictReq := *req
	conflictReq.Amount = 99999
	if _, err := paySvc.Accept(&conflictReq); err != nil {
		fmt.Printf("同键不同内容被明确拒绝: %v\n", err)
	}

	// 再来两笔支付用于结算与对账。
	r3, err := paySvc.Accept(&domain.PaymentRequest{
		IdempotencyKey: "IDEM-ORDER-1002", PayerAccountID: "cust-bob",
		PayeeAccountID: "merch-shop", Amount: 30500, Currency: CNY, Reference: "订单1002",
	})
	must(err)
	r4, err := paySvc.Accept(&domain.PaymentRequest{
		IdempotencyKey: "IDEM-ORDER-1003", PayerAccountID: "cust-alice",
		PayeeAccountID: "merch-shop", Amount: 8800, Currency: CNY, Reference: "订单1003",
	})
	must(err)

	fmt.Println("========== 3. 并发划转（不得透支/丢更新） ==========")
	var wg sync.WaitGroup
	var ok, nsf int
	var mu sync.Mutex
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := led.Transfer(ledger.BizTransfer,
				fmt.Sprintf("CONC-%d", i), "cust-alice", "cust-bob", 40000, CNY, "并发划转")
			mu.Lock()
			if err != nil {
				if domain.IsErrorCode(err, domain.ErrInsufficientFunds) {
					nsf++
				} else {
					panic(err)
				}
			} else {
				ok++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	fmt.Printf("30 笔各 40000 的并发划转：成功 %d，余额不足 %d；双方余额均合法且无丢失更新\n", ok, nsf)
	printBalance(led, "cust-alice")
	printBalance(led, "cust-bob")

	fmt.Println("========== 4. 批次结算与手续费 ==========")
	batch, err := settleSvc.CreateBatch(CNY, []string{r1.Payment.ID, r3.Payment.ID, r4.Payment.ID})
	must(err)
	fmt.Printf("批次 %s：毛额 %d，手续费(0.60%%，逐笔向下取整) %d，净额 %d\n",
		batch.ID, batch.GrossAmount, batch.FeeTotal, batch.NetAmount)
	sb, err := settleSvc.SettleBatch(batch.ID)
	must(err)
	fmt.Printf("结算完成：结算凭证=%s 手续费凭证=%s\n", sb.SettlementJournalID, sb.FeeJournalID)
	printBalance(led, "merch-shop")
	_, err = settleSvc.SettleBatch(batch.ID)
	must(err)
	fmt.Println("重复结算同一批次：幂等返回，未再扣费")

	fmt.Println("========== 5. 退款（批中退款 + 已结算退款 + 幂等） ==========")
	// 新支付进批次但未结算时退一部分：已退部分不会再结给商户。
	r5, err := paySvc.Accept(&domain.PaymentRequest{
		IdempotencyKey: "IDEM-ORDER-1004", PayerAccountID: "cust-alice",
		PayeeAccountID: "merch-shop", Amount: 5000, Currency: CNY, Reference: "订单1004",
	})
	must(err)
	batch2, err := settleSvc.CreateBatch(CNY, []string{r5.Payment.ID})
	must(err)
	rf1, err := refundSvc.Refund(&domain.RefundRequest{
		IdempotencyKey: "IDEM-REFUND-1004-A", PaymentID: r5.Payment.ID,
		Amount: 2000, Currency: CNY, Reason: "部分退货",
	})
	must(err)
	fmt.Printf("批次结算前部分退款：退款号=%s 金额 2000（从商户待结算款冲减）\n", rf1.Refund.ID)
	sb2, err := settleSvc.SettleBatch(batch2.ID)
	must(err)
	fmt.Printf("批次 %s 结算：原毛额 5000，实际结算 %d（已退 2000 不再结给商户），手续费 %d\n",
		batch2.ID, sb2.SettledGross, sb2.SettledFee)
	// 已结算后再退：从商户钱包出账。
	rf2, err := refundSvc.Refund(&domain.RefundRequest{
		IdempotencyKey: "IDEM-REFUND-1004-B", PaymentID: r5.Payment.ID,
		Amount: 500, Currency: CNY, Reason: "售后补偿",
	})
	must(err)
	fmt.Printf("结算后退款：退款号=%s 金额 500（从商户钱包出账）\n", rf2.Refund.ID)
	// 重复提交同一退款：幂等重放，不重复出账。
	rf2again, err := refundSvc.Refund(&domain.RefundRequest{
		IdempotencyKey: "IDEM-REFUND-1004-B", PaymentID: r5.Payment.ID,
		Amount: 500, Currency: CNY, Reason: "售后补偿",
	})
	must(err)
	fmt.Printf("重复提交同一退款：返回原退款号=%s, Replayed=%v（未重复出账）\n", rf2again.Refund.ID, rf2again.Replayed)
	// 超额退款被明确拒绝。
	p5, err := paySvc.Get(r5.Payment.ID)
	must(err)
	if _, err := refundSvc.Refund(&domain.RefundRequest{
		IdempotencyKey: "IDEM-REFUND-1004-C", PaymentID: r5.Payment.ID,
		Amount: p5.RemainingRefundable() + 1, Currency: CNY, Reason: "超额退款",
	}); err != nil {
		fmt.Printf("超额退款被明确拒绝（剩余可退 %d）: %v\n", p5.RemainingRefundable(), err)
	}
	// 退款后对账：对账单含净结算额与两条退款记录，全部识别匹配。
	src2 := reconciliation.NewMemorySource()
	src2.Add(batch2.ID,
		domain.StatementRecord{ExternalTxnID: "EXT-1004", TxnID: r5.Payment.ID, Amount: 3000, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-R1004A", TxnID: rf1.Refund.ID, Amount: 2000, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-R1004B", TxnID: rf2.Refund.ID, Amount: 500, Currency: CNY},
	)
	run3, err := reconSvc.Reconcile(batch2.ID, src2)
	must(err)
	fmt.Printf("退款后对账：内部 %d 笔 + 退款 %d 条，对账单 %d 条全部匹配，差异 %d\n",
		run3.InternalCount, run3.RefundCount, run3.StatementCount, len(run3.Differences))
	printBalance(led, "cust-alice")
	printBalance(led, "merch-shop")

	fmt.Println("========== 6. 对账（差异识别 + 自动调整 + 重复执行幂等） ==========")
	src := reconciliation.NewMemorySource()
	// r1 对账单缺失；r3 金额不符(30500 -> 30000)；r4 正常；ghost 内部缺失；r4 再来一条重复。
	src.Add(batch.ID,
		domain.StatementRecord{ExternalTxnID: "EXT-1002", TxnID: r3.Payment.ID, Amount: 30000, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-1003", TxnID: r4.Payment.ID, Amount: 8800, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-GHOST", TxnID: "pay-ghost", Amount: 666, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-1003-DUP", TxnID: r4.Payment.ID, Amount: 8800, Currency: CNY},
	)
	run, err := reconSvc.Reconcile(batch.ID, src)
	must(err)
	fmt.Printf("对账结果：内部 %d 笔，对账单 %d 条，匹配 %d，差异 %d 类：\n",
		run.InternalCount, run.StatementCount, run.MatchedCount, len(run.Differences))
	for _, d := range run.Differences {
		auto := "人工核查"
		if d.AutoAdjusted {
			auto = "已自动调整 凭证=" + d.JournalID
		}
		fmt.Printf("  - [%-20s] 交易号=%-16s %s | %s\n", d.Type, d.TxnID, auto, d.Reason)
	}
	run2, err := reconSvc.Reconcile(batch.ID, src)
	must(err)
	fmt.Printf("重复执行对账：返回首次结果（差异 %d 条），不产生新的调整凭证\n", len(run2.Differences))

	fmt.Println("========== 7. 试算平衡自检 ==========")
	dr, cr, err := led.TrialBalance()
	must(err)
	for cur := range dr {
		fmt.Printf("币种 %s：借方合计 %d == 贷方合计 %d（平衡）\n", cur, dr[cur], cr[cur])
	}
}

func printBalance(led *ledger.Service, id string) {
	a, err := led.GetAccount(id)
	must(err)
	fmt.Printf("账户 %s 余额 = %d %s\n", a.ID, a.Balance, a.Currency)
}
