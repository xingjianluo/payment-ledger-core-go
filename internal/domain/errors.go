package domain

import (
	"errors"
	"fmt"
)

// 错误码：对不同失败原因做可区分的分类。
type ErrorCode string

const (
	ErrInvalid             ErrorCode = "INVALID_ARGUMENT"
	ErrAccountNotFound     ErrorCode = "ACCOUNT_NOT_FOUND"
	ErrAccountExists       ErrorCode = "ACCOUNT_EXISTS"
	ErrCurrencyMismatch    ErrorCode = "CURRENCY_MISMATCH"
	ErrInsufficientFunds   ErrorCode = "INSUFFICIENT_FUNDS"
	ErrUnbalancedEntry     ErrorCode = "UNBALANCED_ENTRY"
	ErrIdempotencyConflict ErrorCode = "IDEMPOTENCY_CONFLICT"
	ErrPaymentNotFound     ErrorCode = "PAYMENT_NOT_FOUND"
	ErrPaymentDuplicate    ErrorCode = "PAYMENT_DUPLICATE"
	ErrRefundNotFound      ErrorCode = "REFUND_NOT_FOUND"
	ErrRefundExceeds       ErrorCode = "REFUND_EXCEEDS_REMAINING"
	ErrBatchNotFound       ErrorCode = "BATCH_NOT_FOUND"
	ErrBatchSettled        ErrorCode = "BATCH_ALREADY_SETTLED"
	ErrReconAlreadyRun     ErrorCode = "RECON_ALREADY_RUN"
	ErrInternal            ErrorCode = "INTERNAL"
)

// LedgerError 带错误码的领域错误，便于调用方按原因分支处理。
type LedgerError struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *LedgerError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *LedgerError) Unwrap() error { return e.Cause }

func NewError(code ErrorCode, format string, args ...any) *LedgerError {
	return &LedgerError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrapError(code ErrorCode, cause error, format string, args ...any) *LedgerError {
	return &LedgerError{Code: code, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// IsErrorCode 判断错误链中是否包含指定错误码。
func IsErrorCode(err error, code ErrorCode) bool {
	var le *LedgerError
	if errors.As(err, &le) {
		return le.Code == code
	}
	return false
}
