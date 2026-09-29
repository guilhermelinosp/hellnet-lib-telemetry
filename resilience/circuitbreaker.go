package resilience

import (
	"context"
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

// CircuitBreakerConfig configures a circuit breaker.
type CircuitBreakerConfig struct {
	Threshold     int
	OpenTimeout   time.Duration
	ShouldTrip    func(error) bool
	OnStateChange func(from, to State)
}

// CircuitBreaker prevents repeated calls while a dependency is failing.
// Public fields remain available for compatibility with struct literals.
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
	now      func() time.Time
}

// NewCircuitBreaker creates a breaker with resolved defaults.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 5
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = 30 * time.Second
	}
	return &CircuitBreaker{Threshold: cfg.Threshold, OpenTimeout: cfg.OpenTimeout, ShouldTrip: cfg.ShouldTrip, OnStateChange: cfg.OnStateChange, now: time.Now}
}

func (cb *CircuitBreaker) clock() time.Time {
	if cb.now != nil {
		return cb.now()
	}
	return time.Now()
}

func (cb *CircuitBreaker) threshold() int {
	if cb.Threshold > 0 {
		return cb.Threshold
	}
	return 5
}

func (cb *CircuitBreaker) openTimeout() time.Duration {
	if cb.OpenTimeout > 0 {
		return cb.OpenTimeout
	}
	return 30 * time.Second
}

// Policy returns a policy backed by the circuit breaker.
func (cb *CircuitBreaker) Policy() Policy {
	return func(next Func) Func {
		return func(ctx context.Context) error {
			if err := cb.allow(cb.clock()); err != nil {
				return err
			}
			err := next(ctx)
			cb.record(ctx, err)
			return err
		}
	}
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	changed, from, to := cb.transitionIfReadyLocked(cb.clock())
	state := cb.state
	cb.mu.Unlock()
	cb.notify(changed, from, to)
	return state
}

func (cb *CircuitBreaker) allow(now time.Time) error {
	cb.mu.Lock()
	changed, from, to := cb.transitionIfReadyLocked(now)
	var err error
	switch cb.state {
	case Open:
		err = ErrCircuitOpen
	case HalfOpen:
		if cb.probing {
			err = ErrCircuitOpen
		} else {
			cb.probing = true
		}
	}
	cb.mu.Unlock()
	cb.notify(changed, from, to)
	return err
}

func (cb *CircuitBreaker) record(ctx context.Context, err error) {
	cb.mu.Lock()
	var changed bool
	var from, to State
	switch {
	case err == nil:
		cb.failures = 0
		cb.probing = false
		changed, from, to = cb.setStateLocked(Closed)
	case ctx != nil && ctx.Err() != nil:
		if cb.state == HalfOpen {
			cb.probing = false
		}
	case cb.ShouldTrip != nil && !cb.ShouldTrip(err):
		if cb.state == HalfOpen {
			cb.probing = false
		}
	default:
		cb.failures++
		if cb.state == HalfOpen || cb.failures >= cb.threshold() {
			cb.openedAt = cb.clock()
			cb.probing = false
			changed, from, to = cb.setStateLocked(Open)
		}
	}
	cb.mu.Unlock()
	cb.notify(changed, from, to)
}

func (cb *CircuitBreaker) transitionIfReadyLocked(now time.Time) (bool, State, State) {
	if cb.state == Open && now.Sub(cb.openedAt) >= cb.openTimeout() {
		cb.probing = false
		return cb.setStateLocked(HalfOpen)
	}
	return false, cb.state, cb.state
}

func (cb *CircuitBreaker) setStateLocked(next State) (bool, State, State) {
	if cb.state == next {
		return false, cb.state, next
	}
	from := cb.state
	cb.state = next
	return true, from, next
}

func (cb *CircuitBreaker) notify(changed bool, from, to State) {
	if changed && cb.OnStateChange != nil {
		cb.OnStateChange(from, to)
	}
}
