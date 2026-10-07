package refund

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/settlement"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

const CNY domain.Currency = "CNY"

type fixture struct {
	st     *storage.Store
	led    *ledger.Service
	pay    *payment.Service
	refund *Service
	settle *settlement.Service
}

func harness(t *testing.T) *fixture {
	t.Helper()
	st := storage.New()
	clk := fixedClock{t: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)}
	led := ledger.New(st, clk)
	for _, id := range []string{"wallet-cust", "wallet-merch"} {
		if _, err := led.OpenAccount(id, id, CNY, domain.AccountTypeLiability, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := led.Deposit("wallet-cust", 1_000_000, "deposit-cust"); err != nil {
		t.Fatal(err)
	}
	return &fixture{
		st:     st,
		led:    led,
		pay:    payment.New(st, led, clk),
		refund: New(st, led, clk),
		settle: settlement.New(st, led, domain.FeePolicy{BPS: 60}, clk),
	}
}

// acceptPaid 受理一笔支付并返回支付号。
func (f *fixture) acceptPaid(t *testing.T, key string, amount domain.Money) string {
	t.Helper()
	res, err := f.pay.Accept(&domain.PaymentRequest{
		IdempotencyKey: key, PayerAccountID: "wallet-cust", PayeeAccountID: "wallet-merch",
		Amount: amount, Currency: CNY, Reference: "order-" + key,
	})
	if err != nil {
		t.Fatalf("受理支付失败: %v", err)
	}
	t.Logf("支付受理: 幂等键=%s 支付号=%s 金额=%d", key, res.Payment.ID, amount)
	return res.Payment.ID
}

func (f *fixture) balance(t *testing.T, id string) domain.Money {
	t.Helper()
	a, err := f.led.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func (f *fixture) refunded(t *testing.T, paymentID string) domain.Money {
	t.Helper()
	p, err := f.pay.Get(paymentID)
	if err != nil {
		t.Fatal(err)
	}
	return p.RefundedAmount
}

// 未结算支付全额退款：资金从商户应付款出，付款人钱包恢复。
func TestRefundFullBeforeSettlement(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-full", 50000)
	custBefore := f.balance(t, "wallet-cust")
	payableBefore := f.balance(t, ledger.PayableAccountID("wallet-merch", CNY))

	res, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-full-1", PaymentID: payID, Amount: 50000, Reason: "用户取消订单",
	})
	if err != nil {
		t.Fatalf("全额退款失败: %v", err)
	}
	t.Logf("退款成功: 幂等键=rfnd-full-1 退款号=%s 支付号=%s 金额=%d 凭证=%s 判定依据=支付未结算,资金出自商户应付款",
		res.Refund.ID, payID, res.Refund.Amount, res.Refund.JournalID)

	if got := f.balance(t, "wallet-cust"); got != custBefore+50000 {
		t.Fatalf("付款人余额应恢复 50000: 前=%d 后=%d", custBefore, got)
	}
	if got := f.balance(t, ledger.PayableAccountID("wallet-merch", CNY)); got != payableBefore-50000 {
		t.Fatalf("商户应付款应减少 50000: 前=%d 后=%d", payableBefore, got)
	}
	if got := f.refunded(t, payID); got != 50000 {
		t.Fatalf("支付累计退款应为 50000, 实际 %d", got)
	}
	j, err := f.led.GetJournal(res.Refund.JournalID)
	if err != nil {
		t.Fatal(err)
	}
	if j.BizType != ledger.BizRefund {
		t.Fatalf("退款凭证业务类型应为 REFUND, 实际 %s", j.BizType)
	}
}

