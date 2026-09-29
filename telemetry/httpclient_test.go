package telemetry

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryDelayUsesFullJitter(t *testing.T) {
	for attempt, upper := range map[int]time.Duration{0: 100 * time.Millisecond, 1: 200 * time.Millisecond, 2: 400 * time.Millisecond} {
		for i := 0; i < 100; i++ {
			if got := retryDelay(100*time.Millisecond, attempt); got < 0 || got > upper {
				t.Fatalf("retryDelay(attempt=%d) = %s, want [0,%s]", attempt, got, upper)
			}
		}
	}
}

func TestRetryAfterDelay(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"3"}}}
	if got := retryAfterDelay(resp); got != 3*time.Second {
		t.Fatalf("retryAfterDelay() = %s, want 3s", got)
	}
	resp.Header.Set("Retry-After", "invalid")
	if got := retryAfterDelay(resp); got != 0 {
		t.Fatalf("invalid Retry-After = %s, want 0", got)
	}
}
