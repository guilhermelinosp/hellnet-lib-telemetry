// Package telemetrytest is kept for compatibility.
//
// Deprecated: use telemetry.NewHarness, which lives in the telemetry package.
package telemetrytest

import (
	"testing"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/instrument"
	"github.com/guilhermelinosp/hellnet-lib-telemetry/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Harness wraps telemetry.Harness.
//
// Deprecated: use telemetry.Harness.
type Harness struct{ *telemetry.Harness }

var _ instrument.Instrumentation = (*Harness)(nil)

// New creates a harness and registers cleanup with t.
//
// Deprecated: use telemetry.NewHarness.
func New(t testing.TB) *Harness { return &Harness{telemetry.NewHarness(t)} }

// HasAttribute reports whether a span has an attribute with key and value.
//
// Deprecated: use telemetry.HasAttribute.
func HasAttribute(span sdktrace.ReadOnlySpan, key attribute.Key, want attribute.Value) bool {
	return telemetry.HasAttribute(span, key, want)
}
