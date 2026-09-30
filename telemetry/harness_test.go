package telemetry

import (
	"context"
	"testing"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/instrument"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

func TestQueryHelpers(t *testing.T) {
	ctx := context.Background()
	h := NewHarness(t)
	s := instrument.NewScope(h, "scope", "example.invalid/none")
	result := attribute.String("result", "ok")

	s.Int64Counter("ops").Add(ctx, 3, metric.WithAttributes(result))
	s.Float64Histogram("dur").Record(ctx, 0.5, metric.WithAttributes(result))
	g, err := s.Meter.Float64Gauge("g")
	if err != nil {
		t.Fatal(err)
	}
	g.Record(ctx, 2.5, metric.WithAttributes(result))

	if v, ok := h.CounterValue(ctx, "ops", result); !ok || v != 3 {
		t.Fatalf("CounterValue = %d, %v", v, ok)
	}
	if _, ok := h.CounterValue(ctx, "ops", attribute.String("result", "other")); ok {
		t.Fatal("CounterValue matched different attributes")
	}
	if c, ok := h.HistogramCount(ctx, "dur", result); !ok || c != 1 {
		t.Fatalf("HistogramCount = %d, %v", c, ok)
	}
	if v, ok := h.GaugeValue(ctx, "g", result); !ok || v != 2.5 {
		t.Fatalf("GaugeValue = %v, %v", v, ok)
	}
	if _, ok := h.CounterValue(ctx, "missing"); ok {
		t.Fatal("CounterValue found a missing metric")
	}

}

func TestSpanAndLogHelpers(t *testing.T) {
	ctx := context.Background()
	h := NewHarness(t)
	s := instrument.NewScope(h, "scope", "example.invalid/none")

	ctx, parent := s.Tracer.Start(ctx, "parent")
	_, child := s.Tracer.Start(ctx, "child")
	child.End()
	parent.End()
	p, c := h.SpansByName("parent"), h.SpansByName("child")
	if len(p) != 1 || len(c) != 1 || !ChildOf(p[0], c[0]) || ChildOf(c[0], p[0]) {
		t.Fatalf("span helpers: parent=%d child=%d", len(p), len(c))
	}

	s.Logger.Warn(ctx, "w")
	s.Logger.Error(ctx, "e")
	if len(h.LogsBySeverity(log.SeverityWarn)) != 1 || len(h.LogsBySeverity(log.SeverityError)) != 1 {
		t.Fatal("LogsBySeverity did not filter")
	}
}
