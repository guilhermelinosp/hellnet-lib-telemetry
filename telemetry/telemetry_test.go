package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type testContextKey struct{}

func TestOTLPLogBodyIsJSON(t *testing.T) {
	entry := zapcore.Entry{Level: zap.InfoLevel, Time: time.Date(2026, 9, 28, 22, 0, 0, 0, time.UTC), Message: "example log"}
	body := Body(entry, map[string]interface{}{"key": "value", "count": int64(2)})
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if decoded["msg"] != "example log" || decoded["level"] != "INFO" || decoded["key"] != "value" {
		t.Fatalf("unexpected body: %s", body)
	}
}

// newTestTel constrói um Telemetry sem collector real (endpoint vazio) para os
// testes unitários não tentarem conexões de rede.
func newTestTel(t *testing.T) *Telemetry {
	t.Helper()
	t.Setenv("HELLNET_TELEMETRY_SERVICE", "telemetry-test")
	t.Setenv("HELLNET_TELEMETRY_SERVICE_VERSION", "test")
	t.Setenv("HELLNET_TELEMETRY_ENDPOINT", "")
	t.Setenv("HELLNET_ENDPOINT", "")
	tel, err := New(context.Background())
	if err != nil {
		t.Fatalf("New() retornou erro: %v", err)
	}
	t.Cleanup(func() { _ = tel.Close(context.Background()) })
	return tel
}

func TestNewAndClose(t *testing.T) {
	tel := newTestTel(t)
	if tel == nil {
		t.Fatal("New() retornou nil")
	}
	if tel.meter == nil {
		t.Fatal("Meter não deve ser nil")
	}
	if tel.logger == nil {
		t.Fatal("Logger não deve ser nil")
	}
	if err := tel.Close(context.Background()); err != nil {
		t.Fatalf("Close() erro: %v", err)
	}
}

func TestInstrumentationLoggerUsesCallerSpanContext(t *testing.T) {
	tel, harness := newTelemetryWithHarness(t)
	ctx, span := tel.TracerProvider().Tracer("test").Start(context.Background(), "contract-span")
	tel.Logger("github.com/example/lib").Info(ctx, "hello", "token", "secret")
	span.End()

	records := harness.Logs()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	record := records[0]
	if !record.TraceID().IsValid() || !record.SpanID().IsValid() {
		t.Fatalf("native trace context missing: trace=%s span=%s", record.TraceID(), record.SpanID())
	}
	if got := record.InstrumentationScope().Name; got != "github.com/example/lib" {
		t.Fatalf("scope = %q", got)
	}
}

func TestContractLoggerSanitizesLineBreaks(t *testing.T) {
	if got := sanitizeLogMessage("before\r\nafter"); got != `before\r\nafter` {
		t.Fatalf("sanitized message = %q", got)
	}
}

func TestContractLoggerHonorsInfoLevelForOTLP(t *testing.T) {
	tel, harness := newTelemetryWithHarness(t)
	tel.logLevel = zap.InfoLevel
	tel.Logger("github.com/example/lib").Debug(context.Background(), "hidden")
	if got := len(harness.Logs()); got != 0 {
		t.Fatalf("debug records = %d, want 0", got)
	}
}

func TestContractLoggerUsesSanitizedMessageInStdout(t *testing.T) {
	tel, _ := newTelemetryWithHarness(t)
	core, logs := observer.New(zapcore.DebugLevel)
	tel.stdoutLogger = zap.New(core).Sugar()
	tel.Logger("github.com/example/lib").Info(context.Background(), "line\nbreak")
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("stdout entries = %d, want 1", len(entries))
	}
	if got := entries[0].Message; got != `line\nbreak` {
		t.Fatalf("stdout message = %q", got)
	}
}

func TestNoEndpointStillCorrelatesContractLogs(t *testing.T) {
	tel := newTestTel(t)
	core, logs := observer.New(zapcore.DebugLevel)
	tel.stdoutLogger = zap.New(core).Sugar()
	ctx, span := tel.TracerProvider().Tracer("test").Start(context.Background(), "without-endpoint")
	tel.Logger("github.com/example/lib").Info(ctx, "correlated")
	span.End()
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("stdout entries = %d, want 1", len(entries))
	}
	got, ok := entries[0].ContextMap()["trace_id"].(string)
	if !ok || got == "" {
		t.Fatalf("trace_id missing from local log: %#v", entries[0].ContextMap())
	}
}

