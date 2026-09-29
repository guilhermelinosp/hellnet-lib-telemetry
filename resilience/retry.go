package resilience

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// RetryConfig configures retry attempts and backoff behavior.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      float64
	ShouldRetry func(error) bool
	OnRetry     func(attempt int, err error)
}

// Retry retries an operation after transient failures.
//
//nolint:gocyclo // the retry state machine is intentionally explicit.
func Retry(config RetryConfig) Policy {
	return func(next Func) Func {
		return func(ctx context.Context) error {
			maxAttempts := config.MaxAttempts
			if maxAttempts <= 0 {
				maxAttempts = 1
			}
			baseDelay := config.BaseDelay
			if baseDelay <= 0 {
				baseDelay = 100 * time.Millisecond
			}
			maxDelay := config.MaxDelay
			if maxDelay <= 0 {
				maxDelay = 5 * time.Second
			}
			if maxDelay < baseDelay {
				maxDelay = baseDelay
			}
			jitter := config.Jitter
			if jitter < 0 {
				jitter = 0
			}
			if jitter > 1 {
				jitter = 1
			}

			var err error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				err = next(ctx)
				if err == nil {
					return nil
				}
				if errors.Is(err, ErrCircuitOpen) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || attempt == maxAttempts {
					return err
				}
				if config.ShouldRetry != nil && !config.ShouldRetry(err) {
					return err
				}
				if config.OnRetry != nil {
					config.OnRetry(attempt, err)
				}
				delay := retryDelay(baseDelay, maxDelay, jitter, attempt)
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return ctx.Err()
				}
			}
			return err
		}
	}
}

func retryDelay(base, max time.Duration, jitter float64, attempt int) time.Duration {
	delay := base
	for i := 1; i < attempt && delay < max; i++ {
		if delay > max/2 {
			delay = max
			break
		}
		delay *= 2
	}
	if delay > max {
		delay = max
	}
	if jitter == 0 {
		return delay
	}
	// #nosec G404 -- jitter does not require cryptographic randomness.
	return time.Duration(float64(delay) * (1 - jitter + rand.Float64()*2*jitter))
}
