package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ───────────────────────────── SLO ─────────────────────────────

// ErrInvalidTarget indica target de SLO fora de (0,1].
var ErrInvalidTarget = errors.New("telemetry: target de SLO deve estar em (0,1]")

// ErrNoEvents indica totalEvents <= 0 (janela sem dados).
var ErrNoEvents = errors.New("telemetry: totalEvents deve ser > 0")

// ErrBadCounts indica goodEvents inconsistente com totalEvents.
var ErrBadCounts = errors.New("telemetry: goodEvents deve estar em [0,totalEvents]")

// ErrorBudget resume a conformidade de SLO de uma janela.
type ErrorBudget struct {
	// Target é a razão de sucesso alvo do SLO (ex.: 0.99 = 99%).
	Target float64
	// Observed é a razão de sucesso observada em [0,1].
	Observed float64
	// Remaining é Observed - Target (pode ser negativo quando estourado).
	Remaining float64
	// ConsumedPct é o % do error budget consumido: (1-Observed)/(1-Target)*100.
	ConsumedPct float64
	// Breached é true quando Observed < Target (gatilho de alerta SRE).
	Breached bool
}

// ComputeErrorBudget calcula o error budget dado um SLO alvo (razão de sucesso
// em (0,1]) e as contagens de eventos de uma janela. É pura e determinística
// (sem I/O), coberta por TDD e usada em alertas SRE (burn-rate/breach).
//
// Regras de erro:
//   - target <= 0 ou target > 1 → ErrInvalidTarget
//   - totalEvents <= 0          → ErrNoEvents
//   - goodEvents < 0 ou > total → ErrBadCounts
func ComputeErrorBudget(target float64, goodEvents, totalEvents int) (ErrorBudget, error) {
	eb := ErrorBudget{Target: target}
	if target <= 0 || target > 1 {
		return eb, ErrInvalidTarget
	}
	if totalEvents <= 0 {
		return eb, ErrNoEvents
	}
	if goodEvents < 0 || goodEvents > totalEvents {
		return eb, ErrBadCounts
	}

	observed := float64(goodEvents) / float64(totalEvents)
	eb.Observed = observed
	eb.Breached = observed < target

	// Orçamento de erro permitido (razão de eventos ruins): 1 - target.
	allowedBad := 1 - target
	if allowedBad <= 0 {
		// target == 1: tolerância zero.
		eb.Remaining = 0
		if goodEvents < totalEvents {
			eb.ConsumedPct = 100
		}
		return eb, nil
	}

	badRatio := 1 - observed
	eb.ConsumedPct = badRatio / allowedBad * 100
	eb.Remaining = allowedBad - badRatio // == observed - target
	return eb, nil
}

// ──────────────────────────── Health ────────────────────────────

// HealthStatus represents the health check result.
type HealthStatus struct {
	Status string        `json:"status"`
	Checks []CheckResult `json:"checks,omitempty"`
}

// CheckResult holds an individual health check outcome.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Live returns a liveness handler (self-check only).
func (t *Telemetry) Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, http.StatusOK, HealthStatus{Status: "live"})
	})
}

// Ready returns a readiness handler (self + OTLP collector + custom checks).
func (t *Telemetry) Ready() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks, allPass := t.runChecks(r.Context())
		code := http.StatusOK
		status := "ready"
		if !allPass {
			code = http.StatusServiceUnavailable
			status = "not ready"
		}
		writeHealth(w, code, HealthStatus{Status: status, Checks: checks})
	})
}

// Health returns a combined health handler.
func (t *Telemetry) Health() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks, allPass := t.runChecks(r.Context())
		status := "ok"
		code := http.StatusOK
		if !allPass {
			status = "degraded"
			code = http.StatusServiceUnavailable
		}
		writeHealth(w, code, HealthStatus{Status: status, Checks: checks})
	})
}

