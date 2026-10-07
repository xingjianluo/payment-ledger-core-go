package payment

import (
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/ledger"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func harness(t *testing.T) (*Service, *ledger.Service, *storage.Store) {
	t.Helper()
	st := storage.New()
	clk := fixedClock{t: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}
	l := ledger.New(st, clk)
	const CNY domain.Currency = "CNY"
	if _, err := l.OpenAccount("wallet-cust", "customer-wallet", CNY, domain.AccountTypeLiability, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenAccount("wallet-merch", "merchant-wallet", CNY, domain.AccountTypeLiability, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Deposit("wallet-cust", 100000, "deposit-cust"); err != nil {
		t.Fatal(err)
	}
	return New(st, l, clk), l, st
}

func baseReq(key string) *domain.PaymentRequest {
	return &domain.PaymentRequest{
		IdempotencyKey: key,
		PayerAccountID: "wallet-cust",
		PayeeAccountID: "wallet-merch",
		Amount:         2500,
		Currency:       "CNY",
		Reference:      "order-001",
	}
}

func TestAcceptHappyPath(t *testing.T) {
	svc, l, _ := harness(t)
	res, err := svc.Accept(baseReq("key-happy"))
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if res.Replayed {
		t.Fatal("首次受理不应标记为重放")
	}
	t.Logf("幂等键 key-happy 受理成功: 支付号 %s 金额 %d 状态 %s 凭证 %s",
		res.Payment.ID, res.Payment.Amount, res.Payment.Status, res.Payment.JournalID)

	payer, _ := l.GetAccount("wallet-cust")
	if payer.Balance != 97500 {
		t.Fatalf("付款方余额期望 97500, 实际 %d", payer.Balance)
	}
	payable, _ := l.GetAccount(ledger.PayableAccountID("wallet-merch", "CNY"))
	if payable.Balance != 2500 {
		t.Fatalf("商户应付款期望 2500, 实际 %d", payable.Balance)
	}
}

func TestIdempotentReplayNoDoubleCharge(t *testing.T) {
	svc, l, _ := harness(t)
	req := baseReq("key-replay")

	r1, err := svc.Accept(req)
	if err != nil {
		t.Fatalf("首次受理失败: %v", err)
	}
	r2, err := svc.Accept(baseReq("key-replay"))
	if err != nil {
		t.Fatalf("相同内容重复提交应幂等通过: %v", err)
	}
	r3, err := svc.Accept(baseReq("key-replay"))
	if err != nil {
		t.Fatalf("第三次重复提交应幂等通过: %v", err)
	}
	if !r2.Replayed || !r3.Replayed {
		t.Fatal("重复提交必须标记 Replayed=true")
	}
	if r1.Payment.ID != r2.Payment.ID || r2.Payment.ID != r3.Payment.ID {
		t.Fatalf("重复提交必须返回同一支付号: %s/%s/%s", r1.Payment.ID, r2.Payment.ID, r3.Payment.ID)
	}
	payer, _ := l.GetAccount("wallet-cust")
	if payer.Balance != 97500 {
		t.Fatalf("重复提交不得重复扣款: 余额=%d 期望 97500", payer.Balance)
	}
	t.Logf("幂等键 key-replay 判定依据: 三次提交均返回支付号 %s，付款方余额保持 %d（仅扣一次 2500），Replayed=%v",
		r2.Payment.ID, payer.Balance, r2.Replayed)
}

func TestIdempotencyKeyConflictDifferentContent(t *testing.T) {
	svc, l, _ := harness(t)
	if _, err := svc.Accept(baseReq("key-conflict")); err != nil {
		t.Fatalf("首次受理失败: %v", err)
	}

	// 同一幂等键，金额不同。
	diff := baseReq("key-conflict")
	diff.Amount = 9999
	_, err := svc.Accept(diff)
	if !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("同键不同内容必须返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	t.Logf("幂等键 key-conflict 判定依据: 金额 2500 vs 9999 内容指纹不一致 => 拒绝（%v）", err)

	// 付款方不同（使用一个真实但不同的账户）。
	if _, err := l.OpenAccount("wallet-other", "other-wallet", "CNY", domain.AccountTypeLiability, false); err != nil {
		t.Fatal(err)
	}
	diff2 := baseReq("key-conflict")
	diff2.PayerAccountID = "wallet-other"
	_, err = svc.Accept(diff2)
	if !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("同键不同付款方必须返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	// 币种不同。
	diff3 := baseReq("key-conflict")
	diff3.Currency = "USD"
	_, err = svc.Accept(diff3)
	if !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("同键不同币种必须返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	t.Logf("幂等键 key-conflict 再次判定: 换付款方/换币种同样以内容指纹不一致拒绝")
}

func TestAcceptFailureReasonsDistinguishable(t *testing.T) {
	svc, _, _ := harness(t)

	cases := []struct {
		name string
		req  *domain.PaymentRequest
		code domain.ErrorCode
	}{
		{"缺幂等键", &domain.PaymentRequest{PayerAccountID: "wallet-cust", PayeeAccountID: "wallet-merch", Amount: 1, Currency: "CNY"}, domain.ErrInvalid},
		{"金额非正", func() *domain.PaymentRequest { r := baseReq("k1"); r.Amount = 0; return r }(), domain.ErrInvalid},
		{"账户不存在", func() *domain.PaymentRequest { r := baseReq("k2"); r.PayeeAccountID = "nope"; return r }(), domain.ErrAccountNotFound}, {"币种不一致", func() *domain.PaymentRequest { r := baseReq("k3"); r.Currency = "USD"; return r }(), domain.ErrCurrencyMismatch},
		{"余额不足", func() *domain.PaymentRequest { r := baseReq("k4"); r.Amount = 10_000_000; return r }(), domain.ErrInsufficientFunds},
	}
	for _, tc := range cases {
		_, err := svc.Accept(tc.req)
		if !domain.IsErrorCode(err, tc.code) {
			t.Fatalf("%s: 期望错误码 %s, 实际 %v", tc.name, tc.code, err)
		}
		t.Logf("场景[%s] 判定依据: 返回错误码 %s（%v）", tc.name, tc.code, err)
	}
}

func TestConcurrentSameIdempotencyKey(t *testing.T) {
	svc, l, _ := harness(t)
	const n = 20
	var wg sync.WaitGroup
	results := make(chan string, n)
	conflicts := make(chan string, n)
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			res, err := svc.Accept(baseReq("key-concurrent"))
			if err != nil {
				if domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
					conflicts <- err.Error()
					return
				}
				results <- "ERR:" + err.Error()
				return
			}
			results <- res.Payment.ID
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(conflicts)

	ids := map[string]int{}
	for id := range results {
		ids[id]++
	}
	cf := len(conflicts)
	if len(ids) != 1 {
		t.Fatalf("并发同键只应产生一个支付号, 实际: %v", ids)
	}
	var theID string
	for id := range ids {
		theID = id
	}
	if ids[theID]+cf != n {
		t.Fatalf("成功(含幂等返回)+冲突拒绝数应等于并发数: %d+%d != %d", ids[theID], cf, n)
	}
	payer, _ := l.GetAccount("wallet-cust")
	if payer.Balance != 97500 {
		t.Fatalf("并发同键不得重复扣款: 余额=%d 期望 97500", payer.Balance)
	}
	t.Logf("幂等键 key-concurrent 判定依据: %d 个并发请求只产生支付号 %s（成功返回 %d，冲突 %d），付款方余额 %d 仅扣一次",
		n, theID, ids[theID], cf, payer.Balance)
}

// 失败后的重试稳定性：因"说得清的原因"失败的受理，同键同内容重试结论稳定且原因一致；
// 同键不同内容明确拒绝；换新幂等键可正常走通；失败不落账。
func TestAcceptFailureRetryStability(t *testing.T) {
	svc, l, _ := harness(t)

	// 余额不足失败：钱包只有 100000。
	bad := baseReq("key-nsf")
	bad.Amount = 10_000_000
	_, err1 := svc.Accept(bad)
	if !domain.IsErrorCode(err1, domain.ErrInsufficientFunds) {
		t.Fatalf("首次应失败为 ErrInsufficientFunds, 实际: %v", err1)
	}
	// 同键同内容重试：同样的错误码与同样的原因说明（不是含糊的"处理中"冲突）。
	_, err2 := svc.Accept(bad)
	if !domain.IsErrorCode(err2, domain.ErrInsufficientFunds) {
		t.Fatalf("同键同内容重试必须稳定返回 ErrInsufficientFunds, 实际: %v", err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("同键同内容重试结论必须一致:\n首次: %v\n重试: %v", err1, err2)
	}
	t.Logf("幂等键 key-nsf 判定依据: 两次提交均稳定返回 INSUFFICIENT_FUNDS（%v），标识未被永久占住为含糊冲突", err2)

	// 同键不同内容：明确拒绝。
	diff := baseReq("key-nsf")
	diff.Amount = 100
	if _, err := svc.Accept(diff); !domain.IsErrorCode(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("同键不同内容必须返回 ErrIdempotencyConflict, 实际: %v", err)
	}
	// 换新幂等键（内容相同）：正常走通。
	ok, err := svc.Accept(baseReq("key-nsf-new"))
	if err != nil {
		t.Fatalf("换新幂等键应正常受理: %v", err)
	}
	t.Logf("幂等键 key-nsf-new 判定依据: 换键后受理成功，支付号 %s", ok.Payment.ID)

	// 账户不存在失败同样稳定。
	noAcc := baseReq("key-noacc")
	noAcc.PayeeAccountID = "nope"
	_, e1 := svc.Accept(noAcc)
	_, e2 := svc.Accept(noAcc)
	if !domain.IsErrorCode(e1, domain.ErrAccountNotFound) || !domain.IsErrorCode(e2, domain.ErrAccountNotFound) {
		t.Fatalf("账户不存在必须稳定返回 ErrAccountNotFound: %v / %v", e1, e2)
	}
	if e1.Error() != e2.Error() {
		t.Fatalf("账户不存在重试结论必须一致: %v / %v", e1, e2)
	}
	t.Logf("幂等键 key-noacc 判定依据: 两次提交均稳定返回 ACCOUNT_NOT_FOUND（%v）", e2)

	// 失败不落账：只成功了一笔 2500。
	payer, _ := l.GetAccount("wallet-cust")
	if payer.Balance != 97500 {
		t.Fatalf("失败受理不得动账: 余额=%d 期望 97500", payer.Balance)
	}
}
