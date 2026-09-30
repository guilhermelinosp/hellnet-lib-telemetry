package messaging

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
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
	if got := carrier.Get("TrAcEpArEnT"); got != "preferred" {
		t.Fatalf("lowercase precedence get = %q", got)
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
	attrs := []struct {
		got  attribute.KeyValue
		key  attribute.Key
		want any
	}{
		{System("kafka"), "messaging.system", "kafka"},
		{DestinationName("orders"), "messaging.destination.name", "orders"},
		{OperationType("send"), "messaging.operation.type", "send"},
		{OperationName("send orders"), "messaging.operation.name", "send orders"},
		{ConsumerGroupName("orders-worker"), "messaging.consumer.group.name", "orders-worker"},
		{DestinationPartitionID("2"), "messaging.destination.partition.id", "2"},
		{KafkaOffset(10), "messaging.kafka.offset", int64(10)},
		{KafkaMessageKey("id"), "messaging.kafka.message.key", "id"},
		{ErrorType("timeout"), "error.type", "timeout"},
	}
	for _, test := range attrs {
		if test.got.Key != test.key {
			t.Errorf("key = %q, want %q", test.got.Key, test.key)
		}
		switch want := test.want.(type) {
		case string:
			if got := test.got.Value.AsString(); got != want {
				t.Errorf("%s = %q, want %q", test.key, got, want)
			}
		case int64:
			if got := test.got.Value.AsInt64(); got != want {
				t.Errorf("%s = %d, want %d", test.key, got, want)
			}
		}
	}
}
