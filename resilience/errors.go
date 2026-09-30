// Package resilience provides small, context-aware building blocks for
// protecting calls to remote or resource-constrained dependencies.
package resilience

import (
	"context"
	"errors"
)

var (
	// ErrCircuitOpen indicates that the circuit breaker rejected an operation.
	ErrCircuitOpen = errors.New("resilience: circuit open")
	// ErrBulkheadFull indicates that the concurrency limit was reached.
	ErrBulkheadFull = errors.New("resilience: bulkhead full")
	// ErrTimeout identifies a timeout imposed by the Timeout policy.
	ErrTimeout = errors.New("resilience: timeout")
)

type timeoutError struct{}

func (timeoutError) Error() string        { return ErrTimeout.Error() }
func (timeoutError) Unwrap() error        { return context.DeadlineExceeded }
func (timeoutError) Is(target error) bool { return target == ErrTimeout }
