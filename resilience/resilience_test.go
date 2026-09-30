package resilience

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChainAndTypedDo(t *testing.T) {
	policy := Chain(Timeout(time.Second), Bulkhead(1))
	got, err := Do(context.Background(), policy, func(context.Context) (string, error) { return "ok", nil })
	if err != nil || got != "ok" {
		t.Fatalf("result=%q err=%v", got, err)
	}
}

func TestRetryBackoffAndSuccess(t *testing.T) {
	var calls atomic.Int32
	policy := Retry(RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	err := policy(func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("transient")
		}
		return nil
	})(context.Background())
	if err != nil || calls.Load() != 3 {
		t.Fatalf("retry=%v calls=%d", err, calls.Load())
	}
}

func TestTimeoutIsRetried(t *testing.T) {
	var calls atomic.Int32
	policy := Chain(
		Retry(RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}),
		Timeout(time.Millisecond),
	)
	err := policy(func(ctx context.Context) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	})(context.Background())
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 2 {
		t.Fatalf("timeout retry=%v calls=%d", err, calls.Load())
	}
}

func TestTimeoutOpensCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1})
	policy := Chain(cb.Policy(), Timeout(time.Millisecond))
	err := policy(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout=%v, want ErrTimeout", err)
	}
	if got := cb.State(); got != Open {
		t.Fatalf("state=%v, want Open", got)
	}
}

func TestCanceledParentDoesNotOpenCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cb.Policy()(func(context.Context) error { return errors.New("down") })(ctx)
	if err == nil {
		t.Fatal("expected canceled operation error")
	}
	if got := cb.State(); got != Closed {
		t.Fatalf("state=%v, want Closed", got)
	}
}

func TestCircuitBreakerCallbackCanReadState(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1})
	done := make(chan State, 1)
	cb.OnStateChange = func(_, to State) {
		if to == Open {
			done <- cb.State()
		}
	}
	call := cb.Policy()(func(context.Context) error { return errors.New("down") })
	finished := make(chan struct{})
	go func() {
		_ = call(context.Background())
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("state callback deadlocked")
	}
	if got := <-done; got != Open {
		t.Fatalf("callback state=%v, want Open", got)
	}
}

func TestCircuitBreakerUsesInjectedClock(t *testing.T) {
	now := time.Now()
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1, OpenTimeout: time.Second})
	cb.now = func() time.Time { return now }
	call := cb.Policy()(func(context.Context) error { return errors.New("down") })
	_ = call(context.Background())
	if cb.State() != Open {
		t.Fatal("breaker did not open")
	}
	now = now.Add(2 * time.Second)
	if cb.State() != HalfOpen {
		t.Fatal("breaker did not transition deterministically")
	}
}

func TestBulkheadWaitHonorsContext(t *testing.T) {
	policy := BulkheadWait(1, time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = policy(func(context.Context) error { close(started); <-release; return nil })(context.Background())
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := policy(func(context.Context) error { return nil })(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait with canceled context=%v", err)
	}
	close(release)
}

func TestChainOrder(t *testing.T) {
	var order []string
	policy := Chain(func(next Func) Func {
		return func(ctx context.Context) error {
			order = append(order, "outer-before")
			err := next(ctx)
			order = append(order, "outer-after")
			return err
		}
	}, func(next Func) Func {
		return func(ctx context.Context) error {
			order = append(order, "inner-before")
			err := next(ctx)
			order = append(order, "inner-after")
			return err
		}
	})
	if err := policy(func(context.Context) error { order = append(order, "operation"); return nil })(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "outer-before,inner-before,operation,inner-after,outer-after"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order=%q, want %q", got, want)
	}
}

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	now := time.Now()
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 2, OpenTimeout: time.Millisecond})
	cb.now = func() time.Time { return now }
	policy := cb.Policy()
	fail := policy(func(context.Context) error { return errors.New("down") })
	_ = fail(context.Background())
	_ = fail(context.Background())
	if cb.State() != Open {
		t.Fatalf("state=%v", cb.State())
	}
	if !errors.Is(policy(func(context.Context) error { return nil })(context.Background()), ErrCircuitOpen) {
		t.Fatal("open circuit accepted a call")
	}
	now = now.Add(2 * time.Millisecond)
	if err := policy(func(context.Context) error { return nil })(context.Background()); err != nil {
		t.Fatalf("half-open recovery: %v", err)
	}
	if cb.State() != Closed {
		t.Fatalf("state=%v", cb.State())
	}
}

func TestBulkheadRejectsWhenFull(t *testing.T) {
	policy := Bulkhead(1)
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = policy(func(context.Context) error { close(started); <-release; return nil })(context.Background())
	}()
	<-started
	if !errors.Is(policy(func(context.Context) error { return nil })(context.Background()), ErrBulkheadFull) {
		t.Fatal("full bulkhead accepted a call")
	}
	close(release)
}

func TestFallback(t *testing.T) {
	policy := Fallback(func(context.Context, error) error { return nil })
	if err := policy(func(context.Context) error { return errors.New("down") })(context.Background()); err != nil {
		t.Fatalf("fallback=%v", err)
	}
}
