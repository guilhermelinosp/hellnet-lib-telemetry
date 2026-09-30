package instrument

import (
	"context"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

// ModuleVersion returns the version of modulePath in the running binary's
// build info, or "unknown" when it is not available.
func ModuleVersion(modulePath string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath && dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}

// Scope is the tracer, meter and logger of one library instrumentation scope.
// Build it once during construction with NewScope.
type Scope struct {
	Tracer trace.Tracer
	Meter  metric.Meter
	Logger Logger
	name   string
}

// NewScope resolves the tracer, meter and logger for scope, stamping them with
// the version of modulePath. A nil inst is treated as Noop().
func NewScope(inst Instrumentation, scope, modulePath string) Scope {
	if inst == nil {
		inst = Noop()
	}
	version := ModuleVersion(modulePath)
	return Scope{
		Tracer: inst.TracerProvider().Tracer(scope, trace.WithInstrumentationVersion(version)),
		Meter:  inst.MeterProvider().Meter(scope, metric.WithInstrumentationVersion(version)),
		Logger: inst.Logger(scope),
		name:   scope,
	}
}

// Int64Counter creates a counter. If the meter rejects it, the failure is
// logged and a no-op counter is returned so callers never need a nil check.
func (s Scope) Int64Counter(name string, opts ...metric.Int64CounterOption) metric.Int64Counter {
	c, err := s.Meter.Int64Counter(name, opts...)
	if err != nil {
		s.Logger.Error(context.Background(), "metric creation failed", "metric", name, "error", err)
		c, _ = metricnoop.NewMeterProvider().Meter(s.name).Int64Counter(name)
	}
	return c
}

// Float64Histogram creates a histogram with the same no-op fallback as
// Int64Counter.
func (s Scope) Float64Histogram(name string, opts ...metric.Float64HistogramOption) metric.Float64Histogram {
	h, err := s.Meter.Float64Histogram(name, opts...)
	if err != nil {
		s.Logger.Error(context.Background(), "metric creation failed", "metric", name, "error", err)
		h, _ = metricnoop.NewMeterProvider().Meter(s.name).Float64Histogram(name)
	}
	return h
}

// Observe counts one operation and records its duration in seconds, both with
// attrs. Either instrument may be nil.
func Observe(ctx context.Context, count metric.Int64Counter, duration metric.Float64Histogram, started time.Time, attrs ...attribute.KeyValue) {
	opt := metric.WithAttributes(attrs...)
	if count != nil {
		count.Add(ctx, 1, opt)
	}
	if duration != nil {
		duration.Record(ctx, time.Since(started).Seconds(), opt)
	}
}
