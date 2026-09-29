package domain

// Money 以整数最小单位表示金额，配合币种使用，避免浮点误差。
type Money int64

type Currency string

func (m Money) Add(o Money) Money { return m + o }
func (m Money) Sub(o Money) Money { return m - o }
func (m Money) Negative() Money   { return -m }
func (m Money) IsPositive() bool  { return m > 0 }
func (m Money) IsNegative() bool  { return m < 0 }
