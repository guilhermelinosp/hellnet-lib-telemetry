package telemetrytest

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SpansByName returns the finalized spans whose name is name.
func (h *Harness) SpansByName(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range h.Spans() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// ChildOf reports whether child's parent is parent.
func ChildOf(parent, child sdktrace.ReadOnlySpan) bool {
	return child.Parent().SpanID() == parent.SpanContext().SpanID()
}

// LogsBySeverity returns the log records with the given severity.
func (h *Harness) LogsBySeverity(severity log.Severity) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range h.Logs() {
		if r.Severity() == severity {
			out = append(out, r)
		}
	}
	return out
}

// CounterValue returns the value of the int64 counter name whose attributes
// are exactly attrs. The bool is false unless exactly one data point matches.
func (h *Harness) CounterValue(ctx context.Context, name string, attrs ...attribute.KeyValue) (int64, bool) {
	sum, ok := h.find(ctx, name).(metricdata.Sum[int64])
	if !ok {
		return 0, false
	}
	if p, ok := only(sum.DataPoints, dpAttrs[int64], attrs); ok {
		return p.Value, true
	}
	return 0, false
}

// HistogramCount returns the number of recordings of the int64 or float64
// histogram name whose attributes are exactly attrs. The bool is false unless
// exactly one data point matches.
func (h *Harness) HistogramCount(ctx context.Context, name string, attrs ...attribute.KeyValue) (uint64, bool) {
	switch d := h.find(ctx, name).(type) {
	case metricdata.Histogram[float64]:
		if p, ok := only(d.DataPoints, histAttrs[float64], attrs); ok {
			return p.Count, true
		}
	case metricdata.Histogram[int64]:
		if p, ok := only(d.DataPoints, histAttrs[int64], attrs); ok {
			return p.Count, true
		}
	}
	return 0, false
}

// GaugeValue returns the value of the int64 or float64 gauge name whose
// attributes are exactly attrs. The bool is false unless exactly one data
// point matches.
func (h *Harness) GaugeValue(ctx context.Context, name string, attrs ...attribute.KeyValue) (float64, bool) {
	switch d := h.find(ctx, name).(type) {
	case metricdata.Gauge[float64]:
		if p, ok := only(d.DataPoints, dpAttrs[float64], attrs); ok {
			return p.Value, true
		}
	case metricdata.Gauge[int64]:
		if p, ok := only(d.DataPoints, dpAttrs[int64], attrs); ok {
			return float64(p.Value), true
		}
	}
	return 0, false
}

func (h *Harness) find(ctx context.Context, name string) metricdata.Aggregation {
	for _, sm := range h.Metrics(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data
			}
		}
	}
	return nil
}

func dpAttrs[N int64 | float64](p metricdata.DataPoint[N]) attribute.Set { return p.Attributes }

func histAttrs[N int64 | float64](p metricdata.HistogramDataPoint[N]) attribute.Set {
	return p.Attributes
}

// only returns the single point whose attributes equal want.
func only[P any](points []P, attrs func(P) attribute.Set, want []attribute.KeyValue) (P, bool) {
	set := attribute.NewSet(want...)
	var found P
	n := 0
	for _, p := range points {
		if a := attrs(p); a.Equals(&set) {
			found = p
			n++
		}
	}
	return found, n == 1
}
