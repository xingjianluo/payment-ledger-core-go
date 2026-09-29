package domain

import "time"

type IdempotencyStatus string

const (
	IdempotencyInFlight IdempotencyStatus = "IN_FLIGHT"
	IdempotencyDone     IdempotencyStatus = "DONE"
)

type IdempotencyRecord struct {
	Key         string
	Fingerprint PaymentFingerprint
	Status      IdempotencyStatus
	ResultID    string
	CreatedAt   time.Time
	ResolvedAt  *time.Time
}
