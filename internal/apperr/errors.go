// Package apperr 定义账本核心各层可区分的业务错误。
package apperr

import "fmt"

// Code 为业务错误码，调用方按码区分失败原因。
type Code string

const (
	CodeOK                Code = "OK"
	CodeIdemMissing       Code = "IDEM_MISSING"          // 幂等键未登记（内部状态异常）
	CodeIdemConflict      Code = "IDEM_CONFLICT"         // 同一幂等键携带了不同请求内容
	CodeAccountNotFound   Code = "ACCOUNT_NOT_FOUND"     // 账户不存在
	CodeCurrencyMismatch  Code = "CURRENCY_MISMATCH"     // 币种不一致
	CodeInsufficientFunds Code = "INSUFFICIENT_FUNDS"    // 余额不足
	CodeDuplicateTxn      Code = "DUPLICATE_TRANSACTION" // 交易号重复
	CodeTxnNotFound       Code = "TRANSACTION_NOT_FOUND" // 交易不存在
	CodeInvalidAmount     Code = "INVALID_AMOUNT"        // 金额非法（<=0）
	CodeInvalidArgument   Code = "INVALID_ARGUMENT"      // 参数非法
	CodeBatchSettled      Code = "BATCH_SETTLED"         // 批次已结算/已对账
	CodeSettlementPending Code = "SETTLEMENT_PENDING"    // 对账要求的交易尚未结算
)

// Error 携带错误码与可读信息。
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Code) + ": " + e.Message
}

// New 构造一个业务错误。
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
