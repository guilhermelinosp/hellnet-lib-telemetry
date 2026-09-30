// Package telemetrytest provides a small in-memory Instrumentation
// implementation for library tests. It intentionally depends on the OTel SDK
// and is not intended for production binaries.
package telemetrytest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/instrument"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

var _ instrument.Instrumentation = (*Harness)(nil)

// Harness is an in-memory observability backend for a single test.
type Harness struct {
	testing testing.TB

	recorder *tracetest.SpanRecorder
	tp       *sdktrace.TracerProvider

	mu   sync.Mutex
	logs []sdklog.Record
	logp *sdklog.LoggerProvider

	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider
}

type logExporter struct {
	owner *Harness
}

func (e *logExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	for i := range records {
		e.owner.logs = append(e.owner.logs, records[i].Clone())
	}
	return nil
}
func (*logExporter) Shutdown(context.Context) error   { return nil }
func (*logExporter) ForceFlush(context.Context) error { return nil }

// New creates a harness and registers cleanup with t.
func New(t testing.TB) *Harness {
	t.Helper()
	h := &Harness{testing: t, recorder: tracetest.NewSpanRecorder()}
	h.tp = sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(h.recorder),
	)
	h.reader = sdkmetric.NewManualReader()
	h.mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	h.logp = sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(&logExporter{owner: h})),
	)
	t.Cleanup(func() {
		_ = h.tp.Shutdown(context.Background())
		_ = h.mp.Shutdown(context.Background())
		_ = h.logp.Shutdown(context.Background())
	})
	return h
}

// TracerProvider returns the recording tracer provider.
func (h *Harness) TracerProvider() trace.TracerProvider { return h.tp }

// TracerProviderSDK returns the SDK tracer provider for white-box adapter tests.
func (h *Harness) TracerProviderSDK() *sdktrace.TracerProvider { return h.tp }

// MeterProvider returns the manual-reader meter provider.
func (h *Harness) MeterProvider() metric.MeterProvider { return h.mp }

// MeterProviderSDK returns the SDK meter provider for white-box adapter tests.
func (h *Harness) MeterProviderSDK() *sdkmetric.MeterProvider { return h.mp }

// LoggerProviderSDK returns the SDK logger provider for white-box adapter tests.
func (h *Harness) LoggerProviderSDK() *sdklog.LoggerProvider { return h.logp }

// Logger returns an in-memory logger for scope.
func (h *Harness) Logger(scope string) instrument.Logger {
	return &logger{inner: h.logp.Logger(scope)}
}

// Propagator returns the W3C TraceContext+Baggage propagator.
func (h *Harness) Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

// Spans returns finalized spans observed so far.
func (h *Harness) Spans() []sdktrace.ReadOnlySpan { return h.recorder.Ended() }

// Logs returns copies of log records observed so far.
func (h *Harness) Logs() []sdklog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]sdklog.Record, len(h.logs))
	for i := range h.logs {
		out[i] = h.logs[i].Clone()
	}
	return out
}

// Metrics collects and returns the current cumulative metric snapshot.
func (h *Harness) Metrics(ctx context.Context) metricdata.ResourceMetrics {
	var out metricdata.ResourceMetrics
	if err := h.reader.Collect(ctx, &out); err != nil {
		h.testing.Fatalf("collect metrics: %v", err)
	}
	return out
}

// Reset clears spans and logs. Metrics remain cumulative because instruments
// keep the same MeterProvider for the lifetime of the harness.
func (h *Harness) Reset() {
	h.recorder.Reset()
	h.mu.Lock()
	h.logs = nil
	h.mu.Unlock()
}

// FindSpan returns the first finalized span with name.
func (h *Harness) FindSpan(name string) (sdktrace.ReadOnlySpan, bool) {
	for _, span := range h.Spans() {
		if span.Name() == name {
			return span, true
		}
	}
	return nil, false
}

// SpansByName returns all finalized spans with name.
func (h *Harness) SpansByName(name string) []sdktrace.ReadOnlySpan {
	spans := make([]sdktrace.ReadOnlySpan, 0)
	for _, span := range h.Spans() {
		if span.Name() == name {
			spans = append(spans, span)
		}
	}
	return spans
}

