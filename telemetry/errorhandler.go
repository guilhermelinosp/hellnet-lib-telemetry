package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
)

// errorLogWindow is the minimum interval between two logged OpenTelemetry errors.
// A failing exporter retries on a timer, and logging each failure through the same
// pipeline could feed itself, so the errors in between are only counted.
const errorLogWindow = 10 * time.Second

// installErrorHandler routes the internal errors of the OpenTelemetry SDK (for
// example a failed OTLP export) to the structured logger. Without it they go to
// the standard library logger as plain text with no level, which breaks the JSON
// log stream.
func (t *Telemetry) installErrorHandler(ctx context.Context) {
	logCtx := context.WithoutCancel(ctx)
	var (
		mu         sync.Mutex
		last       time.Time
		suppressed int
	)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < errorLogWindow {
			suppressed++
			mu.Unlock()
			return
		}
		last = now
		skipped := suppressed
		suppressed = 0
		mu.Unlock()

		t.Log(logCtx).Warn("opentelemetry: internal error", "error", err.Error(), "suppressed", skipped)
	}))
}