// healthCheckDurationBoundaries (segundos) para o histograma de duração de checks.
var healthCheckDurationBoundaries = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// recordHealthCheck executa fn, mede a duração e registra as métricas análogas ao
// prometheus-net.AspNetCore.HealthChecks:
//   - healthcheck_status{name}      gauge 0/1 (pass=1, fail=0)
//   - healthcheck_duration_seconds{name}  histograma da duração do check
//
// Retorna o status ("pass"/"fail") e o erro de fn (repassado para o caller).
func (t *Telemetry) recordHealthCheck(ctx context.Context, name string, fn func(ctx context.Context) error) (string, error) {
	start := time.Now()
	err := fn(ctx)
	dur := time.Since(start)

	status := "pass"
	val := int64(1)
	if err != nil {
		status = "fail"
		val = 0
	}

	// t.Meter pode ser nil quando o Telemetry é construído manualmente sem New
	// (ex.: testes de health que só checam JSON). Nesse caso, pula as métricas.
	if t.Meter != nil {
		// healthcheck_status é um Int64ObservableGauge (callback) que lê este
		// mapa no scrape/export — exportação confiável em OTLP e Prometheus.
		if t.healthStatus != nil {
			t.healthStatusMu.Lock()
			t.healthStatus[name] = val
			t.healthStatusMu.Unlock()
		}
		if h, herr := t.Meter.Float64Histogram("healthcheck_duration_seconds",
			metric.WithExplicitBucketBoundaries(healthCheckDurationBoundaries...),
			metric.WithUnit("s"),
			metric.WithDescription("Duração de cada health check"),
		); herr == nil {
			h.Record(ctx, dur.Seconds(), metric.WithAttributes(attribute.String("name", name)))
		}
	}
	return status, err
}

// registerHealthMetrics cria o Int64ObservableGauge healthcheck_status, que
// expõe o status (1=pass, 0=fail) de cada check a partir do mapa healthStatus
// no instante do scrape/export. Isso garante que a métrica apareça em OTLP e
// /metrics — o exporter Prometheus (otelprom) não exporta Int64Gauge síncrono
// com atributo de forma consistente, ao contrário dos observables (ex.: process_*).
func (t *Telemetry) registerHealthMetrics() {
	if t.Meter == nil {
		return
	}
	t.healthStatus = map[string]int64{}
	g, err := t.Meter.Int64ObservableGauge("healthcheck_status",
		metric.WithDescription("Health check status (1=pass, 0=fail) por check"),
	)
	if err != nil {
		return
	}
	_, _ = t.Meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		t.healthStatusMu.Lock()
		defer t.healthStatusMu.Unlock()
		for name, v := range t.healthStatus {
			o.ObserveInt64(g, v, metric.WithAttributes(attribute.String("name", name)))
		}
		return nil
	}, g)
}

// runChecks executa self + OTLP collector + health checks customizados registrados,
// registrando também as métricas de health check (ver recordHealthCheck).
func (t *Telemetry) runChecks(ctx context.Context) ([]CheckResult, bool) {
	allPass := true
	checks := make([]CheckResult, 0, 2+len(t.healthChecksSnapshot()))

	selfStatus, _ := t.recordHealthCheck(ctx, "self", func(context.Context) error { return nil })
	checks = append(checks, CheckResult{Name: "self", Status: selfStatus})

	if t.otlpEndpoint != "" {
		st, err := t.recordHealthCheck(ctx, "otlp-collector", func(c context.Context) error {
			return checkOTLPReachable(c, t.otlpEndpoint)
		})
		if err != nil {
			allPass = false
		}
		cr := CheckResult{Name: "otlp-collector", Status: st}
		if err != nil {
			cr.Error = err.Error()
		}
		checks = append(checks, cr)
	}

	snapshot := t.healthChecksSnapshot()
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		st, err := t.recordHealthCheck(ctx, name, snapshot[name])
		cr := CheckResult{Name: name, Status: st}
		if err != nil {
			allPass = false
			cr.Error = err.Error()
		}
		checks = append(checks, cr)
	}

	// healthcheck_all_pass: gauge 0/1 do estado agregado (paridade ao total do prometheus-net).
	if t.Meter != nil {
		if g, gerr := t.Meter.Gauge("healthcheck_all_pass"); gerr == nil {
			v := int64(1)
			if !allPass {
				v = 0
			}
			g.Record(ctx, v)
		}
	}

	return checks, allPass
}

// healthChecksSnapshot retorna cópia do registry sob RLock (safe para concorrência).
func (t *Telemetry) healthChecksSnapshot() map[string]func(ctx context.Context) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cp := make(map[string]func(ctx context.Context) error, len(t.healthChecks))
	for k, v := range t.healthChecks {
		cp[k] = v
	}
	return cp
}

// healthDialTimeout is the TCP dial timeout for the OTLP collector reachability check.
const healthDialTimeout = 2 * time.Second

