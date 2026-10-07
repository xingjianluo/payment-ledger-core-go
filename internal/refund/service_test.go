package refund

import (
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/reconciliation"
	"github.com/xingjianluo/payment-ledger-core-go/internal/settlement"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

const CNY domain.Currency = "CNY"

type harness struct {
	store  *storage.Store
	ledger *ledger.Service
	pay    *payment.Service
	refund *Service
	settle *settlement.Service
	recon  *reconciliation.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := storage.New()
	clk := fixedClock{t: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)}
	l := ledger.New(st, clk)
	if _, err := l.OpenAccount("wallet-cust", "customer-wallet", CNY, domain.AccountTypeLiability, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenAccount("wallet-merch", "merchant-wallet", CNY, domain.AccountTypeLiability, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Deposit("wallet-cust", 100000, "deposit-cust"); err != nil {
		t.Fatal(err)
	}
	return &harness{
		store:  st,
		ledger: l,
		pay:    payment.New(st, l, clk),
		refund: New(st, l, clk),
		settle: settlement.New(st, l, domain.FeePolicy{BPS: 60}, clk),
		recon:  reconciliation.New(st, l, clk),
	}
}

func (h *harness) accept(t *testing.T, key string, amount domain.Money) *domain.Payment {
	t.Helper()
	res, err := h.pay.Accept(&domain.PaymentRequest{
		IdempotencyKey: key,
		PayerAccountID: "wallet-cust", PayeeAccountID: "wallet-merch",
		Amount: amount, Currency: CNY, Reference: "order-" + key,
	})
	if err != nil {
		t.Fatalf("受理支付失败 key=%s: %v", key, err)
	}
	return res.Payment
}

func (h *harness) balance(t *testing.T, id string) domain.Money {
	t.Helper()
	a, err := h.ledger.GetAccount(id)
	if err != nil {
		t.Fatalf("查询账户 %s 失败: %v", id, err)
	}
	return a.Balance
}

func (h *harness) payable(t *testing.T) domain.Money {
	t.Helper()
	return h.balance(t, ledger.PayableAccountID("wallet-merch", CNY))
}

func (h *harness) assertTrialBalance(t *testing.T) {
	t.Helper()
	dr, cr, err := h.ledger.TrialBalance()
	if err != nil {
		t.Fatal(err)
	}
	for c := range dr {
		if dr[c] != cr[c] {
			t.Fatalf("试算不平衡: 币种 %s 借方 %d != 贷方 %d", c, dr[c], cr[c])
		}
	}
}

// 全额退款（未结算）：资金从商户待结算款原路退回付款方。
func TestFullRefundBeforeSettlement(t *testing.T) {
	h := newHarness(t)
	p := h.accept(t, "pay-full", 2500)

	res, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-full", PaymentID: p.ID,
		Amount: 2500, Currency: CNY, Reason: "全额退款",
	})
	if err != nil {
		t.Fatalf("全额退款失败: %v", err)
	}
	if res.Replayed {
		t.Fatal("首次退款不应标记为重放")
	}
	if got := h.balance(t, "wallet-cust"); got != 100000 {
		t.Fatalf("付款方余额期望 100000, 实际 %d", got)
	}
	if got := h.payable(t); got != 0 {
		t.Fatalf("商户待结算款期望 0, 实际 %d", got)
	}
	after, _ := h.pay.Get(p.ID)
	if after.RefundedAmount != 2500 || after.RemainingRefundable() != 0 {
		t.Fatalf("支付 %s 已退 %d 剩余可退 %d, 期望 2500/0", p.ID, after.RefundedAmount, after.RemainingRefundable())
	}
	t.Logf("退款号 %s 幂等键 ref-full 支付号 %s 判定依据: 全额退 2500，付款方余额恢复 %d，待结算款 %d，凭证 %s",
		res.Refund.ID, p.ID, h.balance(t, "wallet-cust"), h.payable(t), res.Refund.JournalID)
	h.assertTrialBalance(t)
}

