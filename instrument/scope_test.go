package instrument

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
)

type failingMeter struct {
	metricnoop.Meter
}

func (failingMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errors.New("boom")
}

func (failingMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errors.New("boom")
}

type recordingLogger struct {
	noopLogger
	errors int
}

func (l *recordingLogger) Error(context.Context, string, ...any) { l.errors++ }

func TestModuleVersionUnknownModule(t *testing.T) {
	if got := ModuleVersion("example.invalid/none"); got != "unknown" {
		t.Fatalf("ModuleVersion = %q, want unknown", got)
	}
}

func TestNewScopeNilIsNoop(t *testing.T) {
	s := NewScope(nil, "scope", "example.invalid/none")
	if s.Tracer == nil || s.Meter == nil || s.Logger == nil {
		t.Fatal("NewScope(nil) must return usable tracer, meter and logger")
	}
	_, span := s.Tracer.Start(context.Background(), "op")
	if span.SpanContext().IsValid() {
		t.Fatal("noop tracer must not produce valid span contexts")
	}
	span.End()
}

func TestInstrumentsFallBackToNoopAndLog(t *testing.T) {
	log := &recordingLogger{}
	s := Scope{Meter: failingMeter{}, Logger: log, name: "scope"}
	c := s.Int64Counter("c")
	h := s.Float64Histogram("h")
	if c == nil || h == nil {
		t.Fatal("fallback instruments must be non-nil")
	}
	Observe(context.Background(), c, h, time.Now(), attribute.String("result", "ok"))
	if log.errors != 2 {
		t.Fatalf("logged %d creation errors, want 2", log.errors)
	}
}

func TestObserveAcceptsNilInstruments(t *testing.T) {
	Observe(context.Background(), nil, nil, time.Now())
}

type nilableInstrumentation struct{ Instrumentation }

func TestResolve(t *testing.T) {
	if Resolve(nil) == nil {
		t.Fatal("Resolve(nil) must return Noop")
	}
	var typedNil *nilableInstrumentation
	got := Resolve(typedNil)
	if got == Instrumentation(typedNil) {
		t.Fatal("Resolve must replace a typed nil pointer with Noop")
	}
	if got.TracerProvider() == nil {
		t.Fatal("Resolve(typed nil) must return a usable Noop")
	}
	real := &nilableInstrumentation{Instrumentation: Noop()}
	if Resolve(real) != Instrumentation(real) {
		t.Fatal("Resolve must return a non-nil instrumentation unchanged")
	}
	if s := NewScope(typedNil, "scope", "example.invalid/none"); s.Tracer == nil {
		t.Fatal("NewScope must accept a typed nil")
	}
}
