package telemetry

import "context"

// Client is the minimal legacy contract still consumed by the database and
// Kafka libraries. New application code should use *Telemetry directly.
type Client interface {
	WithSpan(string, func(context.Context) error) error
	Span(context.Context, string, func(context.Context) error) error
}

// WithSpan is kept for older sibling libraries that have no parent context.
// New code must use Trace(ctx).Span(...).
func (t *Telemetry) WithSpan(name string, fn func(context.Context) error) error {
	_, err := t.runSpan(context.Background(), name, fn)
	return err
}

// Span is the legacy direct form of Trace(ctx).Span(...).
func (t *Telemetry) Span(ctx context.Context, name string, fn func(context.Context) error) error {
	_, err := t.runSpan(ctx, name, fn)
	return err
}