// 多次部分退款：累计不得超过原支付金额，可退余额算得准。
func TestMultiplePartialRefundsCumulativeCap(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-partial", 10000)

	for i, amt := range []domain.Money{3000, 2000, 4000} {
		key := fmt.Sprintf("rfnd-part-%d", i)
		res, err := f.refund.Refund(&domain.RefundRequest{
			IdempotencyKey: key, PaymentID: payID, Amount: amt, Reason: "部分退款",
		})
		if err != nil {
			t.Fatalf("第 %d 笔部分退款失败: %v", i, err)
		}
		t.Logf("部分退款: 幂等键=%s 退款号=%s 金额=%d 累计已退=%d 可退余额=%d",
			key, res.Refund.ID, amt, f.refunded(t, payID), 10000-f.refunded(t, payID))
	}
	if got := f.refunded(t, payID); got != 9000 {
		t.Fatalf("累计退款应为 9000, 实际 %d", got)
	}
	// 可退余额 1000：退 1001 必须拒绝，退 1000 必须成功，再退任何金额都必须拒绝。
	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-part-over", PaymentID: payID, Amount: 1001, Reason: "超额",
	}); !domain.IsErrorCode(err, domain.ErrRefundExceeds) {
		t.Fatalf("超过可退余额应返回 ErrRefundExceeds, 实际: %v", err)
	}
	t.Logf("超额退款被拒绝: 幂等键=rfnd-part-over 金额=1001 可退余额=1000 判定=REFUND_EXCEEDS_REMAINING")
	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-part-last", PaymentID: payID, Amount: 1000, Reason: "退清",
	}); err != nil {
		t.Fatalf("退清可退余额应成功: %v", err)
	}
	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-part-zero-left", PaymentID: payID, Amount: 1, Reason: "退无可退",
	}); !domain.IsErrorCode(err, domain.ErrRefundExceeds) {
		t.Fatalf("退完后任何金额都应返回 ErrRefundExceeds, 实际: %v", err)
	}
	if got := f.refunded(t, payID); got != 10000 {
		t.Fatalf("累计退款应等于原支付金额 10000, 实际 %d", got)
	}
}

// 幂等：同键同内容重放不退两次；同键不同内容明确拒绝。
func TestRefundIdempotencyReplayAndConflict(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-idem", 8000)
	custBefore := f.balance(t, "wallet-cust")

	req := &domain.RefundRequest{IdempotencyKey: "rfnd-idem-1", PaymentID: payID, Amount: 3000, Reason: "重复提交测试"}
	r1, err := f.refund.Refund(req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := f.refund.Refund(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("幂等重放: 幂等键=rfnd-idem-1 首次退款号=%s 重放退款号=%s Replayed=%v",
		r1.Refund.ID, r2.Refund.ID, r2.Replayed)
	if !r2.Replayed || r1.Refund.ID != r2.Refund.ID {
		t.Fatalf("重复提交应返回同一退款单: %s vs %s", r1.Refund.ID, r2.Refund.ID)
	}
	if got := f.balance(t, "wallet-cust"); got != custBefore+3000 {
		t.Fatalf("重复提交不得退两次: 余额 %d, 期望 %d", got, custBefore+3000)
	}
	if got := f.refunded(t, payID); got != 3000 {
		t.Fatalf("累计退款不得重复: %d", got)
	}

	conflict := &domain.RefundRequest{IdempotencyKey: "rfnd-idem-1", PaymentID: payID, Amount: 4000, Reason: "改金额"}
	if _, err := f.refund.Refund(conflict); !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("同键不同内容应返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	t.Logf("同键不同内容被拒绝: 幂等键=rfnd-idem-1 既有金额=3000 本次金额=4000 判定=IDEMPOTENCY_CONFLICT")
}

// 失败原因可区分：支付不存在 / 金额非法 / 币种不符。
func TestRefundFailureReasonsDistinct(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-fail", 5000)

	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-no-pay", PaymentID: "pay-9999999999", Amount: 100,
	}); !domain.IsErrorCode(err, domain.ErrPaymentNotFound) {
		t.Fatalf("原支付不存在应返回 ErrPaymentNotFound, 实际: %v", err)
	}
	t.Logf("失败判定: 幂等键=rfnd-no-pay 支付号=pay-9999999999 判定=PAYMENT_NOT_FOUND")

	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-bad-amt", PaymentID: payID, Amount: 0,
	}); !domain.IsErrorCode(err, domain.ErrInvalid) {
		t.Fatalf("金额非正应返回 ErrInvalid, 实际: %v", err)
	}
	t.Logf("失败判定: 幂等键=rfnd-bad-amt 金额=0 判定=INVALID_ARGUMENT")

	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-bad-ccy", PaymentID: payID, Amount: 100, Currency: "USD",
	}); !domain.IsErrorCode(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("币种不符应返回 ErrCurrencyMismatch, 实际: %v", err)
	}
	t.Logf("失败判定: 幂等键=rfnd-bad-ccy 请求币种=USD 支付币种=CNY 判定=CURRENCY_MISMATCH")
}

