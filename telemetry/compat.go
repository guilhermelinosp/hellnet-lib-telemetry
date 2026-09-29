package telemetry

import "context"

// Client is the minimal contract consumed by the database and Kafka libraries.
// It exposes the same context-first trace facade used by application code.
type Client interface {
	Trace(context.Context) ContextTracer
}
