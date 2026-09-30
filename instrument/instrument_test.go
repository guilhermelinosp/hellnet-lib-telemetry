package instrument

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/propagation"
)

func TestNoopIsCompleteAndPropagatesW3CHeaders(t *testing.T) {
	h := Noop()
	if h.TracerProvider() == nil || h.MeterProvider() == nil || h.Logger("scope") == nil || h.Propagator() == nil {
		t.Fatal("Noop returned a nil contract component")
	}
	carrier := propagation.MapCarrier{}
	h.Propagator().Inject(context.Background(), carrier)
	if got := h.Propagator().Extract(context.Background(), carrier); got == nil {
		t.Fatal("propagator returned nil context")
	}
}