func TestMustNew(t *testing.T) {
	t.Setenv("HELLNET_TELEMETRY_SERVICE", "telemetry-test")
	t.Setenv("HELLNET_TELEMETRY_SERVICE_VERSION", "test")
	t.Setenv("HELLNET_TELEMETRY_ENDPOINT", "")
	tel := MustNew(context.Background())
	if tel == nil {
		t.Fatal("MustNew() retornou nil")
	}
	_ = tel.Close(context.Background())
}

// NewWithOptions keeps configuration explicit and does not implicitly load a
// local .env file.
func TestNewWithOptionsUsesExplicitOptions(t *testing.T) {
	tel, err := NewWithOptions(context.Background(), Options{
		ServiceName:  "test-svc",
		OTLPEndpoint: "http://test-collector:4318",
	})
	if err != nil {
		t.Fatalf("NewWithOptions() erro: %v", err)
	}
	defer func() { _ = tel.Close(context.Background()) }()
	if tel.otlpEndpoint != "http://test-collector:4318" {
		t.Fatalf("endpoint = %q, want http://test-collector:4318", tel.otlpEndpoint)
	}
	if tel.serviceName != "test-svc" {
		t.Fatalf("serviceName = %q, want test-svc", tel.serviceName)
	}
}

func TestComputeErrorBudget(t *testing.T) {
	tests := []struct {
		name         string
		target       float64
		good         int
		total        int
		wantErr      error
		wantObs      float64
		wantRem      float64
		wantConsumed float64
		wantBreached bool
	}{
		{name: "100% success", target: 0.99, good: 100, total: 100, wantObs: 1, wantRem: 0.01, wantConsumed: 0, wantBreached: false},
		{name: "breached", target: 0.99, good: 95, total: 100, wantObs: 0.95, wantRem: -0.04, wantConsumed: 500, wantBreached: true},
		{name: "perfect target=1", target: 1.0, good: 100, total: 100, wantObs: 1, wantRem: 0, wantConsumed: 0, wantBreached: false},
		{name: "target=1 breached", target: 1.0, good: 99, total: 100, wantObs: 0.99, wantRem: 0, wantConsumed: 100, wantBreached: true},
		{name: "zero target", target: 0, good: 100, total: 100, wantErr: ErrInvalidTarget},
		{name: "above 1 target", target: 1.5, good: 100, total: 100, wantErr: ErrInvalidTarget},
		{name: "zero total", target: 0.99, good: 100, total: 0, wantErr: ErrNoEvents},
		{name: "negative good", target: 0.99, good: -1, total: 100, wantErr: ErrBadCounts},
		{name: "good > total", target: 0.99, good: 101, total: 100, wantErr: ErrBadCounts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eb, err := ComputeErrorBudget(tt.target, tt.good, tt.total)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("esperado erro %v, got nil", tt.wantErr)
				}
				if err != tt.wantErr {
					t.Fatalf("erro = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if !approxEqual(eb.Observed, tt.wantObs) {
				t.Errorf("Observed = %v, want %v", eb.Observed, tt.wantObs)
			}
			if !approxEqual(eb.Remaining, tt.wantRem) {
				t.Errorf("Remaining = %v, want %v", eb.Remaining, tt.wantRem)
			}
			if !approxEqual(eb.ConsumedPct, tt.wantConsumed) {
				t.Errorf("ConsumedPct = %v, want %v", eb.ConsumedPct, tt.wantConsumed)
			}
			if eb.Breached != tt.wantBreached {
				t.Errorf("Breached = %v, want %v", eb.Breached, tt.wantBreached)
			}
		})
	}
}

// approxEqual compara floats com tolerância de 1e-9 (evita falhas por
// arredondamento de ponto flutuante em ConsumedPct/Remaining).
func approxEqual(a, b float64) bool {
	const eps = 1e-9
	return (a-b) < eps && (b-a) < eps
}

