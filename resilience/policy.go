package resilience

import (
	"context"
	"time"
)

// Func is an operation that can be wrapped by a resilience policy.
type Func func(context.Context) error

// Policy decorates an operation with resilience behavior.
type Policy func(Func) Func

// Chain composes policies. The first policy is the outermost one.
func Chain(policies ...Policy) Policy {
	return func(next Func) Func {
		for i := len(policies) - 1; i >= 0; i-- {
			if policies[i] != nil {
				next = policies[i](next)
			}
		}
		return next
	}
}

// Do executes a typed operation under a policy chain.
func Do[T any](ctx context.Context, policy Policy, operation func(context.Context) (T, error)) (T, error) {
	var result T
	if operation == nil {
		return result, nil
	}
	if policy == nil {
		return operation(ctx)
	}
	err := policy(func(ctx context.Context) error {
		var err error
		result, err = operation(ctx)
		return err
	})(ctx)
	return result, err
}

// Timeout bounds the complete inner policy chain.
func Timeout(duration time.Duration) Policy {
	return func(next Func) Func {
		return func(ctx context.Context) error {
			if duration <= 0 {
				return next(ctx)
			}
			ctx, cancel := context.WithTimeout(ctx, duration)
			defer cancel()
			return next(ctx)
		}
	}
}

// Fallback handles an error from the inner chain. It is normally the
// outermost policy so it sees circuit, retry, timeout and bulkhead failures.
func Fallback(handler func(context.Context, error) error) Policy {
	return func(next Func) Func {
		return func(ctx context.Context) error {
			err := next(ctx)
			if err == nil || handler == nil {
				return err
			}
			return handler(ctx, err)
		}
	}
}