// LogsBySeverity returns copies of logs with severity.
func (h *Harness) LogsBySeverity(severity log.Severity) []sdklog.Record {
	logs := h.Logs()
	filtered := make([]sdklog.Record, 0)
	for _, record := range logs {
		if record.Severity() == severity {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

// CounterValue returns an int64 sum matching name and the exact attribute set.
func (h *Harness) CounterValue(ctx context.Context, name string, attrs ...attribute.KeyValue) (int64, bool) {
	want := attribute.NewSet(attrs...)
	metrics := h.Metrics(ctx)
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name != name {
				continue
			}
			if sum, ok := current.Data.(metricdata.Sum[int64]); ok {
				for _, point := range sum.DataPoints {
					if point.Attributes.Equals(&want) {
						return point.Value, true
					}
				}
			}
		}
	}
	return 0, false
}

// FloatCounterValue returns a float64 sum matching name and the exact attribute set.
func (h *Harness) FloatCounterValue(ctx context.Context, name string, attrs ...attribute.KeyValue) (float64, bool) {
	want := attribute.NewSet(attrs...)
	metrics := h.Metrics(ctx)
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name != name {
				continue
			}
			if sum, ok := current.Data.(metricdata.Sum[float64]); ok {
				for _, point := range sum.DataPoints {
					if point.Attributes.Equals(&want) {
						return point.Value, true
					}
				}
			}
		}
	}
	return 0, false
}

// HistogramCount returns the count for a histogram matching name and attributes.
func (h *Harness) HistogramCount(ctx context.Context, name string, attrs ...attribute.KeyValue) (uint64, bool) {
	want := attribute.NewSet(attrs...)
	metrics := h.Metrics(ctx)
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name != name {
				continue
			}
			switch histogram := current.Data.(type) {
			case metricdata.Histogram[int64]:
				for _, point := range histogram.DataPoints {
					if point.Attributes.Equals(&want) {
						return point.Count, true
					}
				}
			case metricdata.Histogram[float64]:
				for _, point := range histogram.DataPoints {
					if point.Attributes.Equals(&want) {
						return point.Count, true
					}
				}
			}
		}
	}
	return 0, false
}

// GaugeValue returns a gauge as float64 matching name and the exact attributes.
func (h *Harness) GaugeValue(ctx context.Context, name string, attrs ...attribute.KeyValue) (float64, bool) {
	want := attribute.NewSet(attrs...)
	metrics := h.Metrics(ctx)
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name != name {
				continue
			}
			switch gauge := current.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, point := range gauge.DataPoints {
					if point.Attributes.Equals(&want) {
						return float64(point.Value), true
					}
				}
			case metricdata.Gauge[float64]:
				for _, point := range gauge.DataPoints {
					if point.Attributes.Equals(&want) {
						return point.Value, true
					}
				}
			}
		}
	}
	return 0, false
}

// ChildOf reports whether child directly descends from parent.
func ChildOf(parent, child sdktrace.ReadOnlySpan) bool {
	if parent == nil || child == nil {
		return false
	}
	return child.Parent().IsValid() && child.Parent().TraceID() == parent.SpanContext().TraceID() && child.Parent().SpanID() == parent.SpanContext().SpanID()
}

// HasAttribute reports whether a span has an attribute with key and value.
func HasAttribute(span sdktrace.ReadOnlySpan, key attribute.Key, want attribute.Value) bool {
	for _, got := range span.Attributes() {
		if got.Key == key && got.Value == want {
			return true
		}
	}
	return false
}

type logger struct{ inner log.Logger }

func (l *logger) emit(ctx context.Context, severity log.Severity, msg string, args ...any) { //nolint:contextcheck // the logger contract accepts nil contexts
	if ctx == nil {
		ctx = context.Background()
	}
	record := log.Record{}
	record.SetSeverity(severity)
	record.SetSeverityText(severity.String())
	record.SetBody(attribute.StringValue(msg))
	for i := 0; i < len(args); i += 2 {
		key := "arg"
		if value, ok := args[i].(string); ok {
			key = value
		}
		var value any
		if i+1 < len(args) {
			value = args[i+1]
		}
		record.AddAttributes(attribute.String(key, toString(value)))
	}
	l.inner.Emit(ctx, record)
}
func (l *logger) Debug(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, log.SeverityDebug, msg, args...)
}
func (l *logger) Info(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, log.SeverityInfo, msg, args...)
}
func (l *logger) Warn(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, log.SeverityWarn, msg, args...)
}
func (l *logger) Error(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, log.SeverityError, msg, args...)
}

func toString(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprint(v)
}
