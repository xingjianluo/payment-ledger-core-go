package ledger

import (
	"time"

	"github.com/xingjianluo/payment-ledger-core-go/internal/domain"
	"github.com/xingjianluo/payment-ledger-core-go/internal/storage"
)

// Clock 便于测试注入时间。
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// SystemClock 默认时钟。
func SystemClock() Clock { return realClock{} }

const (
	BizDeposit    = "INITIAL_DEPOSIT"
	BizTransfer   = "TRANSFER"
	BizPayment    = "PAYMENT"
	BizSettlement = "SETTLEMENT"
	BizFee        = "SETTLEMENT_FEE"
	BizReconAdj   = "RECON_ADJUSTMENT"

	sysFloatCash    = "FLOAT_CASH"         // 资产：备付金/清算资金（入金对手科目）
	sysFeeIncome    = "FEE_INCOME"         // 收入：手续费
	sysPayable      = "PAYABLE_MERCHANT"   // 负债：待结算商户款
	sysSuspense     = "RECON_SUSPENSE"     // 负债：对账暂记应付（渠道多收/内部漏账）
	sysSuspenseRecv = "RECON_SUSPENSE_REC" // 资产：对账暂记应收（渠道少收）
)

type Service struct {
	store *storage.Store
	clock Clock
}

func New(st *storage.Store, clock Clock) *Service {
	if clock == nil {
		clock = SystemClock()
	}
	return &Service{store: st, clock: clock}
}

