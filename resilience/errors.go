// Package resilience provides small, context-aware building blocks for
// protecting calls to remote or resource-constrained dependencies.
package resilience

import "errors"

var (
	ErrCircuitOpen  = errors.New("resilience: circuit open")
	ErrBulkheadFull = errors.New("resilience: bulkhead full")
)
