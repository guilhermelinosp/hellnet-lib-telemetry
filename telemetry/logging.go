package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otelLog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	TraceLevel zapcore.Level = -2
	FatalLevel zapcore.Level = 5
	// Custom levels are written directly through the Zap core so they never
	// trigger Zap's terminal or development-only behavior.
	CriticalLevel zapcore.Level = 3
)

// Logger exposes all severities. Fatal and Critical never exit the process.
type Logger interface {
	Trace(string, ...any)
	Debug(string, ...any)
	Info(string, ...any)
	Warn(string, ...any)
	Error(string, ...any)
	Fatal(string, ...any)
	Critical(string, ...any)
	With(...any) Logger
}

type zapLogger struct {
	l       *zap.SugaredLogger
	ctx     context.Context
	onWrite func(zapcore.Level)
}

func (l zapLogger) log(level zapcore.Level, msg string, args ...any) {
	fields := append(contextFields(l.ctx), zapFields(args...)...)
	l.l.Desugar().Check(level, msg).Write(fields...)
}
func (l zapLogger) logNamed(label, msg string, args ...any) {
	fields := append(contextFields(l.ctx), zap.String("severity_text", label))
	fields = append(fields, zapFields(args...)...)
	level := FatalLevel
	if label == "CRITICAL" {
		level = CriticalLevel
	}
	core := l.l.Desugar().Core()
	if core.Enabled(level) {
		_ = core.Write(zapcore.Entry{Level: level, Time: time.Now(), Message: msg}, fields)
		if l.onWrite != nil {
			l.onWrite(level)
		}
	}
}
func (l zapLogger) Trace(m string, a ...any)    { l.log(TraceLevel, m, a...) }
func (l zapLogger) Debug(m string, a ...any)    { l.log(zap.DebugLevel, m, a...) }
func (l zapLogger) Info(m string, a ...any)     { l.log(zap.InfoLevel, m, a...) }
func (l zapLogger) Warn(m string, a ...any)     { l.log(zap.WarnLevel, m, a...) }
func (l zapLogger) Error(m string, a ...any)    { l.log(zap.ErrorLevel, m, a...) }
func (l zapLogger) Fatal(m string, a ...any)    { l.logNamed("FATAL", m, a...) }
func (l zapLogger) Critical(m string, a ...any) { l.logNamed("CRITICAL", m, a...) }
func (l zapLogger) With(a ...any) Logger {
	return zapLogger{l: l.l.With(a...), ctx: l.ctx, onWrite: l.onWrite}
}

func zapFields(args ...any) []zap.Field {
	fields := make([]zap.Field, 0, (len(args)+1)/2)
	for i := 0; i < len(args); i += 2 {
		key := "arg"
		if s, ok := args[i].(string); ok {
			key = s
		}
		var value any
		if i+1 < len(args) {
			value = args[i+1]
		}
		fields = append(fields, zap.Any(key, value))
	}
	return fields
}