// checkOTLPReachable verifies the OTLP collector is reachable via TCP.
// The port comes from the endpoint itself (or scheme default); the library
// does not read a separate port env var.
func checkOTLPReachable(ctx context.Context, endpoint string) error {
	host, port, err := parseOTLPEndpoint(endpoint, "")
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, healthDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// parseOTLPEndpoint extracts host/port from the OTLP endpoint. The port comes
// from the endpoint itself; if absent, the scheme default is inferred (443 for
// https, 80 for http) so the health check can dial. There is no separate port
// env var — the library never reads HELLNET_*_PORT. Pure and testable.
func parseOTLPEndpoint(endpoint, _ string) (host, port string, err error) {
	if u, perr := url.Parse(endpoint); perr == nil && u.Host != "" {
		port = u.Port()
		if port == "" {
			switch u.Scheme {
			case "https":
				port = "443"
			case "http":
				port = "80"
			}
		}
		return u.Hostname(), port, nil
	}
	// Fallback: "host:port" or bare "host"
	if h, p, perr := net.SplitHostPort(endpoint); perr == nil {
		return h, p, nil
	}
	return endpoint, "", nil
}

func writeHealth(w http.ResponseWriter, code int, status HealthStatus) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(status)
}

// ─────────────────────────── Middleware ──────────────────────────

// Middleware wraps an http.Handler with OpenTelemetry tracing + request metrics.
//
// A extração de contexto de trace dos requests inbound (via otelhttp) permanece
// request-scoped por design: é correlação server-side, não "app-passing" — o
// contexto da aplicação (baseCtx de New) não participa deste fluxo. Os logs do
// middleware usam o ctx enriquecido com o span do request para correlação
// trace→log nos sinks (stdout + OTLP).
func Middleware(tel *Telemetry, next http.Handler) http.Handler {
	opts := []otelhttp.Option{}
	if tel.tp != nil {
		opts = append(opts, otelhttp.WithTracerProvider(tel.tp))
	}

	// Instrumentações de request criados uma única vez.
	reqCount, _ := tel.Meter.Counter("http_requests_total")
	reqDuration, _ := tel.Meter.Float64Histogram("http_request_duration_seconds",
		metric.WithExplicitBucketBoundaries(latencyBucketBoundaries...),
		metric.WithUnit("s"),
		metric.WithDescription("Duração de requests HTTP em segundos"),
	)
	// UpDownCounter é o instrumento correto para concorrência (inflight).
	reqInflight, _ := tel.Meter.Int64UpDownCounter("http_requests_inflight")
	respSize, _ := tel.Meter.Float64Histogram("http_response_size_bytes",
		metric.WithExplicitBucketBoundaries(httpResponseSizeBoundaries...),
		metric.WithUnit("By"),
		metric.WithDescription("Tamanho da resposta HTTP em bytes"),
	)
	// Tamanho do corpo da requisição (via ContentLength; não consome o body).
	reqBodySize, _ := tel.Meter.Float64Histogram("http_requests_body_size_bytes",
		metric.WithExplicitBucketBoundaries(requestBodySizeBoundaries...),
		metric.WithUnit("By"),
		metric.WithDescription("Tamanho do corpo da requisição HTTP em bytes"),
	)
	// Erros HTTP (status >= 400) — sinal de taxa de erro automática no server.
	reqErrors, _ := tel.Meter.Counter("http_server_errors_total")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if reqInflight != nil {
			reqInflight.Add(r.Context(), 1, metric.WithAttributes(attribute.String("method", r.Method)))
			defer reqInflight.Add(r.Context(), -1, metric.WithAttributes(attribute.String("method", r.Method)))
		}

		// Wrap response writer to capture status code and response size.
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Captura o context enriquecido (com span) que o otelhttp injeta
		// no handler interno, para correlacionar logs com trace_id.
		var enriched context.Context
		inner := http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
			enriched = r2.Context()
			next.ServeHTTP(w2, r2)
		})

		handler := otelhttp.NewHandler(inner, tel.serviceName, opts...)
		handler.ServeHTTP(lrw, r)

		if enriched == nil {
			enriched = r.Context()
		}

		status := lrw.statusCode
		dur := time.Since(start)

		// Log de request correlacionado ao span do request (nil-safe:
		// logIn usa slog.Default se Logger nil)
		tel.logIn(enriched, slog.LevelInfo, "request completed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("host", r.Host),
			slog.Int("status", status),
			slog.Duration("duration", dur),
		)

		if reqCount != nil {
			reqCount.Add(enriched, 1, metric.WithAttributes(
				attribute.String("method", r.Method),
				attribute.Int("status", status),
			))
		}
		if reqDuration != nil {
			reqDuration.Record(enriched, dur.Seconds(), metric.WithAttributes(
				attribute.String("method", r.Method),
				attribute.Int("status", status),
			))
		}
		if respSize != nil {
			respSize.Record(r.Context(), float64(lrw.size), metric.WithAttributes(
				attribute.String("method", r.Method),
				attribute.Int("status", status),
			))
		}
		if reqBodySize != nil && r.ContentLength >= 0 {
			reqBodySize.Record(r.Context(), float64(r.ContentLength), metric.WithAttributes(
				attribute.String("method", r.Method),
			))
		}
		if reqErrors != nil && status >= 400 {
			reqErrors.Add(enriched, 1, metric.WithAttributes(attribute.String("method", r.Method)))
		}
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	size       int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *loggingResponseWriter) Write(b []byte) (int, error) {
	n, err := lrw.ResponseWriter.Write(b)
	lrw.size += n
	return n, err
}

