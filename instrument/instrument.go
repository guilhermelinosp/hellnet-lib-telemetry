// Package instrument is the small observability contract consumed by Hellnet
// libraries.
//
// A library should accept an instrument.Instrumentation in its constructor.
// Use the library import path as the instrumentation scope, for example
// "github.com/guilhermelinosp/hellnet-lib-cache/cache". Obtain the tracer,
// meter, and scoped logger once during construction and create metric
// instruments once; do not create them per operation. The application can
// pass (*telemetry.Telemetry) directly because it implements this contract.
//
// Library versions should be supplied with trace.WithInstrumentationVersion,
// metric.WithInstrumentationVersion, or the equivalent options when creating
// instruments. A library can derive its version with
// runtime/debug.ReadBuildInfo. A nil Instrumentation must be treated as
// Noop().
package instrument

import (
	"context"

	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Logger is the minimal context-aware logging contract for a library.
type Logger interface {
	Debug(context.Context, string, ...any)
	Info(context.Context, string, ...any)
	Warn(context.Context, string, ...any)
	Error(context.Context, string, ...any)
}

// Instrumentation supplies the OTel API providers and propagator to a library.
type Instrumentation interface {
	TracerProvider() trace.TracerProvider
	MeterProvider() metric.MeterProvider
	Logger(scope string) Logger
	Propagator() propagation.TextMapPropagator
}

type noopInstrumentation struct {
	tracer trace.TracerProvider
	meter  metric.MeterProvider
	prop   propagation.TextMapPropagator
}

type noopLogger struct{}

func (noopLogger) Debug(context.Context, string, ...any) {}
func (noopLogger) Info(context.Context, string, ...any)  {}
func (noopLogger) Warn(context.Context, string, ...any)  {}
func (noopLogger) Error(context.Context, string, ...any) {}

func (n noopInstrumentation) TracerProvider() trace.TracerProvider { return n.tracer }
func (n noopInstrumentation) MeterProvider() metric.MeterProvider  { return n.meter }
func (n noopInstrumentation) Logger(string) Logger                 { return noopLogger{} }
func (n noopInstrumentation) Propagator() propagation.TextMapPropagator {
	return n.prop
}

// Noop returns an instrumentation implementation that discards all signals.
func Noop() Instrumentation {
	return noopInstrumentation{
		tracer: tracenoop.NewTracerProvider(),
		meter:  metricnoop.NewMeterProvider(),
		prop: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	}
}
