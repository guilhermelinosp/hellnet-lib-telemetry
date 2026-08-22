// Package telemetry provides opinionated OpenTelemetry observability for Go services.
//
// Usage:
//
//	tel := telemetry.New("my-service")
//	defer tel.Shutdown()
//
//	// Tracing
//	ctx, span := tel.Tracer.Start(ctx, "operation")
//	span.SetAttributes(attribute.String("key", "val"))
//	span.End()
//
//	// Metrics
//	counter, _ := tel.Meter.Counter("requests.total")
//	counter.Add(ctx, 1)
//
//	// Logging (via slog → stdout + OTLP → Loki)
//	tel.Logger.InfoContext(ctx, "processing", "id", orderID)
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	environments "github.com/guilhermelinosp/hellnet-lib-environments/environments"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconvv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry wraps OpenTelemetry primitives (tracer, meter, logger)
// pre-configured for the service.
type Telemetry struct {
	Tracer trace.Tracer
	Meter  Meter
	Logger *slog.Logger

	serviceName  string
	otlpEndpoint string
	mu           sync.RWMutex
	healthChecks map[string]func(ctx context.Context) error

	// healthStatus guarda o último status (1=pass, 0=fail) de cada health check,
	// lido por um Int64ObservableGauge (healthcheck_status) no momento do scrape/
	// export — garante exportação confiável em OTLP e Prometheus (gauges
	// síncronos com atributo não são exportados de forma consistente pelo otelprom).
	healthStatusMu sync.Mutex
	healthStatus   map[string]int64

	lp *sdklog.LoggerProvider
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider

	// promRegistry guarda o registry Prometheus quando PrometheusExporter está
	// ligado; usado por Metrics() para servir /metrics.
	promRegistry *prometheus.Registry
}

// Options configures the Telemetry instance.
type Options struct {
	ServiceName   string
	OTLPEndpoint  string
	LogLevel      slog.Level
	ResourceAttrs []attribute.KeyValue

	// Enabled liga TODOS os sinais (trace + metrics + logs) de uma vez.
	// false = nada habilitado. Não há controle individual por sinal.
	Enabled bool

	// RegisterGlobals registra os providers no estado global do otel/slog.
	// Opt-in (default false). Quando false, use tel.* / telemetry.Client diretamente
	// (recomendado para DI/mock). Quando true, reproduz o comportamento anterior.
	RegisterGlobals bool

	// RuntimeMetrics liga o conjunto padrão de métricas de runtime/processo
	// (memória, GC, CPU, goroutines, fd, threads, uptime) — ver startRuntimeMetrics.
	//
	// Por padrão (prometheus-net-like) esse conjunto JÁ VEM LIGADO quando
	// Enabled=true. Passe RuntimeMetrics: false para desligá-lo de forma explícita.
	RuntimeMetrics bool

	// RedactSensitive mascara valores de atributos sensíveis nos logs
	// (password, token, secret, authorization, etc.). Veja defaultSensitiveKeys.
	RedactSensitive bool

	// RedactKeys adiciona chaves customizadas à lista de redação.
	RedactKeys []string

	// PrometheusExporter liga um exporter Prometheus à mesma MeterProvider,
	// expondo as métricas da lib em formato de scrape (pull) para um
	// endpoint /metrics. Útil para inspecionar p99/CPU/GC/health checks sem
	// um collector OTLP (ex.: durante testes locais). Veja Telemetry.Metrics().
	//
	// Por padrão (Default) vem LIGADO. Para desligar, passe Options com
	// PrometheusExporter: false.
	PrometheusExporter bool
}

// LoadFromEnv returns options populated from HELLNET_TELEMETRY_* env vars
// (fallback para HELLNET_* por retrocompatibilidade).
// Lê SERVICE, ENDPOINT e compõe o endereço OTLP com
// PORT (quando presente): ENDPOINT[:PORT].
func LoadFromEnv() Options {
	return Options{
		ServiceName:        environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "SERVICE", ""),
		OTLPEndpoint:       otlpEndpointFromEnv(),
		LogLevel:           slog.LevelInfo,
		Enabled:            true,
		PrometheusExporter: true,
	}
}

