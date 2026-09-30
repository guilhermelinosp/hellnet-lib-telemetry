package resilience

import (
	"context"
	"time"
)

// Bulkhead limits concurrent execution and rejects immediately when full.
func Bulkhead(maxConcurrent int) Policy {
	return BulkheadWait(maxConcurrent, 0)
}

// BulkheadWait limits concurrent execution and waits up to maxWait for a slot.
// A non-positive maxWait preserves Bulkhead's immediate-rejection behavior.
func BulkheadWait(maxConcurrent int, maxWait time.Duration) Policy {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	semaphore := make(chan struct{}, maxConcurrent)
	return func(next Func) Func {
		return func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if maxWait > 0 {
				timer := time.NewTimer(maxWait)
				defer timer.Stop()
				select {
				case semaphore <- struct{}{}:
					defer func() { <-semaphore }()
					return next(ctx)
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return ErrBulkheadFull
				}
			}
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
