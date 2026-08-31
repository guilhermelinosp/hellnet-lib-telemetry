package telemetry

import (
	"context"
	"math"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

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

// gcPauseBoundaries são buckets explícitos (segundos) para pausas de GC
// (tipicamente µs a dezenas de ms), habilitando p99 da pausa de GC.
var gcPauseBoundaries = []float64{
	1e-5, 5e-5, 1e-4, 5e-4, 1e-3, 5e-3, 1e-2, 5e-2, 1e-1, 5e-1, 1,
}

// meterAdapter adapta metric.Meter para expor atalhos agnósticos int64.
type meterAdapter struct{ metric.Meter }

func (a meterAdapter) Counter(n string) (metric.Int64Counter, error) {
	return a.Int64Counter(n)
}

func (a meterAdapter) Gauge(n string) (metric.Int64Gauge, error) {
	return a.Int64Gauge(n)
}

func (a meterAdapter) Histogram(n string) (metric.Int64Histogram, error) {
	return a.Int64Histogram(n)
}

// Metric retorna a abstração de metrics (tel.Meter). Nome evita colisão com o campo Meter.
func (t *Telemetry) Metric() Meter { return t.Meter }

// buildMeter monta o MeterProvider (OTLP + Prometheus), os runtime metrics e
// as métricas de health check. Runtime metrics sempre ligadas (prometheus-net).
func (t *Telemetry) buildMeter(o Options, res *sdkresource.Resource) error {
	mp, promReg, err := newMeterProvider(o, res)
	if err != nil {
		return err
	}
	t.mp = mp
	t.promRegistry = promReg
	t.Meter = meterAdapter{mp.Meter(o.ServiceName)}
	otel.SetMeterProvider(mp)
	t.startRuntimeMetrics()
	t.registerHealthMetrics()
	return nil
}

// MetricsHandler returns an http.Handler that serves the library's metrics in
// Prometheus exposition format (text/plain), for scraping via a /metrics
// endpoint. O exporter Prometheus vem sempre ligado por padrão. Permite
// inspecionar as métricas (p99 de latência/worker, CPU, GC, health checks,
// etc.) sem um collector OTLP — ideal durante testes locais.
//
// Exemplo:
//
//	mux.Handle("GET /metrics", tel.MetricsHandler())
//
// Nota: não pode chamar-se Metric() pois o acessor do meter (Client.Metric()
// Meter) já ocupa esse nome no conjunto de métodos do Telemetry.
func (t *Telemetry) MetricsHandler() http.Handler {
	if t.promRegistry != nil {
		return promhttp.HandlerFor(t.promRegistry, promhttp.HandlerOpts{})
	}
	// Fallback defensivo: /metrics vazio.
	return promhttp.Handler()
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
		"process_gc_pause_seconds", metric.WithExplicitBucketBoundaries(gcPauseBoundaries...), metric.WithDescription("Distribution of individual GC pause durations"),
	)

	start := time.Now()
	var (
		mu         sync.Mutex
		lastNumGC  uint32
		prevCPUNs  int64
		prevWallNs int64
		cpuInit    bool
	)

	// memI64 converts a runtime.MemStats counter (uint64) for OTel observation,
	// clamping defensively to satisfy gosec G115 — in practice runtime memory
	// counters never approach MaxInt64 (~9.2 EiB).
	memI64 := func(v uint64) int64 {
		if v > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(v)
	}

	_, _ = m.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)

			o.ObserveInt64(goroutines, int64(runtime.NumGoroutine()))
			o.ObserveInt64(heapAlloc, memI64(ms.Alloc))
			o.ObserveInt64(heapSys, memI64(ms.HeapSys))
			o.ObserveInt64(heapInuse, memI64(ms.HeapInuse))
			o.ObserveInt64(heapReleased, memI64(ms.HeapReleased))
			o.ObserveInt64(heapObjects, memI64(ms.HeapObjects))
			o.ObserveInt64(stackInuse, memI64(ms.StackInuse))
			o.ObserveInt64(stackSys, memI64(ms.StackSys))
			o.ObserveInt64(mspanInuse, memI64(ms.MSpanInuse))
			o.ObserveInt64(mspanSys, memI64(ms.MSpanSys))
			o.ObserveInt64(mcacheInuse, memI64(ms.MCacheInuse))
			o.ObserveInt64(mcacheSys, memI64(ms.MCacheSys))
			o.ObserveInt64(otherSys, memI64(ms.OtherSys))
			o.ObserveInt64(gcSys, memI64(ms.GCSys))
			o.ObserveInt64(sysTotal, memI64(ms.Sys))
			o.ObserveInt64(totalAlloc, memI64(ms.TotalAlloc))
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
// erro e a métrica de CPU (process_cpu_usage_*) simplesmente não é emitida.
// utime/stime vêm em clock ticks; converte-se assumindo USER_HZ=100 (padrão da
// maioria dos kernels Linux).
func readProcessCPUNs() (int64, error) {
	fields, err := procStatFields()
	if err != nil {
		return 0, err
	}
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

// newMeterProvider cria o MeterProvider SDK com OTLP + Prometheus.
func newMeterProvider(opts Options, res *sdkresource.Resource) (*sdkmetric.MeterProvider, *prometheus.Registry, error) {
	readerOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	// Endpoint vazio → sem reader OTLP; métricas exportadas só via Prometheus.
	if opts.OTLPEndpoint != "" {
		exporter, err := otlpmetrichttp.New(
			context.Background(), otlpmetrichttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/metrics")),
		)
		if err != nil {
			return nil, nil, err
		}
		readerOpts = append(readerOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	}

	reg := prometheus.NewRegistry()
	promExp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, nil, err
	}
	readerOpts = append(readerOpts, sdkmetric.WithReader(promExp))
	promReg := reg

	mp := sdkmetric.NewMeterProvider(readerOpts...)
	return mp, promReg, nil
}
