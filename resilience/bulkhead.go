package resilience

import "context"

// Bulkhead limits concurrent execution and rejects immediately when full.
func Bulkhead(maxConcurrent int) Policy {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	semaphore := make(chan struct{}, maxConcurrent)
	return func(next Func) Func {
		return func(ctx context.Context) error {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
				return next(ctx)
			default:
				return ErrBulkheadFull
			}
		}
	}
}
