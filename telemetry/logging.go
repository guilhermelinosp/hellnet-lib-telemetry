package telemetry

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
)

// Logger é a abstração de logs (níveis padrão slog, sem Trace).
// Espelha a superfície básica do *slog.Logger para permitir DI/mock.
//
// Não recebe ctx: a correlação de trace usa o contexto-base (definido em New)
// internamente nos sinks (contextTraceHandler/otelslog).
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	With(args ...any) Logger
}

// contextTraceHandler enriquece registros slog com trace_id/span_id quando há
// um span ativo no contexto, correlacionando logs com traces no stdout (o
// otelslog já cobre o sink OTLP). WithAttrs/WithGroup reaplicam o wrapper
// para handlers derivados manterem a injeção.
type contextTraceHandler struct {
	slog.Handler
}

// errorCountHandler conta automaticamente erros (nível >= Error) emitidos via
// slog, expondo log_errors_total. O contador é criado preguiçosamente no
// primeiro Handle, quando tel.Meter já está disponível (o bloco de logging é
// construído antes do de métricas no New).
//
// IMPORTANTE: mu e cnt são PONTEIROS compartilhados entre o handler original e
// seus derivados (WithAttrs/WithGroup) — handlers derivados incrementam o MESMO
// contador visível no original, inclusive durante o lazy-init (a criação feita
// pelo derivado é vista pelo original e vice-versa). Use newErrorCountHandler.
type errorCountHandler struct {
	slog.Handler
	tel *Telemetry
	mu  *sync.Mutex
	cnt *metric.Int64Counter
}

// slogLogger implementa Logger. Internamente usa o contexto-base
// (retido em New) nas chamadas *Context do slog, mantendo a
// correlação trace→log (stdout e OTLP) para a linhagem raiz→filhos.
type slogLogger struct {
	l   *slog.Logger
	ctx context.Context
}

func (h contextTraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextTraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextTraceHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextTraceHandler) WithGroup(name string) slog.Handler {
	return contextTraceHandler{h.Handler.WithGroup(name)}
}

// newErrorCountHandler cria o handler com o estado compartilhado inicializado.
func newErrorCountHandler(h slog.Handler, tel *Telemetry) *errorCountHandler {
	return &errorCountHandler{
		Handler: h,
		tel:     tel,
		mu:      &sync.Mutex{},
		cnt:     new(metric.Int64Counter),
	}
}

func (h *errorCountHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && h.tel != nil {
		h.mu.Lock()
		c := *h.cnt
		if c == nil && h.tel.Meter != nil {
			c, _ = h.tel.Meter.Counter("log_errors_total")
			*h.cnt = c // escreve via ponteiro compartilhado: derivados + original veem
		}
		h.mu.Unlock()
		if c != nil {
			c.Add(ctx, 1, metric.WithAttributes(attribute.String("level", r.Level.String())))
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h *errorCountHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &errorCountHandler{h.Handler.WithAttrs(attrs), h.tel, h.mu, h.cnt}
}

func (h *errorCountHandler) WithGroup(name string) slog.Handler {
	return &errorCountHandler{h.Handler.WithGroup(name), h.tel, h.mu, h.cnt}
}

func (s slogLogger) log(level slog.Level, msg string, args ...any) {
	s.l.Log(s.ctx, level, msg, args...)
}

func (s slogLogger) Debug(msg string, a ...any) { s.log(slog.LevelDebug, msg, a...) }
func (s slogLogger) Info(msg string, a ...any)  { s.log(slog.LevelInfo, msg, a...) }
func (s slogLogger) Warn(msg string, a ...any)  { s.log(slog.LevelWarn, msg, a...) }
func (s slogLogger) Error(msg string, a ...any) { s.log(slog.LevelError, msg, a...) }

func (s slogLogger) With(a ...any) Logger { return slogLogger{l: s.l.With(a...), ctx: s.ctx} }

// logIn registra um log correlacionado ao ctx informado (ex.: span de request
// capturado pelo Middleware, ou span interno do Worker), preservando a
// correlação trace→log nos sinks (contextTraceHandler no stdout e otelslog via
// OTLP) sem expor ctx na abstração pública Logger. Nil-safe (slog.Default).
func (t *Telemetry) logIn(ctx context.Context, level slog.Level, msg string, args ...any) {
	l := t.Logger
	if l == nil {
		l = slog.Default()
	}
	switch level {
	case slog.LevelDebug:
		l.DebugContext(ctx, msg, args...)
	case slog.LevelWarn:
		l.WarnContext(ctx, msg, args...)
	case slog.LevelError:
		l.ErrorContext(ctx, msg, args...)
	default:
		l.InfoContext(ctx, msg, args...)
	}
}

// buildLogger monta o Logger (stdout JSON + OTLP → Loki) com redação e
// enriquecimento de trace_id/span_id, registrando o erro-count handler.
func (t *Telemetry) buildLogger(o Options, res *sdkresource.Resource) error {
	lp, err := newLoggerProvider(o, res)
	if err != nil {
		return err
	}
	t.lp = lp

	// stdout JSON handler (enriquecido com trace_id/span_id p/ correlação)
	var stdoutHandler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: o.LogLevel})

	stdoutHandler = contextTraceHandler{stdoutHandler}

	// OTel bridge handler (sends via OTLP → Alloy → Loki)
	var otelHandler slog.Handler = otelslog.NewHandler("otel", otelslog.WithLoggerProvider(lp))

	// MultiHandler: writes to BOTH stdout AND OTLP; errorCountHandler
	// conta erros logados automaticamente (log_errors_total).
	t.Logger = slog.New(newErrorCountHandler(slog.NewMultiHandler(stdoutHandler, otelHandler), t))
	slog.SetDefault(t.Logger)
	return nil
}

// newLoggerProvider cria o LoggerProvider SDK. Endpoint vazio → sem export
// OTLP (evita URL "https:" inválida no exporter).
func newLoggerProvider(opts Options, res *sdkresource.Resource) (*sdklog.LoggerProvider, error) {
	logOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}

	if opts.OTLPEndpoint != "" {
		exporter, err := otlploghttp.New(
			context.Background(),
			otlploghttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/logs")),
			otlploghttp.WithTimeout(5*time.Second),
		)
		if err != nil {
			return nil, err
		}
		logOpts = append(logOpts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter, sdklog.WithExportInterval(1*time.Second), sdklog.WithExportMaxBatchSize(10))))
	}

	return sdklog.NewLoggerProvider(logOpts...), nil
}

// Log retorna a abstração de logs. Nome evita colisão com o campo exportado Logger.
// O logger retornado deriva correlação do contexto-base internamente.
func (t *Telemetry) Log() Logger { return slogLogger{l: t.Logger, ctx: t.baseCtx} }