// Validate garante o contrato mínimo obrigatório: SERVICE e ENDPOINT.
// PORT e ENVIRONMENT são opcionais (ENDPOINT pode já carregar a porta;
// ENVIRONMENT ausente é tratado como dev pelo Loading). Aceita prefixo
// HELLNET_TELEMETRY_* (preferido) ou HELLNET_* (fallback). Deve ser chamado
// por apps reais (ex.: example) antes de New(); New() permanece flexível para
// uso em testes/embedded. Retorna erro listando as envs faltantes.
func (o Options) Validate() error {
	required := []string{"SERVICE", "ENDPOINT"}
	var missing []string
	for _, suffix := range required {
		if environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", suffix, "") == "" {
			missing = append(missing, "HELLNET_TELEMETRY_"+suffix)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("envs obrigatórias não definidas: %v", missing)
	}
	return nil
}

// otlpEndpointFromEnv retorna o ENDPOINT OTLP tal como configurado. A porta
// deve vir junto do próprio ENDPOINT (ex.: https://collector:443); não há
// variável de porta separada — a lib não aceita HELLNET_*_PORT.
func otlpEndpointFromEnv() string {
	return environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "ENDPOINT", "")
}

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

// loadEnvFiles carrega o .env de desenvolvimento (dev) antes de ler as envs,
// espelhando o padrão de hellnet-lib-cache. Erros de leitura são ignorados
// (o .env é opcional em runtime).
func loadEnvFiles() {
	_ = environments.LoadDotEnv("HELLNET_TELEMETRY_ENV_FILE", "HELLNET_ENV_FILE")
}

// New creates a fully initialized Telemetry instance.
// Sem argumentos, carrega o .env (dev) + lê as envs HELLNET_*
// obrigatórias (env-first) e usa telemetry.LoadFromEnv(). Com Options explícitas,
// usa-as diretamente (sobrescrevendo o env — útil em testes/embed).
func New(opts ...Options) (*Telemetry, error) {
	// Env-first: sempre carrega .env (dev) + lê envs; opts sobrescrevem.
	loadEnvFiles()

	o := LoadFromEnv()
	if len(opts) > 0 {
		o = opts[0]
	}

	if o.ServiceName == "" {
		return nil, ErrMissingServiceName
	}

	// Build resource with service info
	resourceAttrs := []attribute.KeyValue{
		semconvv.ServiceNameKey.String(o.ServiceName),
		semconvv.ServiceVersionKey.String(buildVersion()),
		attribute.String("deployment.environment", deploymentEnv()),
	}
	resourceAttrs = append(resourceAttrs, o.ResourceAttrs...)
	res, err := sdkresource.New(
		context.Background(),
		sdkresource.WithAttributes(resourceAttrs...),
		sdkresource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}

	tel := &Telemetry{
		serviceName:  o.ServiceName,
		otlpEndpoint: o.OTLPEndpoint,
	}

	// ── Logging / Tracing / Metrics ───────────────────────────────────
	if o.Enabled {
		if err := tel.buildLogger(o, res); err != nil {
			return nil, err
		}
		if err := tel.buildTracer(o, res); err != nil {
			return nil, err
		}
		if err := tel.buildMeter(o, res); err != nil {
			return nil, err
		}
	}

	// Abstração de metrics (tel.Meter) — nunca nil (noop se metrics desligado).
	if tel.Meter == nil {
		tel.Meter = meterAdapter{otel.GetMeterProvider().Meter("noop")}
	}

	return tel, nil
}

// MustNew is like New but panics on error. Use at startup.
func MustNew(opts ...Options) *Telemetry {
	t, err := New(opts...)
	if err != nil {
		panic(err)
	}
	return t
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
	var stdoutHandler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: o.LogLevel,
	})
	if o.RedactSensitive || len(o.RedactKeys) > 0 {
		stdoutHandler = redactHandler{stdoutHandler, redactKeys(o)}
	}
	stdoutHandler = contextTraceHandler{stdoutHandler}

	// OTel bridge handler (sends via OTLP → Alloy → Loki)
	var otelHandler slog.Handler = otelslog.NewHandler("otel", otelslog.WithLoggerProvider(lp))
	if o.RedactSensitive || len(o.RedactKeys) > 0 {
		otelHandler = redactHandler{otelHandler, redactKeys(o)}
	}

	// MultiHandler: writes to BOTH stdout AND OTLP; errorCountHandler
	// conta erros logados automaticamente (log_errors_total).
	t.Logger = slog.New(&errorCountHandler{Handler: slog.NewMultiHandler(stdoutHandler, otelHandler), tel: t})
	if o.RegisterGlobals {
		slog.SetDefault(t.Logger)
	}
	return nil
}