// 多次部分退款：累计不得超过原支付金额。
func TestPartialRefundsCumulativeLimit(t *testing.T) {
	h := newHarness(t)
	p := h.accept(t, "pay-partial", 2500)

	for i, key := range []string{"ref-p1", "ref-p2"} {
		if _, err := h.refund.Refund(&domain.RefundRequest{
			IdempotencyKey: key, PaymentID: p.ID, Amount: 1000, Currency: CNY, Reason: "部分退款",
		}); err != nil {
			t.Fatalf("第 %d 笔部分退款失败: %v", i+1, err)
		}
	}
	mid, _ := h.pay.Get(p.ID)
	if mid.RefundedAmount != 2000 || mid.RemainingRefundable() != 500 {
		t.Fatalf("累计退款期望 2000 剩余 500, 实际 %d/%d", mid.RefundedAmount, mid.RemainingRefundable())
	}

	// 超出剩余可退：600 > 500。
	_, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-p3", PaymentID: p.ID, Amount: 600, Currency: CNY, Reason: "超额退款",
	})
	if !domain.IsErrorCode(err, domain.ErrRefundExceeds) {
		t.Fatalf("超退必须返回 ErrRefundExceeds, 实际: %v", err)
	}
	t.Logf("幂等键 ref-p3 支付号 %s 判定依据: 已退 2000 剩余 500，退 600 被拒绝（%v）", p.ID, err)

	// 退掉最后 500 后，剩余为 0，再退 1 也拒绝。
	if _, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-p4", PaymentID: p.ID, Amount: 500, Currency: CNY, Reason: "尾款退款",
	}); err != nil {
		t.Fatalf("退剩余 500 应成功: %v", err)
	}
	_, err = h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-p5", PaymentID: p.ID, Amount: 1, Currency: CNY, Reason: "超额退款",
	})
	if !domain.IsErrorCode(err, domain.ErrRefundExceeds) {
		t.Fatalf("退满后再退必须返回 ErrRefundExceeds, 实际: %v", err)
	}
	final, _ := h.pay.Get(p.ID)
	if got := h.balance(t, "wallet-cust"); got != 100000 {
		t.Fatalf("付款方余额期望 100000, 实际 %d", got)
	}
	t.Logf("支付号 %s 判定依据: 分 3 笔共退 %d（上限 %d），剩余可退 %d，付款方余额 %d",
		p.ID, final.RefundedAmount, final.Amount, final.RemainingRefundable(), h.balance(t, "wallet-cust"))
	h.assertTrialBalance(t)
}

// 退款幂等：同键同内容重放不重复退款；同键不同内容明确拒绝；支付键不能复用为退款键。
func TestRefundIdempotency(t *testing.T) {
	h := newHarness(t)
	p := h.accept(t, "pay-idem", 2500)
	req := &domain.RefundRequest{
		IdempotencyKey: "ref-idem", PaymentID: p.ID, Amount: 800, Currency: CNY, Reason: "幂等测试",
	}
	r1, err := h.refund.Refund(req)
	if err != nil {
		t.Fatalf("首次退款失败: %v", err)
	}
	r2, err := h.refund.Refund(req)
	if err != nil {
		t.Fatalf("相同内容重复提交应幂等通过: %v", err)
	}
	if !r2.Replayed || r1.Refund.ID != r2.Refund.ID {
		t.Fatalf("重复提交必须返回同一退款号: %s/%s Replayed=%v", r1.Refund.ID, r2.Refund.ID, r2.Replayed)
	}
	if got := h.balance(t, "wallet-cust"); got != 98300 {
		t.Fatalf("重复提交不得重复退款: 余额=%d 期望 98300", got)
	}
	t.Logf("幂等键 ref-idem 判定依据: 两次提交均返回退款号 %s，付款方余额 %d 仅退一次 800",
		r2.Refund.ID, h.balance(t, "wallet-cust"))

	diff := &domain.RefundRequest{
		IdempotencyKey: "ref-idem", PaymentID: p.ID, Amount: 900, Currency: CNY, Reason: "幂等测试",
	}
	if _, err := h.refund.Refund(diff); !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("同键不同金额必须返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	t.Logf("幂等键 ref-idem 判定依据: 金额 800 vs 900 指纹不一致 => 拒绝")

	// 支付的幂等键不能复用于退款（业务种类不同）。
	if _, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "pay-idem", PaymentID: p.ID, Amount: 100, Currency: CNY, Reason: "跨类复用",
	}); !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("支付键复用为退款键必须返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	t.Logf("幂等键 pay-idem 判定依据: 已用于支付受理，复用为退款键被拒绝")
	h.assertTrialBalance(t)
}

