// Package telemetry provides opinionated OpenTelemetry observability for Go services.
//
// Usage (sem parâmetros — a lib lê tudo do ambiente: .env + HELLNET_*):
//
//	tel, err := telemetry.New()
//	defer tel.Close()
//
//	// Tracing (no ctx in the API — spans derive from the base context)
//	err := tel.WithSpan("operation", func(ctx context.Context) error {
//		span := trace.SpanFromContext(ctx) // continues this span
//		return doWork(ctx)
//	})
//
//	// Metrics
//	counter, _ := tel.Meter.Counter("requests.total")
//	counter.Add(context.Background(), 1)
//
//	// Logging (via slog → stdout + OTLP → Loki), correlated with the
//	// base-context trace lineage internally
//	tel.Log().Info("processing", "id", orderID)
//
// Correlation consequence: application-level traces form a single lineage
// rooted at the base context (WithSpan/Worker spawn children under it), and
// nested WithSpan/Worker calls inside fn automatically become CHILDREN of the
// active span. Request-scoped traces extracted by the HTTP Middleware remain
// independent: they originate from inbound requests, which is correct
// server-side behavior.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/guilhermelinosp/hellnet-lib-environments/environments"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconvv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/prometheus/client_golang/prometheus"
)

// Telemetry wraps OpenTelemetry primitives (tracer, meter, logger)
// pre-configured for the service.
type Telemetry struct {
	Tracer trace.Tracer
	Meter  Meter
	Logger *slog.Logger

	// baseCtx é o contexto-raiz da aplicação, informado UMA vez em New/MustNew.
	baseCtx context.Context

	spanMu    sync.Mutex
	spanStack []*spanEntry

	serviceName  string
	otlpEndpoint string
	environment  string
	mu           sync.RWMutex
	healthChecks map[string]func(ctx context.Context) error

	healthStatusMu sync.Mutex
	healthStatus   map[string]int64

	lp *sdklog.LoggerProvider
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider

	promRegistry *prometheus.Registry
}

// Options configures the Telemetry instance.
type Options struct {
	ServiceName   string
	OTLPEndpoint  string
	Environment   string
	LogLevel      slog.Level
	ResourceAttrs []attribute.KeyValue
}

func Default() Options {
	return Options{LogLevel: slog.LevelInfo}
}

func (o *Options) from(base Options) {
	o.ServiceName = envString("HELLNET_SERVICE", base.ServiceName)
	o.OTLPEndpoint = envString("TELEMETRY_ENDPOINT", base.OTLPEndpoint)
	o.Environment = envString("HELLNET_ENVIRONMENT", base.Environment)
	o.LogLevel = base.LogLevel
	o.ResourceAttrs = base.ResourceAttrs
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// Client é a abstração composta dos 3 sinais + lifecycle.
// Use para injeção de dependência e testes:
//
//	var c telemetry.Client = tel
//	c.Meter.Counter("req_total")        // int64 (atalho)
//	c.Meter.Float64Histogram("lat_s")   // float (superfície crua)
//	c.WithSpan("op", func(ctx context.Context) error { ... })
//	c.Error("boom", "err", err)         // log direto
//	c.Warn("slow", "latency", dur)      // log direto
//	c.Info("started", "port", port)     // log direto
//	c.Debug("debug", "detail", val)     // log direto
type Client interface {
	Log() Logger
	Trace() Tracer
	Metric() Meter
	Close() error
	WithSpan(name string, fn func(ctx context.Context) error) error
	Worker(job string, fn func(ctx context.Context) error, extra ...attribute.KeyValue) error
	// Span cria um span FILHO do ctx fornecido, executa fn e finaliza. Em erro,
	// marca o span como erro (RecordError + SetStatus). É a superfície ideal
	// para libs instrumentarem operações concretas (DB, Kafka, HTTP) dentro de
	// um trace já existente: recebe o ctx do caller e o repassa para fn.
	Span(ctx context.Context, name string, fn func(ctx context.Context) error) error
	// Direct logging convenience methods (delegam para Log().*())
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
	Debug(msg string, args ...any)

	// Counter incrementa um contador int64 (atalho: cria/obtém + Add em uma chamada).
	Counter(ctx context.Context, name string, value int64) error

	// Gauge grava um valor em um gauge int64 (atalho: cria/obtém + Record em uma chamada).
	Gauge(ctx context.Context, name string, value int64) error

	// Histogram grava um valor em um histograma int64 (atalho: cria/obtém + Record em uma chamada).
	Histogram(ctx context.Context, name string, value int64) error

	// Duration grava uma duração em segundos em um histograma float64 (atalho: cria/obtém + Record em uma chamada).
	Duration(ctx context.Context, name string, value float64) error
}

// Compile-time: *Telemetry satisfaz Client.
var _ Client = (*Telemetry)(nil)

// otlpSignalURL retorna a URL completa de um sinal OTLP (traces/metrics/logs)
// anexando o path do signal quando o ENDPOINT base não traz path. Versões
// recentes do exporter OTel HTTP (v1.45+) NÃO anexam /v1/traces automaticamente
// quando se usa WithEndpointURL com URL sem path — elas normalizam para "/",
// causando 404 no collector. Por isso anexamos o path do signal aqui.
func otlpSignalURL(base, signalPath string) string {
	if base == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = signalPath
	}
	return u.String()
}

