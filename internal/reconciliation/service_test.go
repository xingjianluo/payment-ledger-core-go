package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/payment"
	"github.com/xingjianluo/payment-ledger-core-go/internal/settlement"
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

type fixture struct {
	db  *storage.DB
	l   *ledger.Service
	pay *payment.Service
	svc *Service
}

func newFixture(t *testing.T, suspBalance int64) *fixture {
	t.Helper()
	db := storage.NewDB()
	l := ledger.New(db)
	open := func(id, ccy string, bal int64) {
		if _, err := l.OpenAccount(id, ccy, bal); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
	}
	open("ACQ_CNY", "CNY", 1_000_000)
	open("MCH_CNY", "CNY", 0)
	open("FEE_CNY", "CNY", 0)
	open("SUSP_CNY", "CNY", suspBalance)
	db.SetFeeAccount("CNY", "FEE_CNY")
	return &fixture{
		db:  db,
		l:   l,
		pay: payment.New(db, l, "ACQ_CNY"),
		svc: New(db, l),
	}
}

// acceptAndSettle 受理一笔支付并立即结算，返回后交易为 SETTLED。
func (f *fixture) acceptAndSettle(t *testing.T, key, txn string, amt int64) {
	t.Helper()
	if _, err := f.pay.Accept(context.Background(), payment.Request{
		IdemKey: key, TxnID: txn, MerchantAcct: "MCH_CNY", Amount: amt, Currency: "CNY",
	}); err != nil {
		t.Fatalf("accept %s: %v", txn, err)
	}
	sett := settlement.New(f.db, f.l)
	if _, err := sett.SettleBatch("SETTLE-"+txn, []string{txn},
		map[string]domain.FeeRule{"CNY": {}}, time.Now()); err != nil {
		t.Fatalf("settle %s: %v", txn, err)
	}
}

func (f *fixture) bal(id string) int64 {
	a, err := f.l.Account(id)
	if err != nil {
		return -1
	}
	return a.Balance
}

func diffCount(diffs []domain.ReconciliationDiff, typ domain.ReconDiffType) int {
	n := 0
	for _, d := range diffs {
		if d.Type == typ {
			n++
		}
	}
	return n
}

func findDiff(diffs []domain.ReconciliationDiff, txn string, typ domain.ReconDiffType) *domain.ReconciliationDiff {
	for i := range diffs {
		if diffs[i].TxnID == txn && diffs[i].Type == typ {
			return &diffs[i]
		}
	}
	return nil
}

// TestReconcile_AllDiffTypes 同时覆盖金额不符、双边单边缺失、重复记录。
func TestReconcile_AllDiffTypes(t *testing.T) {
	f := newFixture(t, 1_000_000)
	f.acceptAndSettle(t, "K1", "P-1", 1000) // 外部记成 1100 -> 金额不符
	f.acceptAndSettle(t, "K2", "P-2", 500)  // 外部缺失 -> MISSING_REMOTE
	f.acceptAndSettle(t, "K3", "P-3", 700)  // 相符
	// P-9999 仅外部存在 -> MISSING_LOCAL
	// P-3 在外部出现两次 -> DUPLICATE

	remote := []domain.ExternalRecord{
		{TxnID: "P-1", Currency: "CNY", Amount: 1100},
		{TxnID: "P-3", Currency: "CNY", Amount: 700},
		{TxnID: "P-3", Currency: "CNY", Amount: 700},
		{TxnID: "P-9999", Currency: "CNY", Amount: 300},
	}
	rep, err := f.svc.Reconcile("RC-1", []string{"P-1", "P-2", "P-3"}, remote, "SUSP_CNY")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, d := range rep.Diffs {
		t.Logf("recon=RC-1 txn=%s type=%s note=%s 判定依据=%s", d.TxnID, d.Type, d.Note, basisOf(d))
	}
	if diffCount(rep.Diffs, domain.DiffAmount) != 1 ||
		diffCount(rep.Diffs, domain.DiffMissingRemote) != 1 ||
		diffCount(rep.Diffs, domain.DiffMissingLocal) != 1 ||
		diffCount(rep.Diffs, domain.DiffDuplicate) != 1 {
		t.Fatalf("差异分类计数不符: %+v", rep.Diffs)
	}
	// 金额不符自动调整 +100：借挂账、贷商户。
	if !rep.Adjusted {
		t.Fatal("金额不符应当生成调整")
	}
	if f.bal("MCH_CNY") != 1000+500+700+100 {
		t.Fatalf("调整后商户余额应为2300, got %d", f.bal("MCH_CNY"))
	}
	if f.bal("SUSP_CNY") != 1_000_000-100 {
		t.Fatalf("挂账户应减少100, got %d", f.bal("SUSP_CNY"))
	}
	adj, _ := f.db.GetTxn("ADJ-RC-1-P-1")
	if adj == nil || adj.Type != domain.TxnAdjustment || adj.Status != domain.StatusSettled {
		t.Fatal("调整交易未按预期落库")
	}
}

func basisOf(d domain.ReconciliationDiff) string {
	switch d.Type {
	case domain.DiffAmount:
		return "本地与外部金额/币种不一致"
	case domain.DiffMissingLocal:
		return "外部账单有而本地无"
	case domain.DiffMissingRemote:
		return "本地有而外部账单无"
	case domain.DiffDuplicate:
		return "同一交易号出现多条记录"
	}
	return ""
}