// buildTracer monta o TracerProvider e o propagador de contexto (opt-in global).
func (t *Telemetry) buildTracer(o Options, res *sdkresource.Resource) error {
	tp, err := newTracerProvider(o, res)
	if err != nil {
		return err
	}
	t.tp = tp
	t.Tracer = tp.Tracer(o.ServiceName)
	if o.RegisterGlobals {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
	}
	return nil
}

// buildMeter monta o MeterProvider (OTLP + Prometheus), os runtime metrics e
// as métricas de health check. Runtime metrics vêm ligadas por padrão
// (estilo prometheus-net); passe RuntimeMetrics: false para desligar.
func (t *Telemetry) buildMeter(o Options, res *sdkresource.Resource) error {
	mp, promReg, err := newMeterProvider(o, res)
	if err != nil {
		return err
	}
	t.mp = mp
	t.promRegistry = promReg
	t.Meter = meterAdapter{mp.Meter(o.ServiceName)}
	if o.RegisterGlobals {
		otel.SetMeterProvider(mp)
	}
	if o.RuntimeMetrics {
		t.startRuntimeMetrics()
	}
	t.registerHealthMetrics()
	return nil
}

// Shutdown flushes telemetry data and cleans up resources with a 5s timeout.
// Call with defer when the service terminates.
func (t *Telemetry) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var errs []error
	if t.lp != nil {
		if err := t.lp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if t.tp != nil {
		if err := t.tp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if t.mp != nil {
		if err := t.mp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// MetricsHandler returns an http.Handler that serves the library's metrics in
// Prometheus exposition format (text/plain), for scraping via a /metrics
// endpoint. Requer Options.PrometheusExporter=true (ligado por padrão em
// Default()). Permite inspecionar as métricas (p99 de latência/worker, CPU,
// GC, health checks, etc.) sem um collector OTLP — ideal durante testes locais.
//
// Exemplo:
//
//	mux.Handle("GET /metrics", tel.MetricsHandler())
//
// Nota: não pode chamar-se Metrics() pois o acessor do meter (Client.Metrics()
// Meter) já ocupa esse nome no conjunto de métodos do Telemetry.
func (t *Telemetry) MetricsHandler() http.Handler {
	if t.promRegistry != nil {
		return promhttp.HandlerFor(t.promRegistry, promhttp.HandlerOpts{})
	}
	// Fallback: /metrics vazio (PrometheusExporter desligado).
	return promhttp.Handler()
}

// HealthRegister registra um health check customizado (ex.: DB, redis, downstream).
// Executado em /ready e /health; falha marca o serviço como degraded.
func (t *Telemetry) HealthRegister(name string, check func(ctx context.Context) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.healthChecks == nil {
		t.healthChecks = make(map[string]func(ctx context.Context) error)
	}
	t.healthChecks[name] = check
}

// WithSpan cria um span, executa fn e finaliza. Em erro, marca o span com o status.
func (t *Telemetry) WithSpan(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	ctx, span := t.Trace().Start(ctx, name)
	defer span.End()
	// Recupera panics automaticamente, contabilizando exceções
	// (exceptions_total) e marcando o span como erro, preservando o
	// comportamento original ao re-propagar o panic.
	defer func() {
		if r := recover(); r != nil {
			if t.Meter != nil {
				if c, err := t.Meter.Counter("exceptions_total"); err == nil {
					c.Add(ctx, 1, metric.WithAttributes(
						attribute.String("span", name),
						attribute.String("kind", "panic"),
					))
				}
			}
			span.RecordError(fmt.Errorf("%v", r))
			span.SetStatus(codes.Error, fmt.Sprintf("%v", r))
			panic(r)
		}
	}()
	if err := fn(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// gcPauseBoundaries são buckets explícitos (segundos) para pausas de GC
// (tipicamente µs a dezenas de ms), habilitando p99 da pausa de GC.
var gcPauseBoundaries = []float64{
	1e-5, 5e-5, 1e-4, 5e-4, 1e-3, 5e-3, 1e-2, 5e-2, 1e-1, 5e-1, 1,
}

// startRuntimeMetrics registra um conjunto abrangente de métricas de
// runtime/processo via callback do SDK. Aplica-se a QUALQUER processo
// (API, CLI, lambda, script) — não depende de Worker/Middleware.
//
// Memória (bytes): heap (alloc/sys/inuse/released/objects), stack, mspan,
// mcache, other_sys, gc_sys, sys total, total_alloc (cumulativo).
// GC: ciclos totais, forçados, pausa total, fração de CPU, e HISTOGRAMA de
// pausa individual (→ p99 da pausa de GC). CPU: uso do processo (% e razão).
// Além de goroutines, num_cpu e uptime.
func (t *Telemetry) startRuntimeMetrics() {
	m := t.Meter

	goroutines, _ := m.Int64ObservableGauge("process_goroutines", metric.WithDescription("Number of goroutines"))
	heapAlloc, _ := m.Int64ObservableGauge("process_heap_alloc_bytes", metric.WithDescription("Bytes of allocated heap objects"))
	heapSys, _ := m.Int64ObservableGauge("process_heap_sys_bytes", metric.WithDescription("Bytes of heap memory obtained from the OS"))
	heapInuse, _ := m.Int64ObservableGauge("process_heap_inuse_bytes", metric.WithDescription("Bytes of heap memory in use"))
	heapReleased, _ := m.Int64ObservableGauge("process_heap_released_bytes", metric.WithDescription("Bytes of heap memory released to the OS"))
	heapObjects, _ := m.Int64ObservableGauge("process_heap_objects", metric.WithDescription("Number of allocated heap objects"))
	stackInuse, _ := m.Int64ObservableGauge("process_stack_inuse_bytes", metric.WithDescription("Bytes of stack memory in use"))
	stackSys, _ := m.Int64ObservableGauge("process_stack_sys_bytes", metric.WithDescription("Bytes of stack memory obtained from the OS"))
	mspanInuse, _ := m.Int64ObservableGauge("process_mspan_inuse_bytes", metric.WithDescription("Bytes of mspan structures in use"))
	mspanSys, _ := m.Int64ObservableGauge("process_mspan_sys_bytes", metric.WithDescription("Bytes of mspan structures obtained from the OS"))
	mcacheInuse, _ := m.Int64ObservableGauge("process_mcache_inuse_bytes", metric.WithDescription("Bytes of mcache structures in use"))
	mcacheSys, _ := m.Int64ObservableGauge("process_mcache_sys_bytes", metric.WithDescription("Bytes of mcache structures obtained from the OS"))
	otherSys, _ := m.Int64ObservableGauge("process_other_sys_bytes", metric.WithDescription("Bytes of memory for other runtime allocations"))
	gcSys, _ := m.Int64ObservableGauge("process_gc_sys_bytes", metric.WithDescription("Bytes of memory used for GC metadata"))
	sysTotal, _ := m.Int64ObservableGauge("process_sys_bytes", metric.WithDescription("Total bytes of memory obtained from the OS"))
	totalAlloc, _ := m.Int64ObservableGauge("process_total_alloc_bytes", metric.WithDescription("Cumulative bytes allocated (including freed)"))
	gcTotal, _ := m.Int64ObservableGauge("process_gc_total", metric.WithDescription("Total number of completed GC cycles"))
	gcForced, _ := m.Int64ObservableGauge("process_gc_forced_total", metric.WithDescription("Total number of forced GC cycles"))
	gcPauseTotal, _ := m.Float64ObservableGauge("process_gc_pause_total_seconds", metric.WithDescription("Cumulative GC pause time in seconds"))
	gcCPUFraction, _ := m.Float64ObservableGauge("process_gc_cpu_fraction", metric.WithDescription("Fraction of CPU time spent in GC (0..1)"))
	cpuPercent, _ := m.Float64ObservableGauge("process_cpu_usage_percent", metric.WithDescription("Process CPU usage as percentage of one core"))
	cpuRatio, _ := m.Float64ObservableGauge("process_cpu_usage_ratio", metric.WithDescription("Process CPU usage as ratio of one core (0..N)"))
	numCPU, _ := m.Int64ObservableGauge("process_num_cpu", metric.WithDescription("Number of logical CPUs visible to the process"))
	uptime, _ := m.Float64ObservableGauge("process_uptime_seconds", metric.WithDescription("Seconds since the process started"))
	openFds, _ := m.Int64ObservableGauge("process_open_fds", metric.WithDescription("Number of open file descriptors"))
	threads, _ := m.Int64ObservableGauge("process_threads", metric.WithDescription("Number of OS threads"))

	// Histograma de pausa de GC individual (não observável: Record por ciclo).
	gcPauseHist, _ := m.Float64Histogram(
		"process_gc_pause_seconds",
		metric.WithExplicitBucketBoundaries(gcPauseBoundaries...),
		metric.WithDescription("Distribution of individual GC pause durations"),
	)

	start := time.Now()
	var (
		mu         sync.Mutex
		lastNumGC  uint32
		prevCPUNs  int64
		prevWallNs int64
		cpuInit    bool
	)

	_, _ = m.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)

			o.ObserveInt64(goroutines, int64(runtime.NumGoroutine()))
			o.ObserveInt64(heapAlloc, int64(ms.Alloc))
			o.ObserveInt64(heapSys, int64(ms.HeapSys))
			o.ObserveInt64(heapInuse, int64(ms.HeapInuse))
			o.ObserveInt64(heapReleased, int64(ms.HeapReleased))
			o.ObserveInt64(heapObjects, int64(ms.HeapObjects))
			o.ObserveInt64(stackInuse, int64(ms.StackInuse))
			o.ObserveInt64(stackSys, int64(ms.StackSys))
			o.ObserveInt64(mspanInuse, int64(ms.MSpanInuse))
			o.ObserveInt64(mspanSys, int64(ms.MSpanSys))
			o.ObserveInt64(mcacheInuse, int64(ms.MCacheInuse))
			o.ObserveInt64(mcacheSys, int64(ms.MCacheSys))
			o.ObserveInt64(otherSys, int64(ms.OtherSys))
			o.ObserveInt64(gcSys, int64(ms.GCSys))
			o.ObserveInt64(sysTotal, int64(ms.Sys))
			o.ObserveInt64(totalAlloc, int64(ms.TotalAlloc))
			o.ObserveInt64(gcTotal, int64(ms.NumGC))
			o.ObserveInt64(gcForced, int64(ms.NumForcedGC))
			o.ObserveFloat64(gcPauseTotal, float64(ms.PauseTotalNs)/1e9)
			o.ObserveFloat64(gcCPUFraction, ms.GCCPUFraction)
			o.ObserveInt64(numCPU, int64(runtime.NumCPU()))
			o.ObserveFloat64(uptime, time.Since(start).Seconds())
			if n, err := readOpenFds(); err == nil {
				o.ObserveInt64(openFds, n)
			}
			if n, err := readThreads(); err == nil {
				o.ObserveInt64(threads, n)
			}

			mu.Lock()

			// GC pause histogram: registra apenas as pausas novas desde o último sample.
			delta := ms.NumGC - lastNumGC
			if delta > 0 {
				if delta > 256 {
					delta = 256 // buffer circular do runtime
				}
				for i := uint32(1); i <= delta; i++ {
					idx := (ms.NumGC - i) % 256
					gcPauseHist.Record(ctx, float64(ms.PauseNs[idx])/1e9)
				}
				lastNumGC = ms.NumGC
			}

			// CPU: (utime+stime) do rusage delta / wall delta.
			if cpuNs, err := readProcessCPUNs(); err == nil {
				now := time.Now().UnixNano()
				if cpuInit && now > prevWallNs {
					ratio := float64(cpuNs-prevCPUNs) / float64(now-prevWallNs)
					o.ObserveFloat64(cpuRatio, ratio)
					o.ObserveFloat64(cpuPercent, ratio*100)
				}
				prevCPUNs, prevWallNs, cpuInit = cpuNs, now, true
			}

			mu.Unlock()
			return nil
		},
		goroutines, heapAlloc, heapSys, heapInuse, heapReleased, heapObjects,
		stackInuse, stackSys, mspanInuse, mspanSys, mcacheInuse, mcacheSys,
		otherSys, gcSys, sysTotal, totalAlloc, gcTotal, gcForced,
		gcPauseTotal, gcCPUFraction, cpuPercent, cpuRatio, numCPU, uptime,
		openFds, threads,
	)
}

// readProcessCPUNs retorna o tempo de CPU (usuário + sistema) do processo em
// nanosegundos, lendo /proc/self/stat (Linux). Em outras plataformas retorna
// erro e a métrica de CPU (process_cpu_usage_*) simplesmente não é emitida —
// sem build tags separados, mantendo o número de arquivos baixo (DRY/KISS).
// utime/stime vêm em clock ticks; converte-se assumindo USER_HZ=100 (padrão da
// maioria dos kernels Linux).
func readProcessCPUNs() (int64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, os.ErrInvalid
	}
	fields := strings.Fields(s[idx+1:])
	// Após o comm: state, ppid, pgrp, session, tty, tpgid, flags, minflt,
	// cminflt, majflt, cmajflt, utime(índice 11), stime(índice 12), ...
	if len(fields) < 13 {
		return 0, os.ErrInvalid
	}
	utime, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	const nsPerTick = int64(10_000_000) // 1e9 ns / USER_HZ(100)
	return (utime + stime) * nsPerTick, nil
}

// defaultSensitiveKeys são mascaradas quando RedactSensitive=true.
var defaultSensitiveKeys = []string{
	"password", "passwd", "pwd", "token", "secret",
	"authorization", "api_key", "apikey", "auth", "cookie",
}

func redactKeys(opts Options) map[string]struct{} {
	keys := make(map[string]struct{})
	if opts.RedactSensitive {
		for _, k := range defaultSensitiveKeys {
			keys[strings.ToLower(k)] = struct{}{}
		}
	}
	for _, k := range opts.RedactKeys {
		keys[strings.ToLower(k)] = struct{}{}
	}
	return keys
}

// redactHandler mascara o valor de atributos sensíveis nos registros slog.
type redactHandler struct {
	slog.Handler
	keys map[string]struct{}
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if _, ok := h.keys[strings.ToLower(a.Key)]; ok {
			nr.AddAttrs(slog.Attr{Key: a.Key, Value: slog.StringValue("[REDACTED]")})
		} else {
			nr.AddAttrs(a)
		}
		return true
	})
	return h.Handler.Handle(ctx, nr)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return redactHandler{h.Handler.WithAttrs(attrs), h.keys}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{h.Handler.WithGroup(name), h.keys}
}

