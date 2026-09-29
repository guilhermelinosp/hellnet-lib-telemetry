package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State identifies the current circuit breaker state.
type State int

const (
	// Closed allows requests and counts failures.
	Closed State = iota
	// Open rejects requests until the open timeout elapses.
	Open
	// HalfOpen allows one probe request.
	HalfOpen
)

// CircuitBreaker prevents repeated calls while a dependency is failing.
type CircuitBreaker struct {
	Threshold     int
	OpenTimeout   time.Duration
	ShouldTrip    func(error) bool
	OnStateChange func(from, to State)

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

// Policy returns a policy backed by the circuit breaker.
func (cb *CircuitBreaker) Policy() Policy {
	return func(next Func) Func {
		return func(ctx context.Context) error {
			if err := cb.allow(time.Now()); err != nil {
				return err
			}
			err := next(ctx)
			cb.record(err)
			return err
		}
	}
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.defaults()
	cb.transitionIfReady(time.Now())
	return cb.state
}

func (cb *CircuitBreaker) allow(now time.Time) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.defaults()
	cb.transitionIfReady(now)
	switch cb.state {
	case Open:
		return ErrCircuitOpen
	case HalfOpen:
		if cb.probing {
			return ErrCircuitOpen
		}
		cb.probing = true
	}
	return nil
}

func (cb *CircuitBreaker) record(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.defaults()
	if err == nil {
		cb.failures = 0
		cb.probing = false
		cb.setState(Closed)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if cb.state == HalfOpen {
			cb.probing = false
		}
		return
	}
	if cb.ShouldTrip != nil && !cb.ShouldTrip(err) {
		if cb.state == HalfOpen {
			cb.probing = false
		}
		return
	}
	cb.failures++
	if cb.state == HalfOpen || cb.failures >= cb.Threshold {
		cb.openedAt = time.Now()
		cb.probing = false
		cb.setState(Open)
	}
}

func (cb *CircuitBreaker) defaults() {
	if cb.Threshold <= 0 {
		cb.Threshold = 5
	}
	if cb.OpenTimeout <= 0 {
		cb.OpenTimeout = 30 * time.Second
	}
}

func (cb *CircuitBreaker) transitionIfReady(now time.Time) {
	if cb.state == Open && now.Sub(cb.openedAt) >= cb.OpenTimeout {
		cb.probing = false
		cb.setState(HalfOpen)
	}
}

func (cb *CircuitBreaker) setState(next State) {
	if cb.state == next {
		return
	}
	from := cb.state
	cb.state = next
	if cb.OnStateChange != nil {
		cb.OnStateChange(from, next)
	}
}
