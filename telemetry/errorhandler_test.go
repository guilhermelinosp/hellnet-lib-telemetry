package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestErrorHandlerLogsThroughTheStructuredLogger(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	tel := &Telemetry{logger: zap.New(core).Sugar(), logLevel: zapcore.DebugLevel}
	t.Cleanup(func() { otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {})) })

	tel.installErrorHandler(context.Background())
	otel.Handle(errors.New("failed to upload metrics: broken pipe"))

	entries := logs.FilterMessage("opentelemetry: internal error").All()
	if len(entries) != 1 {
		t.Fatalf("want one structured log entry, got %d", len(entries))
	}
	if entries[0].Level != zapcore.WarnLevel {
		t.Fatalf("level = %v, want warn", entries[0].Level)
	}
	if got := entries[0].ContextMap()["error"]; got != "failed to upload metrics: broken pipe" {
		t.Fatalf("error field = %v", got)
	}
}

func TestErrorHandlerRateLimitsAndCountsSuppressedErrors(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	tel := &Telemetry{logger: zap.New(core).Sugar(), logLevel: zapcore.DebugLevel}
	t.Cleanup(func() { otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {})) })

	tel.installErrorHandler(context.Background())
	for i := 0; i < 5; i++ {
		otel.Handle(errors.New("exporter down"))
	}

	if n := len(logs.FilterMessage("opentelemetry: internal error").All()); n != 1 {
		t.Fatalf("burst must be logged once inside the %v window, got %d", errorLogWindow, n)
	}
}