// contextTraceHandler enriquece registros slog com trace_id/span_id quando há
// um span ativo no contexto, correlacionando logs com traces no stdout (o
// otelslog já cobre o sink OTLP). WithAttrs/WithGroup reaplicam o wrapper
// para handlers derivados manterem a injeção.
type contextTraceHandler struct {
	slog.Handler
}

func (h contextTraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextTraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextTraceHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextTraceHandler) WithGroup(name string) slog.Handler {
	return contextTraceHandler{h.Handler.WithGroup(name)}
}

// errorCountHandler conta automaticamente erros (nível >= Error) emitidos via
// slog, expondo log_errors_total. O contador é criado preguiçosamente no
// primeiro Handle, quando tel.Meter já está disponível (o bloco de logging é
// construído antes do de métricas no New).
type errorCountHandler struct {
	slog.Handler
	tel *Telemetry
	mu  sync.Mutex
	cnt metric.Int64Counter
}

func (h *errorCountHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && h.tel != nil {
		h.mu.Lock()
		c := h.cnt
		if c == nil && h.tel.Meter != nil {
			c, _ = h.tel.Meter.Counter("log_errors_total")
			h.cnt = c
		}
		h.mu.Unlock()
		if c != nil {
			c.Add(ctx, 1, metric.WithAttributes(attribute.String("level", r.Level.String())))
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h *errorCountHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &errorCountHandler{h.Handler.WithAttrs(attrs), h.tel, sync.Mutex{}, h.cnt}
}

func (h *errorCountHandler) WithGroup(name string) slog.Handler {
	return &errorCountHandler{h.Handler.WithGroup(name), h.tel, sync.Mutex{}, h.cnt}
}

// --- internal: logger provider ---

func newLoggerProvider(opts Options, res *sdkresource.Resource) (*sdklog.LoggerProvider, error) {
	exporter, err := otlploghttp.New(
		context.Background(),
		otlploghttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/logs")),
		otlploghttp.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, err
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(
			exporter,
			sdklog.WithExportInterval(1*time.Second),
			sdklog.WithExportMaxBatchSize(10),
		)),
	)
	return lp, nil
}

// --- internal: tracer provider ---

func newTracerProvider(opts Options, res *sdkresource.Resource) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/traces")),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)
	return tp, nil
}

