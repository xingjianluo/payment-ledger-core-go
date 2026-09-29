package reconciliation

import (
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

type fixture struct {
	st     *storage.Store
	l      *ledger.Service
	pay    *payment.Service
	settle *settlement.Service
	recon  *Service
	batch  *domain.SettlementBatch
	pays   []*domain.Payment
	now    time.Time
}

func buildFixture(t *testing.T) *fixture {
	t.Helper()
	st := storage.New()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clk := fixedClock{t: now}
	l := ledger.New(st, clk)
	paySvc := payment.New(st, l, clk)
	settleSvc := settlement.New(st, l, domain.FeePolicy{BPS: 60}, clk)
	reconSvc := New(st, l, clk)
	const CNY domain.Currency = "CNY"

	for _, id := range []string{"cust-a", "cust-b", "merch-x"} {
		if _, err := l.OpenAccount(id, id, CNY, domain.AccountTypeLiability, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Deposit("cust-a", 100000, "dep-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Deposit("cust-b", 100000, "dep-b"); err != nil {
		t.Fatal(err)
	}
	amts := []domain.Money{10000, 20000, 30000}
	var pays []*domain.Payment
	for i, amt := range amts {
		payer := "cust-a"
		if i == 2 {
			payer = "cust-b"
		}
		res, err := paySvc.Accept(&domain.PaymentRequest{
			IdempotencyKey: "recon-key-" + string(rune('a'+i)),
			PayerAccountID: payer, PayeeAccountID: "merch-x",
			Amount: amt, Currency: CNY, Reference: "ref",
		})
		if err != nil {
			t.Fatal(err)
		}
		pays = append(pays, res.Payment)
	}
	ids := []string{pays[0].ID, pays[1].ID, pays[2].ID}
	batch, err := settleSvc.CreateBatch(CNY, ids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := settleSvc.SettleBatch(batch.ID); err != nil {
		t.Fatal(err)
	}
	return &fixture{st: st, l: l, pay: paySvc, settle: settleSvc, recon: reconSvc, batch: batch, pays: pays, now: now}
}

func stmtFor(p *domain.Payment, amt domain.Money) domain.StatementRecord {
	return domain.StatementRecord{
		ExternalTxnID: "EXT-" + p.ID, TxnID: p.ID, Amount: amt,
		Currency: p.Currency, OccurredAt: time.Now(),
	}
}

func findDiff(run *domain.ReconciliationRun, typ domain.DifferenceType, txnID string) *domain.ReconciliationDifference {
	for i := range run.Differences {
		d := &run.Differences[i]
		if d.Type == typ && d.TxnID == txnID {
			return d
		}
	}
	return nil
}

func TestReconcilePerfectMatch(t *testing.T) {
	f := buildFixture(t)
	src := NewMemorySource()
	for _, p := range f.pays {
		src.Add(f.batch.ID, stmtFor(p, p.Amount))
	}
	run, err := f.recon.Reconcile(f.batch.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	if run.MatchedCount != 3 || len(run.Differences) != 0 {
		t.Fatalf("完全匹配时期望 3 笔匹配 0 差异, 实际 matched=%d diffs=%d", run.MatchedCount, len(run.Differences))
	}
	t.Logf("批次 %s 正常路径判定: 内部 %d 笔与对账单 %d 笔全部匹配，无差异、无调整凭证",
		f.batch.ID, run.InternalCount, run.StatementCount)
}

func TestReconcileAllDifferenceTypes(t *testing.T) {
	f := buildFixture(t)
	p1, p2, p3 := f.pays[0], f.pays[1], f.pays[2]
	src := NewMemorySource()
	// p1 对账单缺失（不出现在对账单里）。
	// p2 金额不符：内部 20000，对账单 19000（渠道少收 1000）。
	src.Add(f.batch.ID, stmtFor(p2, 19000))
	// p3 正常匹配。
	src.Add(f.batch.ID, stmtFor(p3, p3.Amount))
	// 对账单多出一笔内部没有的交易。
	src.Add(f.batch.ID, domain.StatementRecord{
		ExternalTxnID: "EXT-GHOST", TxnID: "pay-ghost", Amount: 777,
		Currency: "CNY", OccurredAt: f.now,
	})
	// 对账单中 p3 再出现一次 => 重复记录。
	src.Add(f.batch.ID, stmtFor(p3, p3.Amount))

	run, err := f.recon.Reconcile(f.batch.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	if run.MatchedCount != 1 {
		t.Fatalf("期望仅 1 笔匹配, 实际 %d", run.MatchedCount)
	}

	d1 := findDiff(run, domain.DiffMissingStatement, p1.ID)
	if d1 == nil || d1.AutoAdjusted {
		t.Fatalf("交易号 %s 应识别为 MISSING_IN_STATEMENT 且不自动调整", p1.ID)
	}
	t.Logf("交易号 %s 判定为单边缺失(对账单缺)：%s", p1.ID, d1.Reason)

	d2 := findDiff(run, domain.DiffAmountMismatch, p2.ID)
	if d2 == nil || !d2.AutoAdjusted || d2.JournalID == "" {
		t.Fatalf("交易号 %s 应识别为 AMOUNT_MISMATCH 并自动调整", p2.ID)
	}
	t.Logf("交易号 %s 判定为金额不符(内=%d/单=%d)：自动调整凭证 %s，%s",
		p2.ID, d2.InternalAmount, d2.StatementAmount, d2.JournalID, d2.Reason)

	d3 := findDiff(run, domain.DiffMissingInternal, "pay-ghost")
	if d3 == nil || !d3.AutoAdjusted || d3.JournalID == "" {
		t.Fatalf("pay-ghost 应识别为 MISSING_IN_INTERNAL 并自动调整")
	}
	t.Logf("交易号 pay-ghost 判定为单边缺失(内部缺)：自动调整凭证 %s，%s", d3.JournalID, d3.Reason)

	dups := 0
	for _, d := range run.Differences {
		if d.Type == domain.DiffDuplicate && d.TxnID == p3.ID {
			dups++
			if d.AutoAdjusted {
				t.Fatalf("重复记录不应自动调整: %s", d.TxnID)
			}
		}
	}
	if dups != 1 { // 第二条为重复条目，登记 1 条 DUPLICATE
		t.Fatalf("交易号 %s 重复记录应登记 1 条, 实际 %d", p3.ID, dups)
	}
	t.Logf("交易号 %s 判定为对账单重复记录 1 条（不自动调整，挂人工）", p3.ID)

	// 暂记科目：ghost 内部漏账 777 贷应付暂记；p2 渠道少收 1000 借应收暂记。
	payable, _ := f.l.GetAccount(ledger.SuspenseAccountID("CNY"))
	receivable, _ := f.l.GetAccount(ledger.SuspenseReceivableID("CNY"))
	if payable.Balance != 777 {
		t.Fatalf("暂记应付期望 777, 实际 %d", payable.Balance)
	}
	if receivable.Balance != 1000 {
		t.Fatalf("暂记应收期望 1000, 实际 %d", receivable.Balance)
	}
	t.Logf("对账后暂记科目：应付(贷)=%d，应收(借)=%d", payable.Balance, receivable.Balance)

	// 试算平衡。
	dr, cr, err := f.l.TrialBalance()
	if err != nil {
		t.Fatal(err)
	}
	if dr["CNY"] != cr["CNY"] {
		t.Fatalf("自动调整后试算不平衡: %d != %d", dr["CNY"], cr["CNY"])
	}
}

func TestReconcileRerunNoDuplicateAdjustment(t *testing.T) {
	f := buildFixture(t)
	p2 := f.pays[1]
	src := NewMemorySource()
	src.Add(f.batch.ID, stmtFor(f.pays[0], f.pays[0].Amount))
	src.Add(f.batch.ID, stmtFor(p2, 21000)) // 渠道多收 1000
	src.Add(f.batch.ID, stmtFor(f.pays[2], f.pays[2].Amount))

	run1, err := f.recon.Reconcile(f.batch.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(run1.Differences) != 1 || !run1.Differences[0].AutoAdjusted {
		t.Fatalf("首次对账应有 1 笔自动金额调整, 实际 %+v", run1.Differences)
	}
	journalID := run1.Differences[0].JournalID
	suspense1, _ := f.l.GetAccount(ledger.SuspenseAccountID("CNY"))
	float1, _ := f.l.GetAccount(ledger.FloatAccountID("CNY"))

	// 用完全相同的对账单重复执行对账。
	run2, err := f.recon.Reconcile(f.batch.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(run2.Differences) != len(run1.Differences) {
		t.Fatal("重复对账返回结果应与首次一致")
	}
	if run2.Differences[0].JournalID != journalID {
		t.Fatalf("重复对账不得产生新调整凭证: %s != %s", run2.Differences[0].JournalID, journalID)
	}
	suspense2, _ := f.l.GetAccount(ledger.SuspenseAccountID("CNY"))
	float2, _ := f.l.GetAccount(ledger.FloatAccountID("CNY"))
	if suspense1.Balance != suspense2.Balance || float1.Balance != float2.Balance {
		t.Fatalf("重复对账不得改变任何账户余额: 暂记 %d->%d 备付金 %d->%d",
			suspense1.Balance, suspense2.Balance, float1.Balance, float2.Balance)
	}
	t.Logf("批次 %s 重复对账判定依据: 复用首次运行结果，调整凭证仍为 %s，暂记余额保持 %d、备付金余额保持 %d，无新增调整",
		f.batch.ID, journalID, suspense2.Balance, float2.Balance)
}
