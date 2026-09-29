// Package resilience provides small, context-aware building blocks for
// protecting calls to remote or resource-constrained dependencies.
package resilience

import "errors"

var (
	// ErrCircuitOpen indicates that the circuit breaker rejected an operation.
	ErrCircuitOpen = errors.New("resilience: circuit open")
	// ErrBulkheadFull indicates that the concurrency limit was reached.
	ErrBulkheadFull = errors.New("resilience: bulkhead full")
)