// --- internal: meter provider ---

func newMeterProvider(opts Options, res *sdkresource.Resource) (*sdkmetric.MeterProvider, *prometheus.Registry, error) {
	exporter, err := otlpmetrichttp.New(
		context.Background(),
		otlpmetrichttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/metrics")),
	)
	if err != nil {
		return nil, nil, err
	}

	readerOpts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	}

	var promReg *prometheus.Registry
	if opts.PrometheusExporter {
		reg := prometheus.NewRegistry()
		promExp, err := otelprom.New(otelprom.WithRegisterer(reg))
		if err != nil {
			return nil, nil, err
		}
		readerOpts = append(readerOpts, sdkmetric.WithReader(promExp))
		promReg = reg
	}

	mp := sdkmetric.NewMeterProvider(readerOpts...)
	return mp, promReg, nil
}

// --- helpers ---

func deploymentEnv() string {
	return environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "ENVIRONMENT", "")
}

// Loading carrega o .env por padrão (dev ou HELLNET_ENVIRONMENT não definido) e
// valida as envs obrigatórias HELLNET_TELEMETRY_* (fail-fast via log.Fatalf).
// Em produção/staging explícitos, apenas retorna — a configuração é esperada no
// ambiente. Mantida por retrocompatibilidade: o mesmo comportamento já é feito
// dentro de New() (sem Options). Prefira New().
func Loading() {
	if err := loadEnv(); err != nil {
		log.Fatalf("%v", err)
	}
}

