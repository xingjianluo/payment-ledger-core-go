package settlement

import (
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/refund"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type env struct {
	st       *storage.Store
	l        *ledger.Service
	pay      *payment.Service
	settle   *Service
	payerIDs []string
	payees   []string
	payments []*domain.Payment
	currency domain.Currency
}

func setup(t *testing.T) *env {
	t.Helper()
	st := storage.New()
	clk := fixedClock{t: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}
	l := ledger.New(st, clk)
	paySvc := payment.New(st, l, clk)
	settleSvc := New(st, l, domain.FeePolicy{BPS: 60}, clk) // 0.60%

	const CNY domain.Currency = "CNY"
	e := &env{st: st, l: l, pay: paySvc, settle: settleSvc, currency: CNY}
	payers := []string{"w-c1", "w-c2"}
	payees := []string{"w-m1", "w-m2"}
	for _, id := range append(append([]string{}, payers...), payees...) {
		if _, err := l.OpenAccount(id, id, CNY, domain.AccountTypeLiability, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range payers {
		if _, err := l.Deposit(id, 100000, "dep-"+id); err != nil {
			t.Fatal(err)
		}
	}
	// c1 -> m1: 10000; c1 -> m2: 5000; c2 -> m1: 2000
	reqs := []struct {
		payer, payee string
		amt          domain.Money
	}{
		{"w-c1", "w-m1", 10000},
		{"w-c1", "w-m2", 5000},
		{"w-c2", "w-m1", 2000},
	}
	for i, r := range reqs {
		res, err := paySvc.Accept(&domain.PaymentRequest{
			IdempotencyKey: "pay-key-" + string(rune('a'+i)),
			PayerAccountID: r.payer, PayeeAccountID: r.payee,
			Amount: r.amt, Currency: CNY, Reference: "order",
		})
		if err != nil {
			t.Fatalf("受理第 %d 笔失败: %v", i, err)
		}
		e.payments = append(e.payments, res.Payment)
	}
	return e
}

func paymentIDs(ps []*domain.Payment) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func TestSettleBatchHappyPath(t *testing.T) {
	e := setup(t)
	batch, err := e.settle.CreateBatch("CNY", paymentIDs(e.payments))
	if err != nil {
		t.Fatalf("建批失败: %v", err)
	}
	if batch.GrossAmount != 17000 {
		t.Fatalf("总金额期望 17000, 实际 %d", batch.GrossAmount)
	}
	// 手续费逐笔计算: 10000*60bp=60; 5000*60bp=30; 2000*60bp=12 => 102
	if batch.FeeTotal != 102 {
		t.Fatalf("手续费期望 102, 实际 %d", batch.FeeTotal)
	}
	if batch.NetAmount != 16898 {
		t.Fatalf("净额期望 16898, 实际 %d", batch.NetAmount)
	}
	t.Logf("批次 %s 判定依据: 总额=%d 手续费(逐笔0.60%%)=%d 净额=%d",
		batch.ID, batch.GrossAmount, batch.FeeTotal, batch.NetAmount)

	sb, err := e.settle.SettleBatch(batch.ID)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if sb.Status != domain.BatchStatusSettled {
		t.Fatalf("结算后状态期望 SETTLED, 实际 %s", sb.Status)
	}

	m1, _ := e.l.GetAccount("w-m1")
	m2, _ := e.l.GetAccount("w-m2")
	// m1 毛 12000 费 72 => 11928; m2 毛 5000 费 30 => 4970
	if m1.Balance != 11928 {
		t.Fatalf("商户 m1 结算后净额期望 11928, 实际 %d", m1.Balance)
	}
	if m2.Balance != 4970 {
		t.Fatalf("商户 m2 结算后净额期望 4970, 实际 %d", m2.Balance)
	}
	pay1 := ledger.PayableAccountID("w-m1", "CNY")
	p1, _ := e.l.GetAccount(pay1)
	if p1.Balance != 0 {
		t.Fatalf("结算后应付款应清零, 实际 %d", p1.Balance)
	}
	for _, p := range e.payments {
		got, _ := e.pay.Get(p.ID)
		if got.Status != domain.PaymentStatusSettled {
			t.Fatalf("支付 %s 状态期望 SETTLED, 实际 %s", p.ID, got.Status)
		}
	}
	t.Logf("交易号集合 %v 结算完成: m1=%d(费72) m2=%d(费30), 应付款清零",
		paymentIDs(e.payments), m1.Balance, m2.Balance)

	d, c, err := e.l.TrialBalance()
	if err != nil {
		t.Fatal(err)
	}
	if d["CNY"] != c["CNY"] {
		t.Fatalf("结算后试算不平衡: 借 %d != 贷 %d", d["CNY"], c["CNY"])
	}
	t.Logf("结算后试算平衡: 借 %d == 贷 %d", d["CNY"], c["CNY"])
}

func TestSettleBatchIdempotent(t *testing.T) {
	e := setup(t)
	batch, err := e.settle.CreateBatch("CNY", paymentIDs(e.payments))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.settle.SettleBatch(batch.ID); err != nil {
		t.Fatal(err)
	}
	m1a, _ := e.l.GetAccount("w-m1")
	// 重复结算不得再记账、不得重复扣费。
	sb2, err := e.settle.SettleBatch(batch.ID)
	if err != nil {
		t.Fatalf("重复结算应幂等返回: %v", err)
	}
	if sb2.Status != domain.BatchStatusSettled {
		t.Fatalf("重复结算返回批次状态异常: %s", sb2.Status)
	}
	m1b, _ := e.l.GetAccount("w-m1")
	if m1a.Balance != m1b.Balance {
		t.Fatalf("重复结算不得改变余额: %d -> %d", m1a.Balance, m1b.Balance)
	}
	t.Logf("批次 %s 重复结算判定: 返回已结算批次且商户余额保持 %d，无重复凭证/扣费", batch.ID, m1b.Balance)
}

func TestCreateBatchValidation(t *testing.T) {
	e := setup(t)
	// 币种不一致。
	badCurrency := append([]string{}, paymentIDs(e.payments)...)
	_, err := e.settle.CreateBatch("USD", badCurrency)
	if !domain.IsErrorCode(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("跨币种建批应 ErrCurrencyMismatch, 实际 %v", err)
	}
	t.Logf("建批币种校验判定: USD 批次收纳 CNY 支付 => %v", err)

	batch, err := e.settle.CreateBatch("CNY", paymentIDs(e.payments))
	if err != nil {
		t.Fatal(err)
	}
	// 已归入批次的支付不得重复打包。
	_, err = e.settle.CreateBatch("CNY", paymentIDs(e.payments))
	if !domain.IsErrorCode(err, domain.ErrBatchSettled) {
		t.Fatalf("重复打包应返回 ErrBatchSettled, 实际 %v", err)
	}
	t.Logf("批次 %s 重复打包判定: 支付已归属批次 => 拒绝（%v）", batch.ID, err)
}

// 批次内支付在结算前被全额退款：结算不再产生任何分录，支付仍随批次置为已结算；
// 重复结算幂等，商户钱包分文不多。
func TestSettleBatchFullyRefundedPayment(t *testing.T) {
	e := setup(t)
	refundSvc := refund.New(e.st, e.l, fixedClock{t: time.Now()})

	// 只对 payments[0]（10000, w-c1 -> w-m1）建批，然后在结算前全额退掉。
	batch, err := e.settle.CreateBatch(e.currency, []string{e.payments[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refundSvc.Refund(&domain.RefundRequest{
		IdempotencyKey: "stl-rfnd-full", PaymentID: e.payments[0].ID, Amount: 10000, Reason: "全额退",
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("结算前全额退款: 幂等键=stl-rfnd-full 支付号=%s 批次=%s", e.payments[0].ID, batch.ID)

	merchBefore, _ := e.l.GetAccount("w-m1")
	sb, err := e.settle.SettleBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.GrossAmount != 0 || sb.FeeTotal != 0 || sb.NetAmount != 0 {
		t.Fatalf("全额退款后结算口径应全为 0: %+v", sb)
	}
	if sb.SettlementJournalID != "" || sb.FeeJournalID != "" {
		t.Fatalf("全额退款后不应产生结算/手续费凭证: %+v", sb)
	}
	merchAfter, _ := e.l.GetAccount("w-m1")
	if merchAfter.Balance != merchBefore.Balance {
		t.Fatalf("商户钱包不得变动: %d -> %d", merchBefore.Balance, merchAfter.Balance)
	}
	p, _ := e.pay.Get(e.payments[0].ID)
	if p.Status != domain.PaymentStatusSettled {
		t.Fatalf("支付仍应随批次置为 SETTLED, 实际 %s", p.Status)
	}
	t.Logf("全额退款批次结算: 批次=%s 毛额=0 无凭证 支付状态=%s 判定依据=已退部分不再结给商户",
		batch.ID, p.Status)

	// 重复结算幂等。
	if _, err := e.settle.SettleBatch(batch.ID); err != nil {
		t.Fatal(err)
	}
	merchAgain, _ := e.l.GetAccount("w-m1")
	if merchAgain.Balance != merchBefore.Balance {
		t.Fatalf("重复结算不得动账: %d", merchAgain.Balance)
	}
}