// OpenAccount 开立账户；system=true 时用于系统内部账户。
func (s *Service) OpenAccount(id, name string, c domain.Currency, t domain.AccountType, system bool) (*domain.Account, error) {
	if id == "" || name == "" || c == "" {
		return nil, domain.NewError(domain.ErrInvalid, "账户ID/名称/币种不能为空")
	}
	var out *domain.Account
	err := s.store.Update(func(tx *storage.Tx) error {
		if existing, err := tx.GetAccount(id); err == nil && existing != nil {
			return domain.NewError(domain.ErrAccountExists, "账户ID已存在: %s", id)
		}
		now := s.clock.Now()
		a := &domain.Account{
			ID: id, Name: name, Currency: c, Type: t,
			System: system, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.SaveAccount(a); err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

// FloatAccountID 返回指定币种的备付金资产账户ID（不创建）。
func FloatAccountID(c domain.Currency) string {
	return "SYS_" + sysFloatCash + "_" + string(c)
}

// EnsureFloatInTx 在事务内惰性获取/创建备付金资产账户。
func (s *Service) EnsureFloatInTx(tx *storage.Tx, c domain.Currency) (string, error) {
	id := FloatAccountID(c)
	if _, err := tx.GetAccount(id); err == nil {
		return id, nil
	}
	return id, s.ensureSysInTx(tx, sysFloatCash, c, domain.AccountTypeAsset, id)
}

// PayableAccountID 返回指定商户的应付款负债账户ID（不创建）。
func PayableAccountID(payeeAccountID string, c domain.Currency) string {
	return "SYS_" + sysPayable + "_" + payeeAccountID + "_" + string(c)
}

// SuspenseAccountID 返回指定币种的对账暂记应付（负债）账户ID（不创建）。
func SuspenseAccountID(c domain.Currency) string {
	return "SYS_" + sysSuspense + "_" + string(c)
}

// SuspenseReceivableID 返回指定币种的对账暂记应收（资产）账户ID（不创建）。
func SuspenseReceivableID(c domain.Currency) string {
	return "SYS_" + sysSuspenseRecv + "_" + string(c)
}

// EnsureSuspenseReceivableInTx 在事务内惰性获取/创建对账暂记应收资产账户。
func (s *Service) EnsureSuspenseReceivableInTx(tx *storage.Tx, c domain.Currency) (string, error) {
	id := SuspenseReceivableID(c)
	if _, err := tx.GetAccount(id); err == nil {
		return id, nil
	}
	return id, s.ensureSysInTx(tx, sysSuspenseRecv, c, domain.AccountTypeAsset, id)
}

func feeIncomeAccountID(c domain.Currency) string {
	return "SYS_" + sysFeeIncome + "_" + string(c)
}

// EnsurePayableInTx 在事务内惰性获取/创建商户应付款负债账户。
func (s *Service) EnsurePayableInTx(tx *storage.Tx, payeeAccountID string, c domain.Currency) (string, error) {
	id := PayableAccountID(payeeAccountID, c)
	if _, err := tx.GetAccount(id); err == nil {
		return id, nil
	}
	now := s.clock.Now()
	if err := tx.SaveAccount(&domain.Account{
		ID: id, Name: sysPayable + ":" + payeeAccountID, Currency: c,
		Type: domain.AccountTypeLiability, System: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return "", err
	}
	return id, nil
}

// EnsureSuspenseInTx 在事务内惰性获取/创建对账暂记负债账户。
func (s *Service) EnsureSuspenseInTx(tx *storage.Tx, c domain.Currency) (string, error) {
	id := SuspenseAccountID(c)
	if _, err := tx.GetAccount(id); err == nil {
		return id, nil
	}
	return id, s.ensureSysInTx(tx, sysSuspense, c, domain.AccountTypeLiability, id)
}

// EnsureFeeIncomeInTx 在事务内惰性获取/创建手续费收入账户。
func (s *Service) EnsureFeeIncomeInTx(tx *storage.Tx, c domain.Currency) (string, error) {
	id := feeIncomeAccountID(c)
	if _, err := tx.GetAccount(id); err == nil {
		return id, nil
	}
	return id, s.ensureSysInTx(tx, sysFeeIncome, c, domain.AccountTypeIncome, id)
}

func (s *Service) ensureSysInTx(tx *storage.Tx, name string, c domain.Currency, t domain.AccountType, id string) error {
	now := s.clock.Now()
	return tx.SaveAccount(&domain.Account{
		ID: id, Name: name, Currency: c, Type: t,
		System: true, CreatedAt: now, UpdatedAt: now,
	})
}

// PostInTx 在既有事务内过账一张通用复式凭证。
func (s *Service) PostInTx(tx *storage.Tx, bizType, bizID string, c domain.Currency, lines []domain.JournalLine, desc string) (*domain.Journal, error) {
	return s.postInTx(tx, bizType, bizID, c, lines, desc)
}

// Deposit 钱包入金（充值）：借 备付金资产 / 贷 钱包负债（INITIAL_DEPOSIT）。
// 平台视角：用户钱包余额是平台对用户的负债，备付金是对应资产。
func (s *Service) Deposit(accountID string, amount domain.Money, ref string) (*domain.Journal, error) {
	if amount <= 0 {
		return nil, domain.NewError(domain.ErrInvalid, "入金金额必须为正数")
	}
	var out *domain.Journal
	err := s.store.Update(func(tx *storage.Tx) error {
		acc, err := tx.GetAccount(accountID)
		if err != nil {
			return err
		}
		if acc.Type != domain.AccountTypeLiability {
			return domain.NewError(domain.ErrInvalid, "仅钱包(负债)账户允许入金: %s 类型为 %s", accountID, acc.Type)
		}
		floatID, err := s.EnsureFloatInTx(tx, acc.Currency)
		if err != nil {
			return err
		}
		lines := []domain.JournalLine{
			{AccountID: floatID, Debit: amount, Reference: ref, Note: "备付金增加"},
			{AccountID: acc.ID, Credit: amount, Reference: ref, Note: "钱包余额增加"},
		}
		j, err := s.postInTx(tx, BizDeposit, ref, acc.Currency, lines, "钱包入金 "+ref)
		if err != nil {
			return err
		}
		out = j
		return nil
	})
	return out, err
}

// Transfer 账户间资金划转（TRANSFER），整体成功或整体失败。
func (s *Service) Transfer(bizType, bizID, fromID, toID string, amount domain.Money, c domain.Currency, note string) (*domain.Journal, error) {
	if bizType == "" {
		return nil, domain.NewError(domain.ErrInvalid, "划转业务ID不能为空")
	}
	if fromID == toID {
		return nil, domain.NewError(domain.ErrInvalid, "转出与转入账户不能相同")
	}
	if amount <= 0 {
		return nil, domain.NewError(domain.ErrInvalid, "划转金额必须为正数")
	}
	var out *domain.Journal
	err := s.store.Update(func(tx *storage.Tx) error {
		j, err := s.TransferInTx(tx, bizType, bizID, fromID, toID, amount, c, note)
		if err != nil {
			return err
		}
		out = j
		return nil
	})
	return out, err
}

// TransferInTx 在既有事务内执行划转并返回凭证（供支付等上层服务复用同一原子临界区）。
func (s *Service) TransferInTx(tx *storage.Tx, bizType, bizID, fromID, toID string, amount domain.Money, c domain.Currency, note string) (*domain.Journal, error) {
	if bizType == "" || bizID == "" {
		return nil, domain.NewError(domain.ErrInvalid, "划转业务类型/业务ID不能为空")
	}
	if fromID == "" || toID == "" || fromID == toID {
		return nil, domain.NewError(domain.ErrInvalid, "转出/转入账户无效")
	}
	if amount <= 0 {
		return nil, domain.NewError(domain.ErrInvalid, "划转金额必须为正数")
	}
	from, err := tx.GetAccount(fromID)
	if err != nil {
		return nil, err
	}
	to, err := tx.GetAccount(toID)
	if err != nil {
		return nil, err
	}
	if c == "" {
		c = from.Currency
	}
	if from.Currency != c || to.Currency != c || from.Currency != to.Currency {
		return nil, domain.NewError(domain.ErrCurrencyMismatch,
			"币种不一致: 请求=%s 转出账户=%s 转入账户=%s", c, from.Currency, to.Currency)
	}
	if from.Type != to.Type {
		return nil, domain.NewError(domain.ErrInvalid,
			"划转仅支持同性质账户之间（当前 %s -> %s），跨科目过账请使用 PostJournal", from.Type, to.Type)
	}
	// 按账户正常余额方向构造分录：转出方余额减少，转入方余额增加。
	lines := buildTransferLines(from, to, amount, note)
	return s.postInTx(tx, bizType, bizID, c, lines, note)
}

// buildTransferLines 按账户正常余额方向构造"转出减少 / 转入增加"的两行分录。
func buildTransferLines(from, to *domain.Account, amount domain.Money, note string) []domain.JournalLine {
	if from.NormalBalanceSide() == "D" {
		// 资产/费用账户：借增贷减。
		return []domain.JournalLine{
			{AccountID: to.ID, Debit: amount, Note: note},
			{AccountID: from.ID, Credit: amount, Note: note},
		}
	}
	// 负债/权益/收入账户：贷增借减。
	return []domain.JournalLine{
		{AccountID: from.ID, Debit: amount, Note: note},
		{AccountID: to.ID, Credit: amount, Note: note},
	}
}

// PostJournal 过账一张通用复式凭证（对外）：校验借贷平衡、币种一致、余额非负。
func (s *Service) PostJournal(bizType, bizID string, c domain.Currency, lines []domain.JournalLine, desc string) (*domain.Journal, error) {
	return s.post(bizType, bizID, c, lines, desc)
}

// 别名，避免在签名里直接暴露 domain 类型造成的包文档割裂。
type JournalLine = domain.JournalLine

func (s *Service) post(bizType, bizID string, c domain.Currency, lines []domain.JournalLine, desc string) (*domain.Journal, error) {
	if bizType == "" || bizID == "" {
		return nil, domain.NewError(domain.ErrInvalid, "凭证业务类型/业务ID不能为空")
	}
	var out *domain.Journal
	err := s.store.Update(func(tx *storage.Tx) error {
		j, err := s.postInTx(tx, bizType, bizID, c, lines, desc)
		if err != nil {
			return err
		}
		out = j
		return nil
	})
	return out, err
}

// postInTx 在既有事务内过账。规则：
//  1. 业务键 (bizType,bizID) 唯一，重复过账返回原凭证（业务级幂等）；
//  2. 至少两行；每行借贷恰有一方为正；所有账户币种与凭证币种一致；
//  3. 借方合计 == 贷方合计（复式记账平衡）；
//  4. 按账户正常余额方向检查，过账后余额不得为负；
//  5. 全部账户与凭证在同一临界区内落库，失败整体回滚。
func (s *Service) postInTx(tx *storage.Tx, bizType, bizID string, c domain.Currency, lines []domain.JournalLine, desc string) (*domain.Journal, error) {
	if j, ok := tx.GetJournalByBiz(bizType, bizID); ok {
		return j, nil
	}
	if len(lines) < 2 {
		return nil, domain.NewError(domain.ErrUnbalancedEntry, "凭证至少需要两行分录")
	}
	var debitTotal, creditTotal domain.Money
	// 按账户聚合借方/贷方发生额。
	debitSum := map[string]domain.Money{}
	creditSum := map[string]domain.Money{}
	order := []string{}
	for _, ln := range lines {
		if ln.Debit < 0 || ln.Credit < 0 {
			return nil, domain.NewError(domain.ErrInvalid, "分录金额不能为负")
		}
		if (ln.Debit > 0) == (ln.Credit > 0) {
			return nil, domain.NewError(domain.ErrInvalid,
				"单行分录借贷必须恰有一方非零: account=%s", ln.AccountID)
		}
		debitTotal = debitTotal.Add(ln.Debit)
		creditTotal = creditTotal.Add(ln.Credit)
		if _, seen := debitSum[ln.AccountID]; !seen {
			order = append(order, ln.AccountID)
		}
		debitSum[ln.AccountID] = debitSum[ln.AccountID].Add(ln.Debit)
		creditSum[ln.AccountID] = creditSum[ln.AccountID].Add(ln.Credit)
	}
	if debitTotal != creditTotal || debitTotal <= 0 {
		return nil, domain.NewError(domain.ErrUnbalancedEntry,
			"借贷不平衡: debit=%d credit=%d", debitTotal, creditTotal)
	}

	accounts := map[string]*domain.Account{}
	for _, id := range order {
		acc, err := tx.GetAccount(id)
		if err != nil {
			return nil, err
		}
		if acc.Closed {
			return nil, domain.NewError(domain.ErrInvalid, "账户已关闭: %s", id)
		}
		if acc.Currency != c {
			return nil, domain.NewError(domain.ErrCurrencyMismatch,
				"分录账户币种与凭证不一致: account=%s %s != %s", id, acc.Currency, c)
		}
		accounts[id] = acc
	}

	// 余额约束：账户只允许在其正常余额方向上持有非负余额。
	// 即"减少侧发生额"不得超过当前余额：
	//   借方正常账户（资产/费用）：贷方发生额 <= 当前借方余额；
	//   贷方正常账户（负债/权益/收入）：借方发生额 <= 当前贷方余额。
	for _, id := range order {
		acc := accounts[id]
		var decrease domain.Money
		if acc.NormalBalanceSide() == "D" {
			decrease = creditSum[id]
		} else {
			decrease = debitSum[id]
		}
		if decrease > acc.Balance {
			return nil, domain.NewError(domain.ErrInsufficientFunds,
				"账户余额不足: account=%s 当前=%d 需扣减=%d", id, acc.Balance, decrease)
		}
	}

	j := &domain.Journal{
		ID:          tx.NextID("jrn"),
		BizType:     bizType,
		BizID:       bizID,
		Currency:    c,
		Lines:       append([]domain.JournalLine(nil), lines...),
		Description: desc,
		PostedAt:    s.clock.Now(),
	}
	if err := tx.SaveJournal(j); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	for _, id := range order {
		acc := accounts[id]
		// 余额始终以正常余额方向的净值保存。
		if acc.NormalBalanceSide() == "D" {
			acc.Balance = acc.Balance.Add(debitSum[id]).Sub(creditSum[id])
		} else {
			acc.Balance = acc.Balance.Add(creditSum[id]).Sub(debitSum[id])
		}
		acc.UpdatedAt = now
		acc.Version++
		if err := tx.SaveAccount(acc); err != nil {
			return nil, err
		}
	}
	return j, nil
}

// GetAccount 只读查询。
func (s *Service) GetAccount(id string) (*domain.Account, error) {
	var out *domain.Account
	err := s.store.View(func(tx *storage.Tx) error {
		a, err := tx.GetAccount(id)
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

// GetJournal 只读查询。
func (s *Service) GetJournal(id string) (*domain.Journal, error) {
	var out *domain.Journal
	err := s.store.View(func(tx *storage.Tx) error {
		j, err := tx.GetJournal(id)
		if err != nil {
			return err
		}
		out = j
		return nil
	})
	return out, err
}

// TrialBalance 返回各币种借方正常账户合计与贷方正常账户合计，用于自检平衡。
func (s *Service) TrialBalance() (debit map[domain.Currency]domain.Money, credit map[domain.Currency]domain.Money, err error) {
	debit = map[domain.Currency]domain.Money{}
	credit = map[domain.Currency]domain.Money{}
	err = s.store.View(func(tx *storage.Tx) error {
		accounts, err := tx.ListAccounts()
		if err != nil {
			return err
		}
		for _, a := range accounts {
			if a.NormalBalanceSide() == "D" {
				debit[a.Currency] = debit[a.Currency].Add(a.Balance)
			} else {
				credit[a.Currency] = credit[a.Currency].Add(a.Balance)
			}
		}
		return nil
	})
	return debit, credit, err
}