// New creates a fully initialized Telemetry instance.
//
// # Sem parâmetros — leitura de ambiente
//
// A lib carrega tudo do ambiente: carrega o .env (dev) + lê as envs
// HELLNET_SERVICE / HELLNET_ENVIRONMENT são as únicas envs canônicas globais;
// TELEMETRY_* contém as configurações específicas de observabilidade.
// (env-first), sem receber ctx
// nem Options. Usa context.Background() como contexto-base (baseCtx).
//
// Requer HELLNET_SERVICE e TELEMETRY_ENDPOINT definidos para exportação.
func New() (*Telemetry, error) {
	ctx := context.Background()

	// Env-first: carrega o .env (dev) antes de ler as envs. O GetString apenas
	// lê os.Getenv; sem LoadDotEnv o .env do working dir nunca é carregado e a
	// lib roda em modo no-op (nada é exportado). Best-effort: sem .env ou com
	// erro de parse, cai para as env vars reais do processo.
	_ = environments.LoadDotEnv()

	o := Default()
	o.from(o)

	// Build resource with service info
	resourceAttrs := []attribute.KeyValue{
		semconvv.ServiceNameKey.String(o.ServiceName),
		semconvv.ServiceVersionKey.String("1.0.0"),
		attribute.String("deployment.environment", o.Environment),
	}

	resourceAttrs = append(resourceAttrs, o.ResourceAttrs...)

	res, err := sdkresource.New(ctx, sdkresource.WithAttributes(resourceAttrs...), sdkresource.WithTelemetrySDK())
	if err != nil {
		return nil, err
	}

	tel := &Telemetry{
		baseCtx:      ctx,
		serviceName:  o.ServiceName,
		otlpEndpoint: o.OTLPEndpoint,
		environment:  o.Environment,
	}

	// ── Logging / Tracing / Metrics ───────────────────────────────────
	if err := tel.buildLogger(o, res); err != nil {
		return nil, err
	}
	if err := tel.buildTracer(o, res); err != nil {
		return nil, err
	}
	if err := tel.buildMeter(o, res); err != nil {
		return nil, err
	}

	// Abstração de metrics (tel.Meter) — nunca nil (noop se metrics desligado).
	if tel.Meter == nil {
		tel.Meter = meterAdapter{otel.GetMeterProvider().Meter("noop")}
	}

	// Diagnóstico de startup: confirma o endpoint efetivamente lido e a
	// conectividade com o Alloy. Evita o cenário de "modo no-op silencioso"
	// (nada é exportado sem o usuário saber) que já causou confusão.
	if o.OTLPEndpoint == "" {
		tel.Logger.Warn("telemetry em modo no-op: TELEMETRY_ENDPOINT vazio, nada será exportado")
	} else {
		tel.Logger.Info("telemetry iniciado", "service", o.ServiceName, "endpoint", o.OTLPEndpoint, "otlp", true, "env", o.Environment)
		// Conectividade do Alloy já é coberta pelo check "otlp-collector"
		// embutido em runChecks (ver instrumentation.go) — não registrar duplicado.
		if err := checkOTLPReachable(ctx, o.OTLPEndpoint); err != nil {
			tel.Logger.Warn("telemetry: Alloy inacessível no startup (dados podem não chegar)",
				"endpoint", o.OTLPEndpoint, "error", err)
		}
	}

	return tel, nil
}

