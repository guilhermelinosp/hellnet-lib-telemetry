package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestCompatWrapper(t *testing.T) {
	h := New(t)
	_, span := h.TracerProvider().Tracer("t").Start(context.Background(), "op")
	span.SetAttributes(attribute.String("k", "v"))
	span.End()
	s, ok := h.FindSpan("op")
	if !ok || !HasAttribute(s, "k", attribute.StringValue("v")) {
		t.Fatal("compat wrapper did not expose the span")
	}
}
