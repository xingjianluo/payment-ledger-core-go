package domain

import "time"

const (
	BatchStatusOpen    = "OPEN"
	BatchStatusSettled = "SETTLED"
)

// FeePolicy 手续费按基点费率计算：fee = amount * BPS / 10000（最小单位，向下取整）。
type FeePolicy struct {
	BPS int64
}

func (p FeePolicy) Fee(amount Money) Money {
	if p.BPS <= 0 || amount <= 0 {
		return 0
	}
	return amount * Money(p.BPS) / 10000
}

type SettlementBatch struct {
	ID                  string
	Currency            Currency
	Status              string // OPEN / SETTLED
	PaymentIDs          []string
	Count               int
	GrossAmount         Money
	FeeTotal            Money
	NetAmount           Money
	SettlementJournalID string
	FeeJournalID        string
	CreatedAt           time.Time
	SettledAt           *time.Time
}
