package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/instrument"
	"go.opentelemetry.io/otel/attribute"
	otelLog "go.opentelemetry.io/otel/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger returns a scoped context-aware logger for library instrumentation.
func (t *Telemetry) Logger(scope string) instrument.Logger {
	return t.contractLogger(scope)
}

type contractLogger struct {
	tel    *Telemetry
	logger otelLog.Logger
}

var _ instrument.Logger = (*contractLogger)(nil)

func (l *contractLogger) Debug(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, zap.DebugLevel, otelLog.SeverityDebug, msg, args...)
}
func (l *contractLogger) Info(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, zap.InfoLevel, otelLog.SeverityInfo, msg, args...)
}
func (l *contractLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, zap.WarnLevel, otelLog.SeverityWarn, msg, args...)
}
func (l *contractLogger) Error(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, zap.ErrorLevel, otelLog.SeverityError, msg, args...)
}

func (l *contractLogger) emit(ctx context.Context, level zapcore.Level, severity otelLog.Severity, msg string, args ...any) {
	if !l.tel.logLevel.Enabled(level) {
		return
	}
	msg = sanitizeLogMessage(msg)
	zapFields, attrs := contractFields(args...)
	if l.tel.stdoutLogger != nil {
		fields := append(contextFields(ctx), zapFields...)
		l.tel.stdoutLogger.Desugar().Check(level, msg).Write(fields...) //nolint:gosec // CR/LF are neutralized above before entering Zap.
	}
	if !l.logger.Enabled(ctx, otelLog.EnabledParameters{Severity: severity}) {
		return
	}
	record := otelLog.Record{}
	record.SetTimestamp(time.Now())
	record.SetSeverity(severity)
	record.SetSeverityText(levelName(level))
	record.SetBody(attribute.StringValue(msg))
	record.AddAttributes(attrs...)
	// The context is passed to Emit, so the SDK records native trace/span IDs.
	l.logger.Emit(ctx, record)
	if level >= zap.ErrorLevel {
		l.tel.recordLogError(ctx, level)
	}
}

func sanitizeLogMessage(msg string) string {
	return strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(msg)
}

func (t *Telemetry) contractLogger(scope string) instrument.Logger {
	if scope == "" {
		scope = t.serviceName
	}
	return &contractLogger{tel: t, logger: t.lp.Logger(scope)}
}

func contractFields(args ...any) ([]zap.Field, []attribute.KeyValue) {
	zapFields := make([]zap.Field, 0, (len(args)+1)/2)
	attrs := make([]attribute.KeyValue, 0, (len(args)+1)/2)
	for i := 0; i < len(args); i += 2 {
		key := "arg"
		if value, ok := args[i].(string); ok {
			key = value
		}
		var value any
		if i+1 < len(args) {
			value = args[i+1]
		}
		if isSensitiveKey(key) {
			value = "[REDACTED]"
		}
		zapFields = append(zapFields, zap.Any(key, value))
		attrs = append(attrs, contractAttribute(key, value))
	}
	return zapFields, attrs
}

func contractAttribute(key string, value any) attribute.KeyValue {
	switch value := value.(type) {
	case string:
		return attribute.String(key, value)
	case bool:
		return attribute.Bool(key, value)
	case int:
		return attribute.Int(key, value)
	case int64:
		return attribute.Int64(key, value)
	case float64:
		return attribute.Float64(key, value)
	case error:
		return attribute.String(key, value.Error())
	default:
		return attribute.String(key, fmt.Sprint(value))
	}
}
