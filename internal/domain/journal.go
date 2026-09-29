package domain

import "time"

// JournalLine 单行分录：Debit 与 Credit 同一行只能有一个非零。
type JournalLine struct {
	AccountID string
	Debit     Money
	Credit    Money
	Reference string
	Note      string
}

// Journal 一张记账凭证，借贷必须平衡且币种一致。
type Journal struct {
	ID          string
	BizType     string
	BizID       string
	Currency    Currency
	Lines       []JournalLine
	Description string
	PostedAt    time.Time
}
