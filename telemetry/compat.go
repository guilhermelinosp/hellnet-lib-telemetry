package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// Meter preserves the historical name for Metric.
func (t *Telemetry) Meter(ctx ...context.Context) ContextMeter { return t.Metric(ctx...) }

func (t *Telemetry) WithSpan(name string, fn func(context.Context) error) error {
	_, err := t.runSpan(nil, name, fn)
	return err
}

func (t *Telemetry) WithSpanContext(ctx context.Context, name string, fn func(context.Context) error) error {
	_, err := t.runSpan(ctx, name, fn)
	return err
}

func (t *Telemetry) Span(ctx context.Context, name string, fn func(context.Context) error) error {
	_, err := t.runSpan(ctx, name, fn)
	return err
}

func (t *Telemetry) Worker(job string, fn func(context.Context) error, extra ...attribute.KeyValue) error {
	return t.WorkerContext(nil, job, fn, extra...)
}

func (t *Telemetry) Error(msg string, args ...any) { t.Log().Error(msg, args...) }
func (t *Telemetry) Warn(msg string, args ...any)  { t.Log().Warn(msg, args...) }
func (t *Telemetry) Info(msg string, args ...any)  { t.Log().Info(msg, args...) }
func (t *Telemetry) Debug(msg string, args ...any) { t.Log().Debug(msg, args...) }