// 失败后重试稳定性：失败不占用幂等键；同键同内容重试结论一致；
// 失败后同一键可携带合法内容重新发起（键未被失败污染），换新键同样能走通。
func TestRefundFailureRetryStability(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-retry", 5000)

	bad := &domain.RefundRequest{IdempotencyKey: "rfnd-retry-1", PaymentID: payID, Amount: 99999, Reason: "超额"}
	_, err1 := f.refund.Refund(bad)
	_, err2 := f.refund.Refund(bad)
	if !domain.IsErrorCode(err1, domain.ErrRefundExceeds) || !domain.IsErrorCode(err2, domain.ErrRefundExceeds) {
		t.Fatalf("同键同内容失败重试结论必须稳定: %v / %v", err1, err2)
	}
	t.Logf("失败重试稳定: 幂等键=rfnd-retry-1 两次均判定=REFUND_EXCEEDS_REMAINING（失败未占用该键）")

	// 首次未成功，键不被污染：同一键携带合法内容可正常走通。
	res, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-retry-1", PaymentID: payID, Amount: 2000, Reason: "修正金额重发",
	})
	if err != nil {
		t.Fatalf("失败后同键携带合法内容应可重新发起: %v", err)
	}
	t.Logf("失败后同键重发成功: 幂等键=rfnd-retry-1 退款号=%s 金额=%d", res.Refund.ID, res.Refund.Amount)

	// 换新幂等键继续退，同样正常走通。
	res2, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-retry-2", PaymentID: payID, Amount: 3000, Reason: "换新键重发",
	})
	if err != nil {
		t.Fatalf("换新幂等键重新发起应成功: %v", err)
	}
	t.Logf("换新键重发成功: 幂等键=rfnd-retry-2 退款号=%s 金额=%d", res2.Refund.ID, res2.Refund.Amount)
	if got := f.refunded(t, payID); got != 5000 {
		t.Fatalf("累计退款应为 5000, 实际 %d", got)
	}
}

// 并发退款：同一笔支付上多个退款请求同时压入，不超退、不丢更新。
func TestConcurrentRefundsNoOverRefund(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-conc", 100000)
	custBefore := f.balance(t, "wallet-cust")

	const n = 20
	const each = 10000 // 20 * 10000 = 200000 > 100000，必然有一部分失败
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded, exceeded int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("rfnd-conc-%d", i)
			res, err := f.refund.Refund(&domain.RefundRequest{
				IdempotencyKey: key, PaymentID: payID, Amount: each, Reason: "并发退款",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
				t.Logf("并发退款成功: 幂等键=%s 退款号=%s 金额=%d", key, res.Refund.ID, each)
			case domain.IsErrorCode(err, domain.ErrRefundExceeds):
				exceeded++
			default:
				t.Errorf("并发退款出现非预期错误: 幂等键=%s err=%v", key, err)
			}
		}(i)
	}
	wg.Wait()
	t.Logf("并发退款汇总: 成功=%d 超退拒绝=%d 判定依据=可退余额不足即拒", succeeded, exceeded)
	if succeeded != 10 || exceeded != 10 {
		t.Fatalf("应恰好 10 笔成功 10 笔拒绝, 实际 成功=%d 拒绝=%d", succeeded, exceeded)
	}
	if got := f.refunded(t, payID); got != 100000 {
		t.Fatalf("累计退款不得超退: 期望 100000, 实际 %d", got)
	}
	if got := f.balance(t, "wallet-cust"); got != custBefore+100000 {
		t.Fatalf("付款人余额与累计退款不一致（丢更新）: 期望 %d, 实际 %d", custBefore+100000, got)
	}
}