// loadEnv carrega o .env (exceto em Production/Staging explícitos) e valida as
// envs obrigatórias HELLNET_TELEMETRY_*, retornando erro em vez de matar o
// processo. Usada por New() (sem Options) e por Loading().
func loadEnv() error {
	env := deploymentEnv()
	// Produção/staging explícitos: não carrega .env local.
	if env == "Production" || env == "Staging" {
		return nil
	}

	if err := environments.LoadDotEnv("HELLNET_TELEMETRY_ENV_FILE", "HELLNET_ENV_FILE"); err != nil {
		return err
	}

	// Validação obrigatória (envs HELLNET_TELEMETRY_* / HELLNET_*).
	return (Options{}).Validate()
}

// exeDir retorna o diretório do executável (onde o binário compilado de main
// reside), para que o .env seja lido do mesmo path do entrypoint — não do
// diretório de onde o processo foi lançado (os.Getwd). Em `go run` o binário
// temporário vai para $TMPDIR, então faz fallback para os.Getwd() quando o
// .env não existir ao lado do executável, preservando o comportamento dev.
func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			return dir
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// buildVersion retorna a versão do módulo via build info (Go 1.18+).
// Útil em dashboards/Loki para correlacionar traces/metrics/logs à versão.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if info.Main.Version == "" {
		return "devel"
	}
	return info.Main.Version
}