// MustNew is like New but panics on error. Use at startup.
func MustNew() *Telemetry {
	t, err := New()
	if err != nil {
		panic(err)
	}
	return t
}

// Close flushes telemetry data and cleans up resources. Each provider
// (logs/traces/metrics) gets a DEDICATED 5s timeout context and the three
// shut down IN PARALLEL — one slow/timing-out provider no longer consumes the
// budget of the others. Errors are aggregated in stable order
// (logs → traces → metrics). Call with defer when the service terminates.
func (t *Telemetry) Close() error {
	const shutdownTimeout = 5 * time.Second

	// shutters em ordem estável para a agregação de erros (logs → traces → metrics).
	var shutters []func(context.Context) error
	if t.lp != nil {
		shutters = append(shutters, t.lp.Shutdown)
	}
	if t.tp != nil {
		shutters = append(shutters, t.tp.Shutdown)
	}
	if t.mp != nil {
		shutters = append(shutters, t.mp.Shutdown)
	}

	// Cada provider desliga em PARALELO com orçamento próprio de 5s; escreve
	// no slot próprio e lê após Wait (happens-before via WaitGroup).
	errs := make([]error, len(shutters))
	var wg sync.WaitGroup
	for i := range shutters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			errs[i] = shutters[i](ctx)
		}()
	}
	wg.Wait()

	var agg []error
	for _, err := range errs {
		if err != nil {
			agg = append(agg, err)
		}
	}
	if len(agg) > 0 {
		return errors.Join(agg...)
	}
	return nil
}

// HealthRegister registra um health check customizado (ex.: DB, redis, downstream).
// Executado em /ready e /health; falha marca o serviço como degraded.
//
// O parâmetro check MANTÉM o signature func(ctx context.Context) error, mas o
// ctx é FORNECIDO PELA LIB na execução (derivado do request da chamada HTTP de
// health, com timeouts internos existentes) — apps não precisam gerenciá-lo.
func (t *Telemetry) HealthRegister(name string, check func(ctx context.Context) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.healthChecks == nil {
		t.healthChecks = make(map[string]func(ctx context.Context) error)
	}
	t.healthChecks[name] = check
}

// Counter incrementa um contador int64 (atalho: cria/obtém + Add em uma chamada).
func (t *Telemetry) Counter(ctx context.Context, name string, value int64) error {
	c, err := t.Meter.Counter(name)
	if err != nil {
		return err
	}
	c.Add(ctx, value)
	return nil
}

// Gauge grava um valor em um gauge int64 (atalho: cria/obtém + Record em uma chamada).
// Nota: Int64Gauge do OTel é observável (callback), para set direto use Int64ObservableGauge via RegisterCallback.
// Este atalho usa Record no Int64Gauge (compatível com OTel 1.27+).
func (t *Telemetry) Gauge(ctx context.Context, name string, value int64) error {
	g, err := t.Meter.Int64Gauge(name)
	if err != nil {
		return err
	}
	g.Record(ctx, value)
	return nil
}

// Histogram grava um valor em um histograma int64 (atalho: cria/obtém + Record em uma chamada).
func (t *Telemetry) Histogram(ctx context.Context, name string, value int64) error {
	h, err := t.Meter.Int64Histogram(name)
	if err != nil {
		return err
	}
	h.Record(ctx, value)
	return nil
}

// Duration grava uma duração em segundos em um histograma float64 (atalho: cria/obtém + Record em uma chamada).
func (t *Telemetry) Duration(ctx context.Context, name string, value float64) error {
	h, err := t.Meter.Float64Histogram(name)
	if err != nil {
		return err
	}
	h.Record(ctx, value)
	return nil
}