func TestOtlpSignalURL(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		signal string
		want   string
	}{
		{name: "empty", base: "", signal: "/v1/traces", want: ""},
		{name: "no path", base: "http://alloy:4318", signal: "/v1/traces", want: "http://alloy:4318/v1/traces"},
		{name: "root path", base: "http://alloy:4318/", signal: "/v1/traces", want: "http://alloy:4318/v1/traces"},
		// Integração com o gateway do Alloy: endpoint raiz + /v1/ PathPrefix.
		{name: "alloy gateway", base: "https://alloy.hellnet.com.br", signal: "/v1/traces", want: "https://alloy.hellnet.com.br/v1/traces"},
		{name: "invalid url", base: "://bad", signal: "/v1/traces", want: "://bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := otlpSignalURL(tt.base, tt.signal); got != tt.want {
				t.Errorf("otlpSignalURL(%q,%q) = %q, want %q", tt.base, tt.signal, got, tt.want)
			}
		})
	}
}

func TestParseOTLPEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{name: "host:port", endpoint: "http://alloy:4318", wantHost: "alloy", wantPort: "4318"},
		{name: "https default", endpoint: "https://alloy.hellnet.com.br", wantHost: "alloy.hellnet.com.br", wantPort: "443"},
		{name: "http default", endpoint: "http://alloy:4317", wantHost: "alloy", wantPort: "4317"},
		{name: "bare host", endpoint: "tempo:4317", wantHost: "tempo", wantPort: "4317"},
		{name: "host only", endpoint: "localhost", wantHost: "localhost", wantPort: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, p, err := parseOTLPEndpoint(tt.endpoint, "")
			if tt.wantErr {
				if err == nil {
					t.Fatal("esperado erro")
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if h != tt.wantHost {
				t.Errorf("host = %q, want %q", h, tt.wantHost)
			}
			if p != tt.wantPort {
				t.Errorf("port = %q, want %q", p, tt.wantPort)
			}
		})
	}
}

func TestTraceAndLog(t *testing.T) {
	tel := newTestTel(t)
	var inner bool
	err := tel.Trace(context.Background()).Span("op", func(ctx context.Context) error {
		inner = true
		logger := tel.Log(ctx)
		logger.Trace("trace", "k", "v")
		logger.Debug("debug", "k", "v")
		logger.Info("info", "k", "v")
		logger.Warn("warn", "k", "v")
		logger.Error("error", "k", "v")
		logger.Critical("critical", "k", "v")
		return nil
	})
	if err != nil {
		t.Fatalf("Trace erro: %v", err)
	}
	if !inner {
		t.Fatal("fn não foi chamada")
	}
}

func TestInMemoryProvidersCaptureSignals(t *testing.T) {
	tel, harness := newTelemetryWithHarness(t)
	ctx := context.Background()

	if err := tel.Trace(ctx).Span("captured-span", func(ctx context.Context) error {
		tel.Logger("telemetry-test").Info(ctx, "captured-log", "key", "value")
		return tel.Metric(ctx).Counter("captured-counter", 1, attribute.String("kind", "test"))
	}); err != nil {
		t.Fatalf("Trace() erro: %v", err)
	}
	if err := tel.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush() erro: %v", err)
	}

	if got := len(harness.Spans()); got != 1 {
		t.Fatalf("spans = %d, want 1", got)
	}
	if len(harness.Logs()) != 1 {
		t.Fatalf("logs = %d, want 1", len(harness.Logs()))
	}
	data := harness.Metrics(ctx)
	if len(data.ScopeMetrics) == 0 || len(data.ScopeMetrics[0].Metrics) == 0 {
		t.Fatal("nenhuma métrica foi coletada")
	}
}