func contextFields(ctx context.Context) []zap.Field {
	if ctx == nil {
		return nil
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []zap.Field{zap.String("trace_id", sc.TraceID().String()), zap.String("span_id", sc.SpanID().String())}
}

func (t *Telemetry) Log() Logger {
	return zapLogger{l: t.Logger, ctx: t.baseCtx, onWrite: t.recordLogError}
}
func (t *Telemetry) LogContext(ctx context.Context) Logger {
	if ctx == nil {
		ctx = context.Background()
	}
	return zapLogger{l: t.Logger, ctx: ctx, onWrite: t.recordLogError}
}
func (t *Telemetry) TraceLog(m string, a ...any) { t.Log().Trace(m, a...) }
func (t *Telemetry) Critical(m string, a ...any) { t.Log().Critical(m, a...) }
func (t *Telemetry) Fatal(m string, a ...any)    { t.Log().Fatal(m, a...) }
func (t *Telemetry) Error(m string, a ...any)    { t.Log().Error(m, a...) }
func (t *Telemetry) Warn(m string, a ...any)     { t.Log().Warn(m, a...) }
func (t *Telemetry) Info(m string, a ...any)     { t.Log().Info(m, a...) }
func (t *Telemetry) Debug(m string, a ...any)    { t.Log().Debug(m, a...) }

func (t *Telemetry) logIn(ctx context.Context, level zapcore.Level, msg string, fields ...zap.Field) {
	fields = append(contextFields(ctx), fields...)
	t.Logger.Desugar().Check(level, msg).Write(fields...)
}

func (t *Telemetry) recordLogError(level zapcore.Level) {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	if t.Meter == nil {
		return
	}
	if t.logErrors == nil {
		t.logErrors, _ = t.Meter.Counter("log_errors_total")
	}
	if t.logErrors != nil {
		t.logErrors.Add(context.Background(), 1, metric.WithAttributes(attribute.String("level", levelName(level))))
	}
}

func levelName(level zapcore.Level) string {
	switch level {
	case TraceLevel:
		return "TRACE"
	case zap.DebugLevel:
		return "DEBUG"
	case zap.InfoLevel:
		return "INFO"
	case zap.WarnLevel:
		return "WARN"
	case zap.ErrorLevel:
		return "ERROR"
	case FatalLevel:
		return "FATAL"
	case CriticalLevel:
		return "CRITICAL"
	default:
		return level.String()
	}
}

type otelZapCore struct {
	logger otelLog.Logger
	level  zapcore.LevelEnabler
	fields []zap.Field
}

func (c otelZapCore) Enabled(level zapcore.Level) bool { return c.level.Enabled(level) }
func (c otelZapCore) With(fields []zap.Field) zapcore.Core {
	return otelZapCore{logger: c.logger, level: c.level, fields: append(append([]zap.Field{}, c.fields...), fields...)}
}
func (c otelZapCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}
func (c otelZapCore) Write(e zapcore.Entry, fields []zap.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, field := range append(append([]zap.Field{}, c.fields...), fields...) {
		field.AddTo(enc)
	}
	record := otelLog.Record{}
	record.SetTimestamp(e.Time)
	record.SetSeverityText(levelName(e.Level))
	record.SetSeverity(otelSeverity(e.Level))
	record.SetBody(attribute.StringValue(e.Message))
	attrs := make([]attribute.KeyValue, 0, len(enc.Fields))
	for key, value := range enc.Fields {
		attrs = append(attrs, zapAttribute(key, value))
	}
	record.AddAttributes(attrs...)
	c.logger.Emit(context.Background(), record)
	return nil
}

func zapAttribute(key string, value any) attribute.KeyValue {
	switch v := value.(type) {
	case string:
		return attribute.String(key, v)
	case bool:
		return attribute.Bool(key, v)
	case int:
		return attribute.Int(key, v)
	case int64:
		return attribute.Int64(key, v)
	case float64:
		return attribute.Float64(key, v)
	default:
		return attribute.String(key, fmt.Sprint(v))
	}
}
func (otelZapCore) Sync() error { return nil }

func otelSeverity(level zapcore.Level) otelLog.Severity {
	switch level {
	case TraceLevel:
		return otelLog.SeverityTrace
	case zap.DebugLevel:
		return otelLog.SeverityDebug
	case zap.WarnLevel:
		return otelLog.SeverityWarn
	case zap.ErrorLevel:
		return otelLog.SeverityError
	case FatalLevel:
		return otelLog.SeverityFatal
	case CriticalLevel:
		return otelLog.SeverityFatal4
	default:
		return otelLog.SeverityInfo
	}
}

func encodeZapLevel(level zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(levelName(level))
}

func (t *Telemetry) buildLogger(o Options, res *sdkresource.Resource) error {
	lp, err := newLoggerProvider(o, res)
	if err != nil {
		return err
	}
	t.lp = lp
	stdout := zapcore.NewCore(zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey: "time", LevelKey: "level", NameKey: "logger", CallerKey: "caller",
		MessageKey: "msg", EncodeTime: zapcore.ISO8601TimeEncoder, EncodeLevel: encodeZapLevel,
	}), zapcore.AddSync(os.Stdout), o.LogLevel)
	otelCore := otelZapCore{logger: lp.Logger("zap"), level: o.LogLevel}
	logger := zap.New(zapcore.NewTee(stdout, otelCore), zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Level >= zap.ErrorLevel {
			t.recordLogError(entry.Level)
		}
		return nil
	}))
	t.Logger = logger.Sugar()
	return nil
}

func newLoggerProvider(opts Options, res *sdkresource.Resource) (*sdklog.LoggerProvider, error) {
	logOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	if opts.OTLPEndpoint != "" {
		exporter, err := otlploghttp.New(context.Background(), otlploghttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/logs")), otlploghttp.WithTimeout(5*time.Second))
		if err != nil {
			return nil, err
		}
		logOpts = append(logOpts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter, sdklog.WithExportInterval(time.Second), sdklog.WithExportMaxBatchSize(10))))
	}
	return sdklog.NewLoggerProvider(logOpts...), nil
}
