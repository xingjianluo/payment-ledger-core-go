package domain

import "time"

type AccountType string

const (
	AccountTypeAsset     AccountType = "ASSET"
	AccountTypeLiability AccountType = "LIABILITY"
	AccountTypeIncome    AccountType = "INCOME"
	AccountTypeExpense   AccountType = "EXPENSE"
	AccountTypeEquity    AccountType = "EQUITY"
)

type Account struct {
	ID       string
	Name     string
	Currency Currency
	Type     AccountType
	// Balance 以账户正常余额方向表示的有符号余额：
	// 资产/费用为借方净额，负债/权益/收入为贷方净额；不得为负。
	Balance   Money
	System    bool
	Closed    bool
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int64
}

// NormalBalanceSide 返回账户正常余额方向："D" 借方 / "C" 贷方。
func (a *Account) NormalBalanceSide() string {
	switch a.Type {
	case AccountTypeAsset, AccountTypeExpense:
		return "D"
	default:
		return "C"
	}
}