var ErrMissingServiceName = &configError{"HELLNET_TELEMETRY_SERVICE (ou HELLNET_SERVICE) is required"}

type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// ───────────────────────── Abstração (Client) ─────────────────────────

// Logger é a abstração de logs (níveis padrão slog, sem Trace).
// Espelha a superfície do *slog.Logger para permitir DI/mock.
type Logger interface {
	Debug(msg string, args ...any)
	DebugContext(ctx context.Context, msg string, args ...any)
	Info(msg string, args ...any)
	InfoContext(ctx context.Context, msg string, args ...any)
	Warn(msg string, args ...any)
	WarnContext(ctx context.Context, msg string, args ...any)
	Error(msg string, args ...any)
	ErrorContext(ctx context.Context, msg string, args ...any)
	// With retorna um logger com atributos pré-populados (contextual).
	With(args ...any) Logger
}

// Meter expõe a superfície COMPLETA de metric.Meter (Float64*, Observable*,
// RegisterCallback, etc.) + atalhos agnósticos int64: Counter/Gauge/Histogram.
// Assim tel.Meter.Counter("x") evita Int64Counter, e tel.Meter.Float64Histogram(...)
// /Int64ObservableGauge/RegisterCallback continuam disponíveis.
type Meter interface {
	metric.Meter
	Counter(name string) (metric.Int64Counter, error)
	Gauge(name string) (metric.Int64Gauge, error)
	Histogram(name string) (metric.Int64Histogram, error)
}

