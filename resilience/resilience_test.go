package resilience

import (
	"context"
	"errors"
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

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	cb := &CircuitBreaker{Threshold: 2, OpenTimeout: time.Millisecond}
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
	time.Sleep(2 * time.Millisecond)
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