// 已结算支付的退款：资金从商户钱包出；商户余额不足时明确失败。
func TestRefundAfterSettlementFromMerchantWallet(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-settled", 20000)
	batch, err := f.settle.CreateBatch(CNY, []string{payID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.settle.SettleBatch(batch.ID); err != nil {
		t.Fatal(err)
	}
	merchBefore := f.balance(t, "wallet-merch")
	custBefore := f.balance(t, "wallet-cust")
	t.Logf("支付已结算: 支付号=%s 批次=%s 商户钱包=%d", payID, batch.ID, merchBefore)

	res, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-settled-1", PaymentID: payID, Amount: 5000, Reason: "售后退款",
	})
	if err != nil {
		t.Fatalf("已结算支付退款失败: %v", err)
	}
	t.Logf("已结算退款成功: 幂等键=rfnd-settled-1 退款号=%s 金额=%d 判定依据=支付已结算,资金出自商户钱包",
		res.Refund.ID, res.Refund.Amount)
	if got := f.balance(t, "wallet-merch"); got != merchBefore-5000 {
		t.Fatalf("商户钱包应减少 5000: 前=%d 后=%d", merchBefore, got)
	}
	if got := f.balance(t, "wallet-cust"); got != custBefore+5000 {
		t.Fatalf("付款人钱包应增加 5000: 前=%d 后=%d", custBefore, got)
	}

	// 商户钱包余额不足以覆盖剩余可退金额时：明确报余额不足。
	merchNow := f.balance(t, "wallet-merch")
	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-settled-nsf", PaymentID: payID, Amount: 15000, Reason: "商户钱不够",
	}); !domain.IsErrorCode(err, domain.ErrInsufficientFunds) {
		t.Fatalf("商户余额不足应返回 ErrInsufficientFunds(商户钱包=%d), 实际: %v", merchNow, err)
	}
	t.Logf("失败判定: 幂等键=rfnd-settled-nsf 金额=15000 商户钱包=%d 判定=INSUFFICIENT_FUNDS", merchNow)
	// 失败后一分钱都不能动。
	if got := f.balance(t, "wallet-merch"); got != merchNow {
		t.Fatalf("失败退款不得动账: 商户钱包 %d -> %d", merchNow, got)
	}
	if got := f.refunded(t, payID); got != 5000 {
		t.Fatalf("失败退款不得累计: %d", got)
	}
}

// 退款进入待结算批次后再结算：已退部分不得再结给商户；重复结算不重复过账。
func TestRefundBeforeSettleThenSettleBatch(t *testing.T) {
	f := harness(t)
	payID := f.acceptPaid(t, "pay-batch", 30000)
	batch, err := f.settle.CreateBatch(CNY, []string{payID})
	if err != nil {
		t.Fatal(err)
	}
	// 批次已建未结：退 10000。
	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-in-batch", PaymentID: payID, Amount: 10000, Reason: "批次内退款",
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("批次内退款: 幂等键=rfnd-in-batch 支付号=%s 批次=%s 退款=10000", payID, batch.ID)

	sb, err := f.settle.SettleBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 结算口径：30000 - 10000 = 20000，手续费 20000*60/10000 = 120。
	if sb.GrossAmount != 20000 || sb.FeeTotal != 120 || sb.NetAmount != 19880 {
		t.Fatalf("结算口径应扣除已退款: 毛额=%d 手续费=%d 净额=%d", sb.GrossAmount, sb.FeeTotal, sb.NetAmount)
	}
	if got := f.balance(t, "wallet-merch"); got != 19880 {
		t.Fatalf("商户实收应为 19880（已退部分不得再结）, 实际 %d", got)
	}
	t.Logf("批次结算: 批次=%s 毛额=%d 手续费=%d 净额=%d 判定依据=结算金额=原额-已退",
		batch.ID, sb.GrossAmount, sb.FeeTotal, sb.NetAmount)

	// 重复结算：幂等返回，不再过账。
	sb2, err := f.settle.SettleBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb2.SettlementJournalID != sb.SettlementJournalID || sb2.FeeJournalID != sb.FeeJournalID {
		t.Fatalf("重复结算不得产生新凭证: %+v vs %+v", sb, sb2)
	}
	if got := f.balance(t, "wallet-merch"); got != 19880 {
		t.Fatalf("重复结算不得重复入账: %d", got)
	}
	// 结算后仍可退剩余部分，资金改从商户钱包出。
	// 注意：手续费不退，商户实收 19880，剩余可退 20000 中本次先退 15000。
	if _, err := f.refund.Refund(&domain.RefundRequest{
		IdempotencyKey: "rfnd-after-settle", PaymentID: payID, Amount: 15000, Reason: "售后部分退款",
	}); err != nil {
		t.Fatalf("结算后退剩余部分应成功: %v", err)
	}
	if got := f.refunded(t, payID); got != 25000 {
		t.Fatalf("累计退款应为 25000, 实际 %d", got)
	}
	t.Logf("结算后退款: 幂等键=rfnd-after-settle 支付号=%s 累计已退=25000 可退余额=5000", payID)
}
