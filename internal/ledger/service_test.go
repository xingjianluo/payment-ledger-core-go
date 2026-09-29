package ledger

import (
	"fmt"
	"sync"
	"testing"

	"github.com/xingjianluo/payment-ledger-core-go/internal/apperr"
	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
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

func newSvc(t *testing.T) *Service {
	t.Helper()
	return New(storage.NewDB())
}

func mustBalances(l *Service) (*domain.Account, *domain.Account) {
	a, _ := l.Account("A")
	b, _ := l.Account("B")
	return a, b
}

func transfer(l *Service, txnID string, from, to string, amt int64, ccy string) error {
	_, err := l.Post(txnID, domain.TxnTransfer, ccy, "", "", []domain.Entry{
		{AccountID: from, Side: domain.Debit, Amount: amt},
		{AccountID: to, Side: domain.Credit, Amount: amt},
	})
	return err
}

// TestPost_ConcurrentTransfers 并发划转：余额不得为负、不得丢失更新、总额守恒。
func TestPost_ConcurrentTransfers(t *testing.T) {
	l := newSvc(t)
	if _, err := l.OpenAccount("A", "CNY", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenAccount("B", "CNY", 1000); err != nil {
		t.Fatal(err)
	}

	const n = 300 // 300 笔 A->B，每笔 10；A 初始 1000，故只有 100 笔应成功
	var wg sync.WaitGroup
	var ok, rejected int64
	var mu sync.Mutex
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := transfer(l, fmt.Sprintf("T-%04d", i), "A", "B", 10, "CNY")
			mu.Lock()
			defer mu.Unlock()
			switch codeOf(err) {
			case "":
				ok++
			case apperr.CodeInsufficientFunds:
				rejected++
			default:
				t.Errorf("txn=T-%04d 非预期错误: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	a, _ := l.Account("A")
	b, _ := l.Account("B")
	t.Logf("并发划转 txns=300 成功=%d 余额不足拒绝=%d；判定依据=A余额=%d恒>=0且=1000-成功*10, B余额=%d, 合计=%d守恒=2000",
		ok, rejected, a.Balance, b.Balance, a.Balance+b.Balance)

	if a.Balance < 0 {
		t.Fatalf("A 余额为负: %d", a.Balance)
	}
	if ok != 100 || rejected != 200 {
		t.Fatalf("期望 100 成功 / 200 余额不足, 实际 %d / %d", ok, rejected)
	}
	if a.Balance != 0 || b.Balance != 2000 {
		t.Fatalf("期望 A=0 B=2000, 实际 A=%d B=%d", a.Balance, b.Balance)
	}
	if a.Balance+b.Balance != 2000 {
		t.Fatalf("借贷不平, 合计=%d", a.Balance+b.Balance)
	}
}

// TestPost_ConcurrentNoLostUpdate 双向并发划转，验证无丢失更新。
func TestPost_ConcurrentNoLostUpdate(t *testing.T) {
	l := newSvc(t)
	if _, err := l.OpenAccount("A", "CNY", 10000); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenAccount("B", "CNY", 10000); err != nil {
		t.Fatal(err)
	}

	const each = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	// A->B 与 B->A 各 each 笔，每笔 1。
	for dir := 0; dir < 2; dir++ {
		for i := 0; i < each; i++ {
			wg.Add(1)
			go func(dir, i int) {
				defer wg.Done()
				<-start
				id := fmt.Sprintf("T-%d-%04d", dir, i)
				if dir == 0 {
					_ = transfer(l, id, "A", "B", 1, "CNY")
				} else {
					_ = transfer(l, id, "B", "A", 1, "CNY")
				}
			}(dir, i)
		}
	}
	close(start)
	wg.Wait()

	a, _ := l.Account("A")
	b, _ := l.Account("B")
	t.Logf("双向并发 txn=%d 判定依据=A=%d应=10000, B=%d应=10000（流入流出抵消，无丢失更新），合计=%d",
		2*each, a.Balance, b.Balance, a.Balance+b.Balance)
	if a.Balance != 10000 || b.Balance != 10000 {
		t.Fatalf("期望 A=B=10000, 实际 A=%d B=%d", a.Balance, b.Balance)
	}
}

// TestPost_FailureAtomicityAndCauses 失败必须整体回滚，原因可区分。
func TestPost_FailureAtomicityAndCauses(t *testing.T) {
	l := newSvc(t)
	if _, err := l.OpenAccount("A", "CNY", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenAccount("B", "CNY", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenAccount("U", "USD", 0); err != nil {
		t.Fatal(err)
	}

	// 余额不足：A 借 100。
	err := transfer(l, "TX-INSUF", "A", "B", 100, "CNY")
	if codeOf(err) != apperr.CodeInsufficientFunds {
		t.Fatalf("txn=TX-INSUF 期望 INSUFFICIENT_FUNDS, got %v", err)
	}
	a, _ := l.Account("A")
	b, _ := l.Account("B")
	t.Logf("txn=TX-INSUF code=INSUFFICIENT_FUNDS 判定依据=A余额50<借100；回滚后A=%d(应50) B=%d(应0)", a.Balance, b.Balance)
	if a.Balance != 50 || b.Balance != 0 {
		t.Fatal("余额不足时不得出现半截入账")
	}
	if _, getErr := l.Post("TX-INSUF", domain.TxnTransfer, "CNY", "", "", []domain.Entry{
		{AccountID: "A", Side: domain.Debit, Amount: 1},
		{AccountID: "B", Side: domain.Credit, Amount: 1},
	}); getErr != nil {
		t.Fatalf("失败交易号未占用，改小金额后应可提交: %v", getErr)
	}
	a, b = mustBalances(l)
	t.Logf("txn=TX-INSUF 复用交易号改提1元成功；A=%d(应49) B=%d(应1)", a.Balance, b.Balance)

	// 币种不一致。
	err = transfer(l, "TX-CCY", "A", "U", 10, "CNY")
	if codeOf(err) != apperr.CodeCurrencyMismatch {
		t.Fatalf("txn=TX-CCY 期望 CURRENCY_MISMATCH, got %v", err)
	}
	t.Logf("txn=TX-CCY code=CURRENCY_MISMATCH 判定依据=U账户USD, 交易币种CNY")

	// 借贷不平。
	_, err = l.Post("TX-UNBAL", domain.TxnTransfer, "CNY", "", "", []domain.Entry{
		{AccountID: "A", Side: domain.Debit, Amount: 10},
		{AccountID: "B", Side: domain.Credit, Amount: 9},
	})
	if codeOf(err) != apperr.CodeInvalidArgument {
		t.Fatalf("txn=TX-UNBAL 期望借贷不平 INVALID_ARGUMENT, got %v", err)
	}
	t.Logf("txn=TX-UNBAL code=INVALID_ARGUMENT 判定依据=借10贷9不相等")

	// 账户不存在。
	err = transfer(l, "TX-NOACT", "A", "GHOST", 10, "CNY")
	if codeOf(err) != apperr.CodeAccountNotFound {
		t.Fatalf("txn=TX-NOACT 期望 ACCOUNT_NOT_FOUND, got %v", err)
	}
	t.Logf("txn=TX-NOACT code=ACCOUNT_NOT_FOUND 判定依据=GHOST账户不存在")

	// 交易号重复。
	if err := transfer(l, "TX-DUP", "A", "B", 10, "CNY"); err != nil {
		t.Fatal(err)
	}
	err = transfer(l, "TX-DUP", "A", "B", 10, "CNY")
	if codeOf(err) != apperr.CodeDuplicateTxn {
		t.Fatalf("txn=TX-DUP 期望 DUPLICATE_TRANSACTION, got %v", err)
	}
	t.Logf("txn=TX-DUP code=DUPLICATE_TRANSACTION 判定依据=相同交易号二次提交")
}