// httpResponseSizeBoundaries (bytes) para o histograma de tamanho de resposta.
var httpResponseSizeBoundaries = []float64{
	100, 500, 1000, 5000, 10000, 50000, 100000, 500000, 1000000, 5000000,
}

// requestBodySizeBoundaries (bytes) para o histograma de tamanho de requisição.
var requestBodySizeBoundaries = []float64{
	64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216,
}

// ───────────────────────────── Worker ────────────────────────────

// latencyBucketBoundaries são buckets explícitos (segundos) ajustados para
// latência de requests/jobs (de ms a minutos), garantindo precisão de
// p99/p95 no Prometheus. Compartilhado entre Middleware e Worker.
var latencyBucketBoundaries = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// Worker executa fn como uma unidade de trabalho observada automaticamente.
// É o equivalente "sem HTTP" do Middleware: qualquer worker, consumer de fila,
// job agendado (cron) ou task em background ganha observabilidade sem escrever
// boilerplate.
//
// O contexto é passado UMA vez em New (baseCtx): o span do job deriva dele
// (raiz(baseCtx) → filhos) e o ctx derivado é repassado a fn para continuação
// da linhagem por código otel-instrumentado mais a fundo.
//
// Para cada execução o Worker produz, transparente para o caller:
//   - span de trace (nome = job), com status de erro quando fn falha;
//   - contador   worker_jobs_total{job,status[,extra]}   — total de execuções;
//   - histograma worker_job_duration_seconds{job,status} — habilita p99/p95/p50
//     via histogram_quantile no Prometheus/Grafana;
//   - gauge      worker_jobs_inflight{job[,extra]}       — concorrência ativa;
//   - log estruturado de início/fim com duração e erro (Error em falha),
//     correlacionado ao span do job.
//
// Em panic: exceptions_total é incrementada, span finalizado e o panic
// re-propagado (paridade com WithSpan; métricas/log pós-execução não são
// emitidos). O erro de fn é repassado (não tratado), então o caller decide
// retry/backoff. extra permite atributos adicionais (fila, partition, tenant).
func (t *Telemetry) Worker(job string, fn func(ctx context.Context) error, extra ...attribute.KeyValue) error {
	jobsTotal, _ := t.Meter.Counter("worker_jobs_total")
	jobDur, _ := t.Meter.Float64Histogram("worker_job_duration_seconds",
		metric.WithExplicitBucketBoundaries(latencyBucketBoundaries...),
		metric.WithUnit("s"),
		metric.WithDescription("Duração de execução de jobs/workers em segundos"),
	)
	// UpDownCounter é o instrumento correto para concorrência (inflight):
	// permite Add/Sub de delta, diferente de Gauge (Record de valor absoluto).
	inflight, _ := t.Meter.Int64UpDownCounter("worker_jobs_inflight")

	baseAttrs := append([]attribute.KeyValue{attribute.String("job", job)}, extra...)
	withStatus := func(status string) []attribute.KeyValue {
		return append(append([]attribute.KeyValue{}, baseAttrs...), attribute.String("status", status))
	}

	if inflight != nil {
		inflight.Add(t.baseContext(), 1, metric.WithAttributes(baseAttrs...))
		defer inflight.Add(t.baseContext(), -1, metric.WithAttributes(baseAttrs...))
	}

	start := time.Now()
	spanCtx, err := t.runBaseSpan(job, func(ctx context.Context) error {
		return fn(ctx)
	})
	dur := time.Since(start)

	status := "ok"
	if err != nil {
		status = "error"
	}

	if jobsTotal != nil {
		jobsTotal.Add(spanCtx, 1, metric.WithAttributes(withStatus(status)...))
	}
	if jobDur != nil {
		jobDur.Record(spanCtx, dur.Seconds(), metric.WithAttributes(withStatus(status)...))
	}

	if err != nil {
		t.logIn(spanCtx, slog.LevelError, "worker job failed",
			slog.String("job", job),
			slog.Duration("duration", dur),
			slog.String("error", err.Error()),
		)
		return err
	}

	t.logIn(spanCtx, slog.LevelInfo, "worker job completed",
		slog.String("job", job),
		slog.Duration("duration", dur),
	)
	return nil
}

