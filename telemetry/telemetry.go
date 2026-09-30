// Package telemetry provides opinionated OpenTelemetry observability for Go services.
//
// Usage (sem parâmetros — a lib lê tudo diretamente do ambiente):
//
//	ctx := context.Background()
//	tel, err := telemetry.New(ctx)
//	defer tel.Close(ctx)
//
//	// Tracing (context-first)
//	err := tel.Trace(ctx).Span("operation", func(ctx context.Context) error {
//		span := trace.SpanFromContext(ctx) // continues this span
//		return doWork(ctx)
//	})
//
//	// Metrics
//	_ = tel.Metric(ctx).Counter("requests.total", 1)
//
//	// Logging (via Zap → stdout + OTLP → Loki), correlated with the
//	// base-context trace lineage internally
//	tel.Log(ctx).Info("processing", "id", orderID)
//
// Context-first operations preserve the caller's distributed trace. The
// Every operation receives the caller's context.
package telemetry

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/internal/env"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconvv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Telemetry wraps OpenTelemetry primitives (tracer, meter, logger)
// pre-configured for the service.
type Telemetry struct {
	tracer trace.Tracer
	meter  metric.Meter
	Logger *zap.SugaredLogger

	serviceName  string
	otlpEndpoint string
	environment  string
	mu           sync.RWMutex
	healthChecks map[string]func(ctx context.Context) error

	healthStatusMu sync.Mutex
	healthStatus   map[string]int64
	logMu          sync.Mutex
	logErrors      metric.Int64Counter

	lp *sdklog.LoggerProvider
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider

	shutdownOnce             sync.Once
	shutdownErr              error
	includeHealthCheckErrors bool
}

// Options configures the Telemetry instance.
type Options struct {
	ServiceName              string
	ServiceVersion           string
	OTLPEndpoint             string
	Environment              string
	LogLevel                 zapcore.Level
	ResourceAttrs            []attribute.KeyValue
	OTLPHeaders              map[string]string
	IncludeHealthCheckErrors bool
}

// Telemetry is the single application facade for logs, metrics, traces and lifecycle.

// Telemetry is the single application facade for logs, metrics, traces and
// lifecycle. Context-aware methods are the public API; implementation details
// remain private to this package.

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
// A lib lê diretamente as envs HELLNET_TELEMETRY_*, HELLNET_* e OTEL_*, sem
// carregar arquivos .env ou depender de uma biblioteca externa de ambiente.
//
// Requer HELLNET_TELEMETRY_SERVICE (ou HELLNET_SERVICE) e
// HELLNET_TELEMETRY_ENDPOINT (ou HELLNET_ENDPOINT) definidos.
func New(ctx context.Context) (*Telemetry, error) {
	prefixes := []string{"HELLNET_TELEMETRY_", "HELLNET_"}
	o := Options{
		ServiceName:    prefixed(prefixes, "SERVICE", env.String("OTEL_SERVICE_NAME", "telemetry")),
		ServiceVersion: prefixed(prefixes, "SERVICE_VERSION", env.String("OTEL_SERVICE_VERSION", "")),
		OTLPEndpoint:   prefixed(prefixes, "ENDPOINT", env.String("OTEL_EXPORTER_OTLP_ENDPOINT", "")),
		Environment:    prefixed(prefixes, "ENVIRONMENT", env.String("OTEL_DEPLOYMENT_ENVIRONMENT", "")),
		OTLPHeaders:    parseOTLPHeaders(prefixed(prefixes, "HEADERS", env.String("OTEL_EXPORTER_OTLP_HEADERS", ""))),
		LogLevel:       zapcore.InfoLevel,
	}
	return NewWithOptions(ctx, o)
}

func prefixed(prefixes []string, key, fallback string) string {
	for _, prefix := range prefixes {
		if value := env.String(prefix+key, ""); value != "" {
			return value
		}
	}
	return fallback
}