// TestReconcile_IdempotentReplay 同批对账重放不得重复调整。
func TestReconcile_IdempotentReplay(t *testing.T) {
	f := newFixture(t, 1_000_000)
	f.acceptAndSettle(t, "K1", "P-1", 1000)
	remote := []domain.ExternalRecord{{TxnID: "P-1", Currency: "CNY", Amount: 1100}}

	rep1, err := f.svc.Reconcile("RC-1", []string{"P-1"}, remote, "SUSP_CNY")
	if err != nil {
		t.Fatal(err)
	}
	mch1, susp1 := f.bal("MCH_CNY"), f.bal("SUSP_CNY")
	t.Logf("recon=RC-1 txn=P-1 首次 adjusted=%v 商户=%d 挂账=%d 判定依据=金额差100已调整", rep1.Adjusted, mch1, susp1)

	for i := 0; i < 3; i++ {
		rep, err := f.svc.Reconcile("RC-1", []string{"P-1"}, remote, "SUSP_CNY")
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if rep.Adjusted {
			t.Fatalf("replay %d 不得再次调整", i)
		}
	}
	mchN, suspN := f.bal("MCH_CNY"), f.bal("SUSP_CNY")
	t.Logf("recon=RC-1 txn=P-1 重放x3 商户=%d 挂账=%d 判定依据=与首次完全一致即无重复调整; 调整交易数=%d",
		mchN, suspN, len(f.db.ListTxnsByRef("P-1")))
	if mchN != mch1 || suspN != susp1 {
		t.Fatalf("重复对账改变了余额: %d/%d -> %d/%d", mch1, susp1, mchN, suspN)
	}
	if len(f.db.ListTxnsByRef("P-1")) != 1 {
		t.Fatal("只应有一笔针对 P-1 的调整交易")
	}
}

// TestReconcile_OverlapDifferentKey 换对账键重复覆盖同一交易也必须拒绝。
func TestReconcile_OverlapDifferentKey(t *testing.T) {
	f := newFixture(t, 1_000_000)
	f.acceptAndSettle(t, "K1", "P-1", 1000)
	remote := []domain.ExternalRecord{{TxnID: "P-1", Currency: "CNY", Amount: 1000}}

	if _, err := f.svc.Reconcile("RC-1", []string{"P-1"}, remote, "SUSP_CNY"); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Reconcile("RC-2", []string{"P-1"}, remote, "SUSP_CNY")
	if codeOf(err) != apperr.CodeBatchSettled {
		t.Fatalf("txn=P-1 跨对账键重复 期望 BATCH_SETTLED, got %v", err)
	}
	t.Logf("recon=RC-2 txn=P-1 code=BATCH_SETTLED 判定依据=P-1已在RC-1覆盖, 防止跨批次重复调整")
}

// TestReconcile_NotSettledRejected 对账未结算交易应拒绝。
func TestReconcile_NotSettledRejected(t *testing.T) {
	f := newFixture(t, 1_000_000)
	// 只受理不结算。
	if _, err := f.pay.Accept(context.Background(), payment.Request{
		IdemKey: "K1", TxnID: "P-1", MerchantAcct: "MCH_CNY", Amount: 1000, Currency: "CNY",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Reconcile("RC-1", []string{"P-1"},
		[]domain.ExternalRecord{{TxnID: "P-1", Currency: "CNY", Amount: 1000}}, "SUSP_CNY")
	if codeOf(err) != apperr.CodeSettlementPending {
		t.Fatalf("期望 SETTLEMENT_PENDING, got %v", err)
	}
	t.Logf("recon=RC-1 txn=P-1 code=SETTLEMENT_PENDING 判定依据=P-1仍PENDING, 不允许对账")
}

// TestReconcile_AmountDownAdjustment 本地多记时冲减商户，余额不足则跳过调整但保留差异。
func TestReconcile_AmountDownAdjustment(t *testing.T) {
	f := newFixture(t, 1_000_000)
	f.acceptAndSettle(t, "K1", "P-1", 1000)

	// 外部金额 900：需冲减商户 100，正常。
	rep, err := f.svc.Reconcile("RC-1", []string{"P-1"},
		[]domain.ExternalRecord{{TxnID: "P-1", Currency: "CNY", Amount: 900}}, "SUSP_CNY")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Adjusted || f.bal("MCH_CNY") != 900 || f.bal("SUSP_CNY") != 1_000_100 {
		t.Fatalf("冲减调整错误: adjusted=%v mch=%d susp=%d", rep.Adjusted, f.bal("MCH_CNY"), f.bal("SUSP_CNY"))
	}
	t.Logf("recon=RC-1 txn=P-1 外部900<本地1000 adjusted=true 商户=%d(应900) 挂账=%d(应1000100) 判定依据=借商户贷挂账",
		f.bal("MCH_CNY"), f.bal("SUSP_CNY"))
}

// TestReconcile_LocalDuplicateRequest 本地重复传入同一交易号应报 DUPLICATE。
func TestReconcile_LocalDuplicateRequest(t *testing.T) {
	f := newFixture(t, 1_000_000)
	f.acceptAndSettle(t, "K1", "P-1", 1000)
	remote := []domain.ExternalRecord{{TxnID: "P-1", Currency: "CNY", Amount: 1000}}

	rep, err := f.svc.Reconcile("RC-1", []string{"P-1", "P-1"}, remote, "SUSP_CNY")
	if err != nil {
		t.Fatal(err)
	}
	if diffCount(rep.Diffs, domain.DiffDuplicate) != 1 {
		t.Fatalf("期望1条本地重复差异, got %+v", rep.Diffs)
	}
	t.Logf("recon=RC-1 txn=P-1 type=DUPLICATE 判定依据=本地对账请求中P-1出现两次")
}
