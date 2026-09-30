package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

func TestHarnessCapturesMetricHelpers(t *testing.T) {
	h := New(t)
	ctx := context.Background()
	attr := attribute.String("route", "/orders")
	meter := h.MeterProvider().Meter("test")
	counter, err := meter.Int64Counter("requests_total")
	if err != nil {
		t.Fatal(err)
	}
	floatCounter, err := meter.Float64Counter("latency_total")
	if err != nil {
		t.Fatal(err)
	}
	histogram, err := meter.Float64Histogram("request_duration")
	if err != nil {
		t.Fatal(err)
	}
	gauge, err := meter.Int64ObservableGauge("queue_depth")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		observer.ObserveInt64(gauge, 7, metric.WithAttributes(attr))
		return nil
	}, gauge); err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 3, metric.WithAttributes(attr))
	floatCounter.Add(ctx, 1.5, metric.WithAttributes(attr))
	histogram.Record(ctx, 0.25, metric.WithAttributes(attr))

	if got, ok := h.CounterValue(ctx, "requests_total", attr); !ok || got != 3 {
		t.Fatalf("counter = %d, %v", got, ok)
	}
	if got, ok := h.FloatCounterValue(ctx, "latency_total", attr); !ok || got != 1.5 {
		t.Fatalf("float counter = %f, %v", got, ok)
	}
	if got, ok := h.HistogramCount(ctx, "request_duration", attr); !ok || got != 1 {
		t.Fatalf("histogram count = %d, %v", got, ok)
	}
	if got, ok := h.GaugeValue(ctx, "queue_depth", attr); !ok || got != 7 {
		t.Fatalf("gauge = %f, %v", got, ok)
	}
}

func TestHarnessCapturesSpansAndLogs(t *testing.T) {
	h := New(t)
	ctx := context.Background()
	h.Logger("github.com/example/lib").Info(ctx, "request completed", "route", "/orders")

	parentCtx, parent := h.TracerProvider().Tracer("test").Start(ctx, "parent")
	_, child := h.TracerProvider().Tracer("test").Start(parentCtx, "child")
	child.End()
	parent.End()

	if got := len(h.LogsBySeverity(log.SeverityInfo)); got != 1 {
		t.Fatalf("info logs = %d", got)
	}
	parents := h.SpansByName("parent")
	children := h.SpansByName("child")
	if len(parents) != 1 || len(children) != 1 || !ChildOf(parents[0], children[0]) {
		t.Fatalf("parent/child relationship not captured")
	}
}

func TestResetKeepsInstrumentsCollectable(t *testing.T) {
	h := New(t)
	counter, err := h.MeterProvider().Meter("test").Int64Counter("requests_total")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1)
	h.Reset()
	counter.Add(context.Background(), 2)
	if got, ok := h.CounterValue(context.Background(), "requests_total"); !ok || got != 3 {
		t.Fatalf("counter after reset = %d, %v; want 3, true", got, ok)
	}
}
