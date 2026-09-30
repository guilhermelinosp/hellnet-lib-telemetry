// Package messaging contains OpenTelemetry messaging conventions shared by
// Hellnet Kafka and other message-based libraries. It has no client-library
// dependency; callers own the message loop and pass an instrumented context.
package messaging

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

// System returns the messaging.system attribute.
func System(value string) attribute.KeyValue {
	return semconv.MessagingSystemKey.String(value)
}

// DestinationName returns the messaging.destination.name attribute.
func DestinationName(value string) attribute.KeyValue {
	return semconv.MessagingDestinationNameKey.String(value)
}

// OperationType returns the messaging.operation.type attribute.
func OperationType(value string) attribute.KeyValue {
	return semconv.MessagingOperationTypeKey.String(value)
}

// OperationName returns the messaging.operation.name attribute.
func OperationName(value string) attribute.KeyValue {
	return semconv.MessagingOperationNameKey.String(value)
}

// ConsumerGroupName returns the messaging.consumer.group.name attribute.
func ConsumerGroupName(value string) attribute.KeyValue {
	return semconv.MessagingConsumerGroupNameKey.String(value)
}

// DestinationPartitionID returns the messaging.destination.partition.id attribute.
func DestinationPartitionID(value string) attribute.KeyValue {
	return semconv.MessagingDestinationPartitionIDKey.String(value)
}

// KafkaOffset returns the messaging.kafka.offset attribute.
func KafkaOffset(value int64) attribute.KeyValue {
	return semconv.MessagingKafkaOffsetKey.Int64(value)
}

// KafkaMessageKey returns the messaging.kafka.message.key attribute.
func KafkaMessageKey(value string) attribute.KeyValue {
	return semconv.MessagingKafkaMessageKeyKey.String(value)
}

// ErrorType returns the error.type attribute.
func ErrorType(value string) attribute.KeyValue {
	return semconv.ErrorTypeKey.String(value)
}

// SendSpanName returns the conventional producer span name.
func SendSpanName(destination string) string { return "send " + destination }

// ProcessSpanName returns the conventional consumer span name.
func ProcessSpanName(destination string) string { return "process " + destination }

// Header is a message header key and value.
type Header struct {
	Key   string
	Value []byte
}

// Carrier implements TextMapCarrier without canonicalizing keys. Kafka
// headers are case-sensitive, so propagation keys are deliberately written in
// lowercase while reads accept any capitalization from other languages.
type Carrier []Header

var _ propagation.TextMapCarrier = (*Carrier)(nil)

// NewCarrier creates a carrier and copies the supplied headers.
func NewCarrier(headers []Header) *Carrier {
	copyHeaders := make(Carrier, len(headers))
	for i, header := range headers {
		copyHeaders[i] = Header{Key: header.Key, Value: append([]byte(nil), header.Value...)}
	}
	return &copyHeaders
}

// Get prefers an exact lower-case key, then returns the first case-insensitive
// match. This precedence preserves Kafka headers written by W3C propagators
// while accepting headers produced by languages with canonicalization.
func (c *Carrier) Get(key string) string {
	if c == nil {
		return ""
	}
	for _, header := range *c {
		if header.Key == strings.ToLower(key) {
			return string(header.Value)
		}
	}
	for _, header := range *c {
		if strings.EqualFold(header.Key, key) {
			return string(header.Value)
		}
	}
	return ""
}

// Set replaces all case variants of key and writes the lower-case key.
func (c *Carrier) Set(key, value string) {
	if c == nil {
		return
	}
	lower := strings.ToLower(key)
	filtered := (*c)[:0]
	for _, header := range *c {
		if !strings.EqualFold(header.Key, key) {
			filtered = append(filtered, header)
		}
	}
	*c = append(filtered, Header{Key: lower, Value: []byte(value)})
}

// Keys returns the carrier header names in insertion order.
func (c *Carrier) Keys() []string {
	if c == nil {
		return nil
	}
	keys := make([]string, 0, len(*c))
	for _, header := range *c {
		keys = append(keys, header.Key)
	}
	return keys
}