func TestCloseFlushesWithCanceledContext(t *testing.T) {
	tel, harness := newTelemetryWithHarness(t)
	if err := tel.Trace(context.Background()).Span("pending-span", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Trace() erro: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tel.Close(ctx); err != nil {
		t.Fatalf("Close() erro: %v", err)
	}
	if got := len(harness.Spans()); got != 1 {
		t.Fatalf("spans após Close com ctx cancelado = %d, want 1", got)
	}
}

func TestTracePreservesParent(t *testing.T) {
	tel := newTestTel(t)
	parent := context.WithValue(context.Background(), testContextKey{}, "parent")
	if err := tel.Trace(parent).Span("context-op", func(ctx context.Context) error {
		if got := ctx.Value(testContextKey{}); got != "parent" {
			t.Fatalf("parent context value = %v, want parent", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("Trace() erro: %v", err)
	}
}

func TestSensitiveFieldsAreRedacted(t *testing.T) {
	fields := redactFields([]zap.Field{zap.String("authorization", "Bearer secret"), zap.String("order_id", "123")})
	if fields[0].String != "[REDACTED]" {
		t.Fatalf("authorization was not redacted: %q", fields[0].String)
	}
	if fields[1].String != "123" {
		t.Fatalf("non-sensitive field changed: %q", fields[1].String)
	}
}

func TestReadyDoesNotDependOnCollector(t *testing.T) {
	tel := newTestTel(t)
	tel.otlpEndpoint = "127.0.0.1:1"
	ready, allPass := tel.runChecks(context.Background(), false)
	if !allPass {
		t.Fatal("readiness should pass without checking the collector")
	}
	for _, check := range ready {
		if check.Name == "otlp-collector" {
			t.Fatal("readiness must not include the OTLP collector")
		}
	}
	health, _ := tel.runChecks(context.Background(), true)
	foundCollector := false
	for _, check := range health {
		if check.Name == "otlp-collector" {
			foundCollector = true
		}
	}
	if !foundCollector {
		t.Fatal("health should include the OTLP collector")
	}
}

func TestTracePanic(t *testing.T) {
	tel := newTestTel(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("esperado panic re-propagado")
		}
	}()
	_ = tel.Trace(context.Background()).Span("op", func(ctx context.Context) error {
		panic("boom")
	})
}

func TestWorker(t *testing.T) {
	tel := newTestTel(t)
	called := false
	err := tel.WorkerContext(context.Background(), "job", func(ctx context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Worker erro: %v", err)
	}
	if !called {
		t.Fatal("worker fn não foi chamada")
	}
}

func TestWorkerError(t *testing.T) {
	tel := newTestTel(t)
	want := context.DeadlineExceeded
	err := tel.WorkerContext(context.Background(), "job", func(ctx context.Context) error {
		return want
	})
	if err != want {
		t.Fatalf("Worker erro = %v, want %v", err, want)
	}
}

func TestMiddleware(t *testing.T) {
	tel := newTestTel(t)
	h := Middleware(tel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/foo", nil)
	if err != nil {
		t.Fatalf("NewRequest erro: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET erro: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
}

func TestHealthEndpoints(t *testing.T) {
	tel := newTestTel(t)
	tests := []struct {
		name string
		h    http.Handler
		want int
	}{
		{name: "live", h: tel.Live(), want: http.StatusOK},
		{name: "ready", h: tel.Ready(), want: http.StatusOK},
		{name: "health", h: tel.Health(), want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), "GET", "/"+tt.name, nil)
			w := httptest.NewRecorder()
			tt.h.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Fatalf("code = %d, want %d", w.Code, tt.want)
			}
		})
	}

	tel.HealthRegister("failing", func(context.Context) error {
		return context.Canceled
	})
	for _, tt := range []struct {
		name string
		h    http.Handler
	}{
		{name: "ready", h: tel.Ready()},
		{name: "health", h: tel.Health()},
	} {
		t.Run("failing-"+tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "GET", "/"+tt.name, nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("code = %d, want %d", w.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

// TestAlloyIntegration valida o envio real de traces/metrics/logs para um
// collector Alloy. É pulado a menos que ALLOY_ENDPOINT esteja definido
// (ex.: http://alloy:4318 ou https://alloy.hellnet.com.br).
func TestAlloyIntegration(t *testing.T) {
	endpoint := os.Getenv("ALLOY_ENDPOINT")
	if endpoint == "" {
		t.Skip("defina ALLOY_ENDPOINT (ex.: http://alloy:4318) para rodar a integração real com o Alloy")
	}
	t.Setenv("HELLNET_TELEMETRY_ENDPOINT", endpoint)
	t.Setenv("HELLNET_TELEMETRY_SERVICE", "telemetry-test")
	tel := MustNew(context.Background())
	defer func() { _ = tel.Close(context.Background()) }()

	tel.Log(context.Background()).Info("integration test log", "ok", true)
	if err := tel.Trace(context.Background()).Span("integration-span", func(ctx context.Context) error {
		return tel.Metric(ctx).Counter("integration_test_total", 1)
	}); err != nil {
		t.Fatalf("Trace erro: %v", err)
	}

	// Tempo para os exporters batchearem e enviarem ao Alloy.
	time.Sleep(2 * time.Second)
}

func TestRetryDelayUsesFullJitter(t *testing.T) {
	for attempt, upper := range map[int]time.Duration{0: 100 * time.Millisecond, 1: 200 * time.Millisecond, 2: 400 * time.Millisecond} {
		for i := 0; i < 100; i++ {
			if got := retryDelay(100*time.Millisecond, attempt); got < 0 || got > upper {
				t.Fatalf("retryDelay(attempt=%d) = %s, want [0,%s]", attempt, got, upper)
			}
		}
	}
}

func TestRetryAfterDelay(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"3"}}}
	if got := retryAfterDelay(resp); got != 3*time.Second {
		t.Fatalf("retryAfterDelay() = %s, want 3s", got)
	}
	resp.Header.Set("Retry-After", "invalid")
	if got := retryAfterDelay(resp); got != 0 {
		t.Fatalf("invalid Retry-After = %s, want 0", got)
	}
}

type testSignalHarness struct {
	recorder *tracetest.SpanRecorder
	reader   *sdkmetric.ManualReader
	logsMu   sync.Mutex
	logs     []sdklog.Record
	logp     *sdklog.LoggerProvider
}

type testLogExporter struct {
	harness *testSignalHarness
}

func (e *testLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.harness.logsMu.Lock()
	defer e.harness.logsMu.Unlock()
	for _, record := range records {
		e.harness.logs = append(e.harness.logs, record.Clone())
	}
	return nil
}

func (*testLogExporter) Shutdown(context.Context) error   { return nil }
func (*testLogExporter) ForceFlush(context.Context) error { return nil }

func (h *testSignalHarness) Spans() []sdktrace.ReadOnlySpan { return h.recorder.Ended() }

func (h *testSignalHarness) Logs() []sdklog.Record {
	h.logsMu.Lock()
	defer h.logsMu.Unlock()
	return append([]sdklog.Record(nil), h.logs...)
}

func (h *testSignalHarness) Metrics(ctx context.Context) metricdata.ResourceMetrics {
	var metrics metricdata.ResourceMetrics
	if err := h.reader.Collect(ctx, &metrics); err != nil {
		panic(err)
	}
	return metrics
}

func newTelemetryWithHarness(t *testing.T) (*Telemetry, *testSignalHarness) {
	t.Helper()
	harness := &testSignalHarness{recorder: tracetest.NewSpanRecorder(), reader: sdkmetric.NewManualReader()}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(harness.recorder),
	)
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(harness.reader))
	harness.logp = sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(&testLogExporter{harness: harness})),
	)
	tel := &Telemetry{
		tracer:       tp.Tracer("telemetry-test"),
		meter:        mp.Meter("telemetry-test"),
		logLevel:     zapcore.DebugLevel,
		logger:       zap.NewNop().Sugar(),
		stdoutLogger: zap.NewNop().Sugar(),
		serviceName:  "telemetry-test",
		lp:           harness.logp,
		tp:           tp,
		mp:           mp,
		tpProvider:   tp,
		mpProvider:   mp,
	}
	t.Cleanup(func() { _ = tel.Close(context.Background()) })
	return tel, harness
}

func TestMiddlewareMetricsCarryTheRouteOnlyWhenARouterSetOne(t *testing.T) {
	tel, harness := newTelemetryWithHarness(t)
	inner := Middleware(tel, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("0123456789"))
	}))
	routed := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/orders/42", nil)
	routed.Pattern = "/orders/:id"
	inner.ServeHTTP(httptest.NewRecorder(), routed)
	inner.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/raw/777", nil))

	var routes []string
	for _, scope := range harness.Metrics(context.Background()).ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "http_requests_total" {
				continue
			}
			sum := m.Data.(metricdata.Sum[int64])
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value("http_route"); ok {
					routes = append(routes, v.AsString())
				} else {
					routes = append(routes, "")
				}
			}
		}
	}
	has := map[string]bool{}
	for _, r := range routes {
		has[r] = true
	}
	if len(routes) != 2 || !has["/orders/:id"] || !has[""] {
		t.Fatalf("http_route values = %q, want the router pattern for the routed request and none for the unrouted one (never the raw path)", routes)
	}
}