// 退款失败原因可区分。
func TestRefundFailureReasonsDistinguishable(t *testing.T) {
	h := newHarness(t)
	p := h.accept(t, "pay-fail", 2500)

	cases := []struct {
		name string
		req  *domain.RefundRequest
		code domain.ErrorCode
	}{
		{"缺幂等键", &domain.RefundRequest{PaymentID: p.ID, Amount: 1, Currency: CNY}, domain.ErrInvalid},
		{"金额非正", &domain.RefundRequest{IdempotencyKey: "rf1", PaymentID: p.ID, Amount: 0, Currency: CNY}, domain.ErrInvalid},
		{"原支付不存在", &domain.RefundRequest{IdempotencyKey: "rf2", PaymentID: "pay-nope", Amount: 1, Currency: CNY}, domain.ErrPaymentNotFound},
		{"币种不一致", &domain.RefundRequest{IdempotencyKey: "rf3", PaymentID: p.ID, Amount: 1, Currency: "USD"}, domain.ErrCurrencyMismatch},
		{"超过可退金额", &domain.RefundRequest{IdempotencyKey: "rf4", PaymentID: p.ID, Amount: 2501, Currency: CNY}, domain.ErrRefundExceeds},
	}
	for _, tc := range cases {
		_, err := h.refund.Refund(tc.req)
		if !domain.IsErrorCode(err, tc.code) {
			t.Fatalf("%s: 期望错误码 %s, 实际 %v", tc.name, tc.code, err)
		}
		t.Logf("场景[%s] 幂等键 %s 判定依据: 返回错误码 %s（%v）", tc.name, tc.req.IdempotencyKey, tc.code, err)
	}
	// 失败不应动账。
	if got := h.payable(t); got != 2500 {
		t.Fatalf("失败退款不得动账: 待结算款=%d 期望 2500", got)
	}
	h.assertTrialBalance(t)
}

// 失败后的重试稳定性：同键同内容结论稳定；同键不同内容冲突；换新键可走通。
func TestRefundFailureRetryStability(t *testing.T) {
	h := newHarness(t)
	p := h.accept(t, "pay-retry", 2500)

	bad := &domain.RefundRequest{
		IdempotencyKey: "ref-fail", PaymentID: p.ID, Amount: 3000, Currency: CNY, Reason: "超额",
	}
	_, err1 := h.refund.Refund(bad)
	if !domain.IsErrorCode(err1, domain.ErrRefundExceeds) {
		t.Fatalf("首次应失败为 ErrRefundExceeds, 实际: %v", err1)
	}
	// 同键同内容重试：同样的错误码与同样的原因说明（不是含糊的冲突错误）。
	_, err2 := h.refund.Refund(bad)
	if !domain.IsErrorCode(err2, domain.ErrRefundExceeds) {
		t.Fatalf("同键同内容重试必须稳定返回 ErrRefundExceeds, 实际: %v", err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("同键同内容重试结论必须一致:\n首次: %v\n重试: %v", err1, err2)
	}
	t.Logf("幂等键 ref-fail 判定依据: 两次提交结论一致（%v），标识未被永久占住为含糊冲突", err2)

	// 同键不同内容：明确拒绝。
	diff := &domain.RefundRequest{
		IdempotencyKey: "ref-fail", PaymentID: p.ID, Amount: 2000, Currency: CNY, Reason: "超额",
	}
	if _, err := h.refund.Refund(diff); !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("同键不同内容必须返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	// 换新幂等键：正常走通。
	ok, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-fail-new", PaymentID: p.ID, Amount: 2000, Currency: CNY, Reason: "超额",
	})
	if err != nil {
		t.Fatalf("换新幂等键应正常退款: %v", err)
	}
	t.Logf("幂等键 ref-fail-new 判定依据: 换键后退款成功，退款号 %s", ok.Refund.ID)
	h.assertTrialBalance(t)
}

