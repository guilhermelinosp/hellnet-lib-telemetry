// Package telemetry provides opinionated OpenTelemetry observability for Go services.
//
// Usage (sem parâmetros — a lib lê tudo do ambiente: .env + HELLNET_*):
//
//	tel, err := telemetry.New()
//	defer tel.Shutdown()
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

	// profiler é o profiler Pyroscope (push), iniciado via ProfilesStart e
	// parado em Shutdown.
	profiler pyroscopeProfiler
}

// Options configures the Telemetry instance.
type Options struct {
	ServiceName   string
	OTLPEndpoint  string
	Environment   string
	LogLevel      slog.Level
	ResourceAttrs []attribute.KeyValue
}

// Client é a abstração composta dos 3 sinais + lifecycle.
// Use para injeção de dependência e testes:
//
//	var c telemetry.Client = tel
//	c.Meter.Counter("req_total")        // int64 (atalho)
//	c.Meter.Float64Histogram("lat_s")   // float (superfície crua)
//	c.WithSpan("op", func(ctx context.Context) error { ... })
//	c.Log().Error("boom", "err", err)
type Client interface {
	Log() Logger
	Trace() Tracer
	Metric() Meter
	Shutdown() error
	WithSpan(name string, fn func(ctx context.Context) error) error
	Worker(job string, fn func(ctx context.Context) error, extra ...attribute.KeyValue) error
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
// HELLNET_TELEMETRY_* / HELLNET_* obrigatórias (env-first), sem receber ctx
// nem Options. Usa context.Background() como contexto-base (baseCtx).
//
// Requer HELLNET_TELEMETRY_SERVICE (ou HELLNET_SERVICE) e
// HELLNET_TELEMETRY_ENDPOINT (ou HELLNET_ENDPOINT) definidos.
func New() (*Telemetry, error) {
	ctx := context.Background()

	// Env-first: carrega o .env (dev) antes de ler as envs. O GetString apenas
	// lê os.Getenv; sem LoadDotEnv o .env do working dir nunca é carregado e a
	// lib roda em modo no-op (nada é exportado). Best-effort: sem .env ou com
	// erro de parse, cai para as env vars reais do processo.
	_ = environments.LoadDotEnv()

	o := Options{
		ServiceName:  environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "SERVICE", ""),
		OTLPEndpoint: environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "ENDPOINT", ""),
		Environment:  environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "ENVIRONMENT", ""),
		LogLevel:     slog.LevelInfo,
	}

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
		tel.Logger.Warn("telemetry em modo no-op: HELLNET_TELEMETRY_ENDPOINT vazio, nada será exportado")
	} else {
		tel.Logger.Info("telemetry iniciado", "service", o.ServiceName, "endpoint", o.OTLPEndpoint, "otlp", true, "profiling", "auto", "env", o.Environment)
		// Health check de conectividade com o Alloy, executado em /ready e /health.
		tel.HealthRegister("otlp-collector", func(c context.Context) error {
			return checkOTLPReachable(c, o.OTLPEndpoint)
		})
		if err := checkOTLPReachable(ctx, o.OTLPEndpoint); err != nil {
			tel.Logger.Warn("telemetry: Alloy inacessível no startup (dados podem não chegar)",
				"endpoint", o.OTLPEndpoint, "error", err)
		}
	}

	// Profiling push (Pyroscope): inicia automaticamente quando há collector
	// OTLP configurado. Se não houver endpoint, fica desligado silenciosamente
	// (não falha o New — profiling é best-effort).
	if o.OTLPEndpoint != "" {
		if _, err := tel.ProfilesStart(); err != nil {
			tel.Logger.Warn("telemetry: profiling não iniciado", "error", err)
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

// Shutdown flushes telemetry data and cleans up resources. Each provider
// (logs/traces/metrics) gets a DEDICATED 5s timeout context and the three
// shut down IN PARALLEL — one slow/timing-out provider no longer consumes the
// budget of the others. Errors are aggregated in stable order
// (logs → traces → metrics). Call with defer when the service terminates.
func (t *Telemetry) Shutdown() error {
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
	if t.profiler != nil {
		shutters = append(shutters, func(context.Context) error { return t.profiler.Stop() })
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