// ───────────── Process metrics helpers (runtime-guarded) ─────────────
// readOpenFds e readThreads leem /proc (Linux). Em outras plataformas retornam
// erro e a métrica relacionada simplesmente não é emitida — sem build tags
// separados, mantendo o número de arquivos baixo.

func readOpenFds() (int64, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, err
	}
	return int64(len(entries)), nil
}

func readThreads() (int64, error) {
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
	if len(fields) < 18 { // num_threads é o 18º campo após o comm
		return 0, os.ErrInvalid
	}
	n, err := strconv.ParseInt(fields[17], 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ─────────────────── HTTP client (outbound) ───────────────────
//
// O lado de SAÍDA movou-se para httpclient.go: a factory tel.HTTPClient(opts...)
// devolve um client com tracing W3C, retry/backoff idempotente e métricas
// por tentativa (família http_client_*).

// ─────────────────── SQL / DB (connection pool) ───────────────────

// WatchDB regista automaticamente métricas do pool de conexões de db
// (database/sql), particionadas por name (ex.: "main", "replica"). Usa um
// callback assíncrono que lê db.Stats() a cada collect — sem goroutines
// próprias, acompanhando o ciclo do OTLP/Prometheus. Paridade com as métricas
// de SqlClient do prometheus-net (ao nível do pool).
func (t *Telemetry) WatchDB(db *sql.DB, name string) {
	m := t.Meter
	openConns, _ := m.Int64ObservableGauge("db_sql_open_connections",
		metric.WithDescription("Number of open connections in the pool"))
	inUse, _ := m.Int64ObservableGauge("db_sql_in_use_connections",
		metric.WithDescription("Connections currently in use"))
	idle, _ := m.Int64ObservableGauge("db_sql_idle_connections",
		metric.WithDescription("Idle connections in the pool"))
	maxOpen, _ := m.Int64ObservableGauge("db_sql_max_open_connections",
		metric.WithDescription("Maximum open connections"))
	waitCount, _ := m.Int64ObservableCounter("db_sql_wait_count_total",
		metric.WithDescription("Total number of connections waited for"))
	waitDur, _ := m.Float64ObservableCounter("db_sql_wait_duration_seconds_total",
		metric.WithDescription("Total time blocked waiting for a new connection"))
	closedLifetime, _ := m.Int64ObservableCounter("db_sql_closed_max_lifetime_total",
		metric.WithDescription("Connections closed due to max lifetime"))
	closedIdle, _ := m.Int64ObservableCounter("db_sql_closed_max_idle_total",
		metric.WithDescription("Connections closed due to max idle time"))

	_, _ = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := db.Stats()
		attrs := metric.WithAttributes(attribute.String("db", name))
		o.ObserveInt64(openConns, int64(st.OpenConnections), attrs)
		o.ObserveInt64(inUse, int64(st.InUse), attrs)
		o.ObserveInt64(idle, int64(st.Idle), attrs)
		o.ObserveInt64(maxOpen, int64(st.MaxOpenConnections), attrs)
		o.ObserveInt64(waitCount, st.WaitCount, attrs)
		o.ObserveFloat64(waitDur, st.WaitDuration.Seconds(), attrs)
		o.ObserveInt64(closedLifetime, st.MaxLifetimeClosed, attrs)
		o.ObserveInt64(closedIdle, st.MaxIdleClosed, attrs)
		return nil
	}, openConns, inUse, idle, maxOpen, waitCount, waitDur, closedLifetime, closedIdle)
}