// 并发退款：同一笔支付多请求并发，不超退、不丢更新。
func TestConcurrentRefundsNoOverRefund(t *testing.T) {
	h := newHarness(t)
	p := h.accept(t, "pay-conc", 2500)

	const n = 20
	const each = 500 // 20 * 500 = 10000 > 2500，只有 5 笔能成功
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	succeeded := map[string]bool{}
	exceeds := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			key := "ref-conc-" + string(rune('a'+i))
			_, err := h.refund.Refund(&domain.RefundRequest{
				IdempotencyKey: key, PaymentID: p.ID, Amount: each, Currency: CNY, Reason: "并发退款",
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded[key] = true
			} else if domain.IsErrorCode(err, domain.ErrRefundExceeds) {
				exceeds++
			} else {
				t.Errorf("并发退款出现意外错误 key=%s: %v", key, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(succeeded) != 5 || exceeds != n-5 {
		t.Fatalf("并发退款应成功 5 笔、超退拒绝 %d 笔, 实际成功 %d 拒绝 %d", n-5, len(succeeded), exceeds)
	}
	after, _ := h.pay.Get(p.ID)
	if after.RefundedAmount != 2500 || after.RemainingRefundable() != 0 {
		t.Fatalf("并发后累计退款期望 2500 剩余 0, 实际 %d/%d（超退或丢更新）",
			after.RefundedAmount, after.RemainingRefundable())
	}
	if got := h.balance(t, "wallet-cust"); got != 100000 {
		t.Fatalf("付款方余额期望 100000, 实际 %d", got)
	}
	t.Logf("支付号 %s 判定依据: %d 个并发请求各退 %d，成功 %d 笔（%v），超退拒绝 %d 笔，累计已退 %d 未超原额 %d",
		p.ID, n, each, len(succeeded), succeeded, exceeds, after.RefundedAmount, after.Amount)
	h.assertTrialBalance(t)
}

// 已结算支付的退款：资金从商户钱包出；商户余额不足时失败且结论稳定。
func TestRefundAfterSettlement(t *testing.T) {
	h := newHarness(t)
	p := h.accept(t, "pay-settled", 2500)
	batch, err := h.settle.CreateBatch(CNY, []string{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.settle.SettleBatch(batch.ID); err != nil {
		t.Fatal(err)
	}
	// 结算后商户钱包 = 2500 - 手续费 15 = 2485。
	if got := h.balance(t, "wallet-merch"); got != 2485 {
		t.Fatalf("结算后商户余额期望 2485, 实际 %d", got)
	}

	res, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-settled", PaymentID: p.ID, Amount: 1000, Currency: CNY, Reason: "售后退款",
	})
	if err != nil {
		t.Fatalf("已结算退款失败: %v", err)
	}
	if got := h.balance(t, "wallet-merch"); got != 1485 {
		t.Fatalf("已结算退款应从商户钱包出账: 商户余额期望 1485, 实际 %d", got)
	}
	if got := h.balance(t, "wallet-cust"); got != 98500 {
		t.Fatalf("付款方余额期望 98500, 实际 %d", got)
	}
	t.Logf("退款号 %s 支付号 %s 判定依据: 支付已结算，退款 1000 从商户钱包出账（商户余额 2485→%d），凭证 %s",
		res.Refund.ID, p.ID, h.balance(t, "wallet-merch"), res.Refund.JournalID)

	// 商户钱包只剩 1485，退 1500 失败：余额不足，且同键重试结论稳定。
	bad := &domain.RefundRequest{
		IdempotencyKey: "ref-nsf", PaymentID: p.ID, Amount: 1500, Currency: CNY, Reason: "大额退款",
	}
	_, err1 := h.refund.Refund(bad)
	if !domain.IsErrorCode(err1, domain.ErrInsufficientFunds) {
		t.Fatalf("商户余额不足应返回 ErrInsufficientFunds, 实际: %v", err1)
	}
	_, err2 := h.refund.Refund(bad)
	if !domain.IsErrorCode(err2, domain.ErrInsufficientFunds) || err1.Error() != err2.Error() {
		t.Fatalf("同键同内容重试结论必须稳定:\n首次: %v\n重试: %v", err1, err2)
	}
	t.Logf("幂等键 ref-nsf 判定依据: 商户余额 1485 < 1500，两次提交均稳定返回 INSUFFICIENT_FUNDS（%v）", err2)
	h.assertTrialBalance(t)
}

// 支付已进待结算批次但未结算时退款：结算时只结净额，已退部分不再结给商户；
// 之后对账口径跟随退款，退款记录被正确识别；重复结算/对账/退款不重复记账。
func TestRefundInOpenBatchThenSettleAndReconcile(t *testing.T) {
	h := newHarness(t)
	p1 := h.accept(t, "pay-b1", 2500)
	p2 := h.accept(t, "pay-b2", 3000)
	p3 := h.accept(t, "pay-b3", 1000)

	batch, err := h.settle.CreateBatch(CNY, []string{p1.ID, p2.ID, p3.ID})
	if err != nil {
		t.Fatal(err)
	}
	// 建批后、结算前：p1 部分退 1000，p3 全额退 1000。
	r1, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-b1", PaymentID: p1.ID, Amount: 1000, Currency: CNY, Reason: "批中部分退",
	})
	if err != nil {
		t.Fatal(err)
	}
	r3, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-b3", PaymentID: p3.ID, Amount: 1000, Currency: CNY, Reason: "批中全额退",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("批次 %s 结算前退款: 退款号 %s（支付 %s 退 1000）、退款号 %s（支付 %s 全额退 1000）",
		batch.ID, r1.Refund.ID, p1.ID, r3.Refund.ID, p3.ID)

	sb, err := h.settle.SettleBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 实际结算口径：p1 1500 + p2 3000 + p3 0 = 4500；手续费 9+18+0=27。
	if sb.SettledGross != 4500 || sb.SettledFee != 27 || sb.SettledNet != 4473 {
		t.Fatalf("批次实际结算口径期望 4500/27/4473, 实际 %d/%d/%d", sb.SettledGross, sb.SettledFee, sb.SettledNet)
	}
	if got := h.balance(t, "wallet-merch"); got != 4473 {
		t.Fatalf("商户钱包期望 4473（已退部分不得再结给商户）, 实际 %d", got)
	}
	if got := h.payable(t); got != 0 {
		t.Fatalf("结算后待结算款应清零, 实际 %d", got)
	}
	sp1, _ := h.pay.Get(p1.ID)
	sp3, _ := h.pay.Get(p3.ID)
	if sp1.SettledAmount != 1500 || sp3.SettledAmount != 0 {
		t.Fatalf("支付实际结算额期望 1500/0, 实际 %d/%d", sp1.SettledAmount, sp3.SettledAmount)
	}
	t.Logf("批次 %s 判定依据: 实际结算毛额 %d（原毛额 %d − 已退 2000），商户到账 %d，待结算款 %d",
		batch.ID, sb.SettledGross, sb.GrossAmount, h.balance(t, "wallet-merch"), h.payable(t))

	// 重复结算：幂等，不重复过账。
	sb2, err := h.settle.SettleBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb2.SettledGross != 4500 || h.balance(t, "wallet-merch") != 4473 {
		t.Fatal("重复结算不得重复过账")
	}

	// 对账：对账单按退款后口径（p1=1500, p2=3000），并含两条退款记录；
	// p3 全额退款无结算资金，不出现在对账单也不算差异。
	src := reconciliation.NewMemorySource()
	src.Add(batch.ID,
		domain.StatementRecord{ExternalTxnID: "EXT-B1", TxnID: p1.ID, Amount: 1500, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-B2", TxnID: p2.ID, Amount: 3000, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-R1", TxnID: r1.Refund.ID, Amount: 1000, Currency: CNY},
		domain.StatementRecord{ExternalTxnID: "EXT-R3", TxnID: r3.Refund.ID, Amount: 1000, Currency: CNY},
	)
	run, err := h.recon.Reconcile(batch.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Differences) != 0 {
		t.Fatalf("退款后对账不应有差异（退款记录须被识别）, 实际: %+v", run.Differences)
	}
	if run.MatchedCount != 4 || run.RefundCount != 2 {
		t.Fatalf("对账匹配期望 4 笔（含 2 条退款记录）, 实际匹配 %d 退款数 %d", run.MatchedCount, run.RefundCount)
	}
	t.Logf("批次 %s 对账判定依据: 内部 %d 笔 + 退款 %d 条，对账单 %d 条全部匹配（含退款号 %s/%s），差异 0",
		batch.ID, run.InternalCount, run.RefundCount, run.StatementCount, r1.Refund.ID, r3.Refund.ID)

	// 重复对账：返回首次结果，不产生新差异/调整凭证。
	run2, err := h.recon.Reconcile(batch.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	if !run2.RanAt.Equal(run.RanAt) || len(run2.Differences) != 0 {
		t.Fatal("重复对账必须复用首次结果")
	}
	// 重复退款：幂等重放，不重复出账。
	again, err := h.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "ref-b1", PaymentID: p1.ID, Amount: 1000, Currency: CNY, Reason: "批中部分退",
	})
	if err != nil || !again.Replayed || again.Refund.ID != r1.Refund.ID {
		t.Fatalf("重复退款必须幂等重放: %+v err=%v", again, err)
	}
	if got := h.balance(t, "wallet-cust"); got != 100000-6500+2000 {
		t.Fatalf("重复退款不得重复出账: 付款方余额=%d", got)
	}
	t.Logf("幂等键 ref-b1 判定依据: 重复退款返回原退款号 %s（Replayed），账户余额未再变动", again.Refund.ID)
	h.assertTrialBalance(t)
}