func parseOTLPHeaders(raw string) map[string]string {
	result := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if ok && strings.TrimSpace(key) != "" {
			result[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return result
}

// NewWithOptions creates telemetry with an explicit context and options.
func NewWithOptions(ctx context.Context, o Options) (*Telemetry, error) {
	if ctx == nil {
		return nil, errors.New("telemetry: context is required")
	}
	if o.LogLevel == 0 {
		o.LogLevel = zapcore.InfoLevel
	}
	if o.ServiceName == "" {
		return nil, errors.New("telemetry: service name is required")
	}

	version := o.ServiceVersion
	if version == "" {
		version = "unknown"
	}

	resourceAttrs := []attribute.KeyValue{
		semconvv.ServiceNameKey.String(o.ServiceName),
		semconvv.ServiceVersionKey.String(version),
	}
	if o.Environment != "" {
		resourceAttrs = append(resourceAttrs, semconvv.DeploymentEnvironmentNameKey.String(o.Environment))
	}

	resourceAttrs = append(resourceAttrs, o.ResourceAttrs...)

	res, err := sdkresource.New(ctx, sdkresource.WithAttributes(resourceAttrs...), sdkresource.WithTelemetrySDK())
	if err != nil {
		return nil, err
	}

	tel := &Telemetry{
		serviceName:              o.ServiceName,
		otlpEndpoint:             o.OTLPEndpoint,
		environment:              o.Environment,
		includeHealthCheckErrors: o.IncludeHealthCheckErrors,
	}

	// ── Logging / Tracing / Metrics ───────────────────────────────────
	if err := tel.buildLogger(ctx, o, res); err != nil {
		return nil, err
	}
	if err := tel.buildTracer(ctx, o, res); err != nil {
		return nil, err
	}
	if err := tel.buildMeter(ctx, o, res); err != nil {
		return nil, err
	}

	// Meter nunca fica nil (noop se metrics desligado).
	if tel.meter == nil {
		tel.meter = otel.GetMeterProvider().Meter("noop")
	}

	// Diagnóstico de startup: confirma o endpoint efetivamente lido e a
	// conectividade com o Alloy. Evita o cenário de "modo no-op silencioso"
	// (nada é exportado sem o usuário saber) que já causou confusão.
	if o.OTLPEndpoint == "" {
		tel.Log(ctx).Warn("telemetry em modo no-op: HELLNET_TELEMETRY_ENDPOINT vazio, nada será exportado")
	} else {
		tel.Log(ctx).Info("telemetry iniciado", "service", o.ServiceName, "endpoint", o.OTLPEndpoint, "otlp", true, "env", o.Environment)
		// Conectividade do Alloy já é coberta pelo check "otlp-collector"
		// embutido em runChecks (ver instrumentation.go) — não registrar duplicado.
		if err := checkOTLPReachable(ctx, o.OTLPEndpoint); err != nil {
			tel.Log(ctx).Warn("telemetry: Alloy inacessível no startup (dados podem não chegar)",
				"endpoint", o.OTLPEndpoint, "error", err)
		}
	}

	return tel, nil
}

// MustNew is like New but panics on error. Use at startup.
func MustNew(ctx context.Context) *Telemetry {
	t, err := New(ctx)
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
func (t *Telemetry) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("telemetry: close context is required")
	}
	t.shutdownOnce.Do(func() {
		shutdownTimeout := env.Duration("HELLNET_TELEMETRY_SHUTDOWN_TIMEOUT", 5*time.Second)
		if shutdownTimeout <= 0 {
			shutdownTimeout = 5 * time.Second
		}
		shutdownContext := context.WithoutCancel(ctx)

		// shutters em ordem estável para a agregação de erros (logs → traces → metrics).
		var shutters []func(context.Context) error
		if t.lp != nil {
			shutters = append(shutters, func(c context.Context) error {
				return errors.Join(t.lp.ForceFlush(c), t.lp.Shutdown(c))
			})
		}
		if t.tp != nil {
			shutters = append(shutters, func(c context.Context) error {
				return errors.Join(t.tp.ForceFlush(c), t.tp.Shutdown(c))
			})
		}
		if t.mp != nil {
			shutters = append(shutters, func(c context.Context) error {
				return errors.Join(t.mp.ForceFlush(c), t.mp.Shutdown(c))
			})
		}
		// Cada provider desliga em PARALELO com orçamento próprio de 5s; escreve
		// no slot próprio e lê após Wait (happens-before via WaitGroup).
		errs := make([]error, len(shutters))
		var wg sync.WaitGroup
		for i := range shutters {
			wg.Go(func() {
				shutdownCtx, cancel := context.WithTimeout(shutdownContext, shutdownTimeout)
				defer cancel()
				errs[i] = shutters[i](shutdownCtx)
			})
		}
		wg.Wait()

		var agg []error
		for _, err := range errs {
			if err != nil {
				agg = append(agg, err)
			}
		}
		if len(agg) > 0 {
			t.shutdownErr = errors.Join(agg...)
		}
	})
	return t.shutdownErr
}

// ForceFlush exports all telemetry currently buffered by the SDK providers.
// The caller controls the deadline through ctx.
func (t *Telemetry) ForceFlush(ctx context.Context) error {
	if ctx == nil {
		return errors.New("telemetry: flush context is required")
	}
	var flushers []func(context.Context) error
	if t.lp != nil {
		flushers = append(flushers, t.lp.ForceFlush)
	}
	if t.tp != nil {
		flushers = append(flushers, t.tp.ForceFlush)
	}
	if t.mp != nil {
		flushers = append(flushers, t.mp.ForceFlush)
	}
	errs := make([]error, len(flushers))
	var wg sync.WaitGroup
	for i := range flushers {
		wg.Go(func() { errs[i] = flushers[i](ctx) })
	}
	wg.Wait()
	var joined []error
	for _, err := range errs {
		if err != nil {
			joined = append(joined, err)
		}
	}
	return errors.Join(joined...)
}

// HealthRegister registra um health check customizado (ex.: DB, redis, downstream).
// Executado em /ready e /health; falha marca o serviço como degraded. O
// collector OTLP é verificado apenas por /health e não bloqueia readiness.
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
