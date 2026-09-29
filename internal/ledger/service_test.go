package ledger

import (
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func newTestService(t *testing.T) (*Service, *storage.Store) {
	t.Helper()
	st := storage.New()
	return New(st, fixedClock{t: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}), st
}

func openWallet(t *testing.T, svc *Service, id string, c domain.Currency) *domain.Account {
	t.Helper()
	a, err := svc.OpenAccount(id, "wallet-"+id, c, domain.AccountTypeLiability, false)
	if err != nil {
		t.Fatalf("开立账户 %s 失败: %v", id, err)
	}
	return a
}

func TestDepositAndTransferHappyPath(t *testing.T) {
	svc, _ := newTestService(t)
	const CNY domain.Currency = "CNY"
	openWallet(t, svc, "u-alice", CNY)
	openWallet(t, svc, "u-bob", CNY)

	if _, err := svc.Deposit("u-alice", 10000, "dep1"); err != nil {
		t.Fatalf("入金失败: %v", err)
	}
	alice, _ := svc.GetAccount("u-alice")
	if alice.Balance != 10000 {
		t.Fatalf("入金后 Alice 余额期望 10000, 实际 %d", alice.Balance)
	}

	j, err := svc.Transfer(BizTransfer, "tx-1", "u-alice", "u-bob", 3000, CNY, "Alice 转给 Bob")
	if err != nil {
		t.Fatalf("划转失败: %v", err)
	}
	t.Logf("交易号 tx-1 过账凭证 %s: 借 u-bob 3000 / 贷 u-alice 3000（判定依据：借贷平衡且两账户同币种 CNY）", j.ID)

	alice, _ = svc.GetAccount("u-alice")
	bob, _ := svc.GetAccount("u-bob")
	if alice.Balance != 7000 || bob.Balance != 3000 {
		t.Fatalf("划转后余额错误: alice=%d bob=%d, 期望 7000/3000", alice.Balance, bob.Balance)
	}

	debit, credit, err := svc.TrialBalance()
	if err != nil {
		t.Fatalf("试算平衡失败: %v", err)
	}
	if debit[CNY] != credit[CNY] {
		t.Fatalf("试算不平衡: 借方合计=%d 贷方合计=%d", debit[CNY], credit[CNY])
	}
	t.Logf("试算平衡判定: 借方合计=%d == 贷方合计=%d", debit[CNY], credit[CNY])
}

func TestTransferIdempotentBizKey(t *testing.T) {
	svc, _ := newTestService(t)
	const CNY domain.Currency = "CNY"
	openWallet(t, svc, "u-a", CNY)
	openWallet(t, svc, "u-b", CNY)
	_, _ = svc.Deposit("u-a", 5000, "d1")

	j1, err := svc.Transfer(BizTransfer, "dup-1", "u-a", "u-b", 1000, CNY, "")
	if err != nil {
		t.Fatalf("首次划转失败: %v", err)
	}
	j2, err := svc.Transfer(BizTransfer, "dup-1", "u-a", "u-b", 1000, CNY, "")
	if err != nil {
		t.Fatalf("重复业务键划转应幂等返回: %v", err)
	}
	if j1.ID != j2.ID {
		t.Fatalf("重复业务键必须返回同一凭证: %s != %s", j1.ID, j2.ID)
	}
	a, _ := svc.GetAccount("u-a")
	if a.Balance != 4000 {
		t.Fatalf("幂等划转不得重复扣款: 余额=%d 期望 4000", a.Balance)
	}
	t.Logf("交易号 dup-1 重复提交判定: 返回同一凭证 %s，余额仍为 %d（未重复扣款）", j2.ID, a.Balance)
}

func TestInsufficientFundsAndCurrencyMismatch(t *testing.T) {
	svc, _ := newTestService(t)
	const CNY domain.Currency = "CNY"
	const USD domain.Currency = "USD"
	openWallet(t, svc, "u-poor", CNY)
	openWallet(t, svc, "u-rich", CNY)
	openWallet(t, svc, "u-usd", USD)
	_, _ = svc.Deposit("u-poor", 100, "d1")
	_, _ = svc.Deposit("u-rich", 10000, "d2")

	_, err := svc.Transfer(BizTransfer, "tx-nsf", "u-poor", "u-rich", 101, CNY, "")
	if !domain.IsErrorCode(err, domain.ErrInsufficientFunds) {
		t.Fatalf("余额不足应返回 ErrInsufficientFunds, 实际: %v", err)
	}
	t.Logf("交易号 tx-nsf 判定依据: 余额 100 < 需扣减 101, 错误码 ErrInsufficientFunds（%v）", err)

	_, err = svc.Transfer(BizTransfer, "tx-fx", "u-rich", "u-usd", 100, CNY, "")
	if !domain.IsErrorCode(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("币种不一致应返回 ErrCurrencyMismatch, 实际: %v", err)
	}
	t.Logf("交易号 tx-fx 判定依据: 付款账户 CNY 与收款账户 USD 币种不一致, 错误码 ErrCurrencyMismatch（%v）", err)

	// 失败划转后双方余额不得变化，且试算平衡。
	rich, _ := svc.GetAccount("u-rich")
	if rich.Balance != 10000 {
		t.Fatalf("失败划转不得改余额: rich=%d", rich.Balance)
	}
	d, c, _ := svc.TrialBalance()
	if d[CNY] != c[CNY] {
		t.Fatalf("失败后试算不平衡: %d != %d", d[CNY], c[CNY])
	}
}

func TestConcurrentTransfersNoLostUpdateNoNegative(t *testing.T) {
	svc, _ := newTestService(t)
	const CNY domain.Currency = "CNY"
	openWallet(t, svc, "u-sender", CNY)
	openWallet(t, svc, "u-recv", CNY)
	_, _ = svc.Deposit("u-sender", 100000, "d1")

	const goroutines = 50
	const perG = 10
	var wg sync.WaitGroup
	var success, failed int64
	var mu sync.Mutex
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				bizID := "cc-" + itoa(g) + "-" + itoa(i)
				_, err := svc.Transfer(BizTransfer, bizID, "u-sender", "u-recv", 100, CNY, "并发划转")
				mu.Lock()
				if err != nil {
					failed++
				} else {
					success++
				}
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()

	sender, _ := svc.GetAccount("u-sender")
	recv, _ := svc.GetAccount("u-recv")
	t.Logf("并发划转判定依据: 成功=%d 失败=%d；付款方余额=%d 收款方余额=%d（每笔 100）",
		success, failed, sender.Balance, recv.Balance)
	if sender.Balance < 0 {
		t.Fatalf("并发划转后余额不得为负: %d", sender.Balance)
	}
	if int64(recv.Balance) != success*100 {
		t.Fatalf("收款方余额丢失更新: 期望 %d 实际 %d", success*100, recv.Balance)
	}
	if int64(sender.Balance) != 100000-success*100 {
		t.Fatalf("付款方余额与成功笔数不匹配: 期望 %d 实际 %d", 100000-success*100, sender.Balance)
	}
	if success+failed != goroutines*perG {
		t.Fatalf("划转笔数统计不符: %d+%d != %d", success, failed, goroutines*perG)
	}
	d, c, _ := svc.TrialBalance()
	if d[CNY] != c[CNY] {
		t.Fatalf("并发后试算不平衡: %d != %d", d[CNY], c[CNY])
	}
}

func TestConcurrentTransfersContention(t *testing.T) {
	// 余额只够一半请求成功：验证并发下不会透支也不会多扣。
	svc, _ := newTestService(t)
	const CNY domain.Currency = "CNY"
	openWallet(t, svc, "u-s", CNY)
	openWallet(t, svc, "u-r", CNY)
	_, _ = svc.Deposit("u-s", 500, "d1")

	const n = 50
	var wg sync.WaitGroup
	var success int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := svc.Transfer(BizTransfer, "hot-"+itoa(i), "u-s", "u-r", 20, CNY, "")
			mu.Lock()
			if err == nil {
				success++
			} else if !domain.IsErrorCode(err, domain.ErrInsufficientFunds) {
				t.Errorf("非预期错误: %v", err)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	s, _ := svc.GetAccount("u-s")
	r, _ := svc.GetAccount("u-r")
	t.Logf("竞争划转判定依据: 成功=%d/50（每笔20，初始500，期望恰好25）; s=%d r=%d", success, s.Balance, r.Balance)
	if success != 25 || s.Balance != 0 || r.Balance != 500 {
		t.Fatalf("竞争场景结果错误: success=%d s=%d r=%d", success, s.Balance, r.Balance)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
