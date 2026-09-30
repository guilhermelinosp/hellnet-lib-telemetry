package messaging

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestCarrierInjectsLowercaseAndRoundTrips(t *testing.T) {
	carrier := NewCarrier(nil)
	propagator := propagation.TraceContext{}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	propagator.Inject(ctx, carrier)

	for _, key := range carrier.Keys() {
		if key != "traceparent" {
			t.Fatalf("injected header %q is not lowercase traceparent", key)
		}
	}
	extracted := propagator.Extract(context.Background(), carrier)
	if got := trace.SpanContextFromContext(extracted); !got.IsValid() || got.TraceID() != sc.TraceID() || got.SpanID() != sc.SpanID() {
		t.Fatalf("round-trip span context = %v", got)
	}
}

func TestCarrierReadsForeignCaseAndDuplicateHeaders(t *testing.T) {
	carrier := NewCarrier([]Header{
		{Key: "TraceParent", Value: []byte("foreign")},
		{Key: "traceparent", Value: []byte("preferred")},
		{Key: "BAGGAGE", Value: []byte("k=v")},
	})
	if got := carrier.Get("TrAcEpArEnT"); got != "foreign" {
		t.Fatalf("case-insensitive get = %q", got)
	}
	if got := carrier.Get("baggage"); got != "k=v" {
		t.Fatalf("foreign baggage = %q", got)
	}
	carrier.Set("TraceParent", "replacement")
	if got := carrier.Get("traceparent"); got != "replacement" {
		t.Fatalf("set replacement = %q", got)
	}
	if len(carrier.Keys()) != 2 {
		t.Fatalf("headers after deduplicating set = %v", carrier.Keys())
	}
}

func TestMessagingNamesAndAttributes(t *testing.T) {
	if SendSpanName("orders") != "send orders" || ProcessSpanName("orders") != "process orders" {
		t.Fatal("unexpected messaging span name")
	}
	attrs := []interface{}{
		System("kafka"), DestinationName("orders"), OperationType("send"), OperationName("send orders"),
		ConsumerGroupName("orders-worker"), DestinationPartitionID("2"), KafkaOffset(10), KafkaMessageKey("id"), ErrorType("timeout"),
	}
	if len(attrs) != 9 {
		t.Fatalf("attributes = %d", len(attrs))
	}
}
