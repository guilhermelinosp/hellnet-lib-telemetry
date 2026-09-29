package telemetry

import (
	"context"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// gcPauseBoundaries são buckets explícitos (segundos) para pausas de GC
// (tipicamente µs a dezenas de ms), habilitando p99 da pausa de GC.
var gcPauseBoundaries = []float64{
	1e-5, 5e-5, 1e-4, 5e-4, 1e-3, 5e-3, 1e-2, 5e-2, 1e-1, 5e-1, 1,
}

type ContextMeter struct {
	tel *Telemetry
	ctx context.Context
}

func (m ContextMeter) Counter(name string, value int64) error {
	return m.tel.Counter(m.ctx, name, value)
}

func (m ContextMeter) Gauge(name string, value int64) error {
	return m.tel.Gauge(m.ctx, name, value)
}

func (m ContextMeter) Histogram(name string, value float64) error {
	return m.tel.Duration(m.ctx, name, value)
}

func (t *Telemetry) Metric(ctx context.Context) ContextMeter {
	return ContextMeter{tel: t, ctx: ctx}
}

// buildMeter monta o MeterProvider OTLP, as runtime metrics e as métricas de
// health check. Não há endpoint local de exposição de métricas.
func (t *Telemetry) buildMeter(ctx context.Context, o Options, res *sdkresource.Resource) error {
	mp, err := newMeterProvider(ctx, o, res)
	if err != nil {
		return err
	}
	t.mp = mp
	t.meter = mp.Meter(o.ServiceName)
	otel.SetMeterProvider(mp)
	t.startRuntimeMetrics()
	t.registerHealthMetrics()
	return nil
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
	m := t.meter

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

// newMeterProvider cria o MeterProvider SDK com exportação exclusivamente OTLP.
func newMeterProvider(ctx context.Context, opts Options, res *sdkresource.Resource) (*sdkmetric.MeterProvider, error) {
	readerOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	// Endpoint vazio → sem reader OTLP; métricas ficam apenas no SDK local.
	if opts.OTLPEndpoint != "" {
		exporterOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/metrics"))}
		if len(opts.OTLPHeaders) > 0 {
			exporterOpts = append(exporterOpts, otlpmetrichttp.WithHeaders(opts.OTLPHeaders))
		}
		exporter, err := otlpmetrichttp.New(ctx, exporterOpts...)
		if err != nil {
			return nil, err
		}
		readerOpts = append(readerOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	}
	mp := sdkmetric.NewMeterProvider(readerOpts...)
	return mp, nil
}
