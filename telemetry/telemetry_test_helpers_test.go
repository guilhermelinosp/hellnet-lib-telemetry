package telemetry

import (
	"context"
	"sync"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type memoryLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *memoryLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}

func (*memoryLogExporter) Shutdown(context.Context) error   { return nil }
func (*memoryLogExporter) ForceFlush(context.Context) error { return nil }

func newSignalTestTel(t *testing.T) (*Telemetry, *tracetest.InMemoryExporter, *memoryLogExporter, *sdkmetric.ManualReader) {
	t.Helper()
	spanExporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExporter))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	logExporter := &memoryLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)))

	ctx := context.Background()
	logger := zap.New(otelZapCore{
		logger: lp.Logger("telemetry-test"),
		level:  zapcore.DebugLevel,
		ctx:    ctx,
	}).Sugar()
	tel := &Telemetry{
		tracer:      tp.Tracer("telemetry-test"),
		meter:       mp.Meter("telemetry-test"),
		Logger:      logger,
		serviceName: "telemetry-test",
		lp:          lp,
		tp:          tp,
		mp:          mp,
	}
	t.Cleanup(func() {
		_ = tel.Close(context.Background())
	})
	return tel, spanExporter, logExporter, reader
}