type meterAdapter struct{ metric.Meter }

func (a meterAdapter) Counter(n string) (metric.Int64Counter, error) {
	return a.Meter.Int64Counter(n)
}

func (a meterAdapter) Gauge(n string) (metric.Int64Gauge, error) {
	return a.Meter.Int64Gauge(n)
}

func (a meterAdapter) Histogram(n string) (metric.Int64Histogram, error) {
	return a.Meter.Int64Histogram(n)
}

// Tracer abstrai a criação de spans (assinatura idêntica a trace.Tracer.Start).
type Tracer interface {
	Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span)
}

// Client é a abstração composta dos 3 sinais + lifecycle.
// Use para injeção de dependência e testes:
//
//	var c telemetry.Client = tel
//	c.Meter.Counter("req_total")        // int64 (atalho)
//	c.Meter.Float64Histogram("lat_s")   // float (superfície crua)
//	_, span := c.Trace().Start(ctx, "op"); defer span.End()
//	c.Log().ErrorContext(ctx, "boom", "err", err)
type Client interface {
	Log() Logger
	Trace() Tracer
	Metrics() Meter
	Shutdown() error
	WithSpan(ctx context.Context, name string, fn func(ctx context.Context) error) error
	Worker(ctx context.Context, job string, fn func(ctx context.Context) error, extra ...attribute.KeyValue) error
}

// ── implementação (non-breaking: *Telemetry satisfaz Client) ──

type slogLogger struct{ l *slog.Logger }

func (s slogLogger) base() *slog.Logger {
	if s.l == nil {
		return slog.Default()
	}
	return s.l
}

func (s slogLogger) Debug(msg string, a ...any) { s.base().Debug(msg, a...) }
func (s slogLogger) DebugContext(ctx context.Context, msg string, a ...any) {
	s.base().DebugContext(ctx, msg, a...)
}
func (s slogLogger) Info(msg string, a ...any) { s.base().Info(msg, a...) }
func (s slogLogger) InfoContext(ctx context.Context, msg string, a ...any) {
	s.base().InfoContext(ctx, msg, a...)
}
func (s slogLogger) Warn(msg string, a ...any) { s.base().Warn(msg, a...) }
func (s slogLogger) WarnContext(ctx context.Context, msg string, a ...any) {
	s.base().WarnContext(ctx, msg, a...)
}
func (s slogLogger) Error(msg string, a ...any) { s.base().Error(msg, a...) }
func (s slogLogger) ErrorContext(ctx context.Context, msg string, a ...any) {
	s.base().ErrorContext(ctx, msg, a...)
}

func (s slogLogger) With(a ...any) Logger {
	return slogLogger{s.base().With(a...)}
}

// Log retorna a abstração de logs. Nome evita colisão com o campo exportado Logger.
func (t *Telemetry) Log() Logger { return slogLogger{t.Logger} }

// Trace retorna a abstração de traces. Nome evita colisão com o campo exportado Tracer.
func (t *Telemetry) Trace() Tracer {
	if t.Tracer == nil {
		return otel.Tracer(t.serviceName)
	}
	return t.Tracer
}

// Metrics retorna a abstração de metrics (tel.Meter). Nome evita colisão com o campo Meter.
func (t *Telemetry) Metrics() Meter { return t.Meter }

// compile-time: *Telemetry satisfaz Client.
var _ Client = (*Telemetry)(nil)
